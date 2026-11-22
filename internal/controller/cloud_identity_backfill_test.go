// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestPostgresLateCloudMatchCommitsExactlyOnce(t *testing.T) {
	dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL test DSN not set")
	}
	ctx := context.Background()
	// Store conformance tests truncate their own tables in parallel packages.
	// Keep the controller race on an independent schema in that same service.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	schema := fmt.Sprintf("cloud_match_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test URL")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now()
	pin := cloud.NodeIdentity{Node: "worker", NodeUID: "uid", InstanceID: "instance", ProviderID: "cloud:///instance", BootID: "boot"}
	inc := types.Incident{ID: "incident", Target: types.Target{Node: pin.Node}, RepairNodeUID: pin.NodeUID, State: types.StateEvaluating, OpenedAt: now, UpdatedAt: now, StateChangedAt: now}
	if err := st.CreateIncident(ctx, &inc); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		copy := *snapshot
		go func() { <-start; c := Controller{store: st}; results <- c.recordLateCloudIdentity(ctx, &copy, pin) }()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || got.CloudIdentity == nil || *got.CloudIdentity != pin || len(trail) != 1 || trail[0].Action != "cloud-instance-matched" {
		t.Fatalf("successes=%d conflicts=%d incident=%+v trail=%+v", successes, conflicts, got, trail)
	}
}

type lateMatchPlatform struct {
	autoCloudPlatform
	readErr error
	matches int
}

func (p *lateMatchPlatform) CloudRecyclingConfigured() bool { return true }
func (p *lateMatchPlatform) CloudNode(ctx context.Context, node string) (cloud.NodeRef, error) {
	if p.readErr != nil {
		return cloud.NodeRef{}, p.readErr
	}
	return p.autoCloudPlatform.CloudNode(ctx, node)
}
func (p *lateMatchPlatform) MatchCloudNodeUID(ctx context.Context, node, uid string) (cloud.NodeIdentity, error) {
	p.matches++
	ref, err := p.CloudNode(ctx, node)
	if err == nil && ref.Identity.NodeUID != uid {
		return cloud.NodeIdentity{}, cloud.ErrIdentityChanged
	}
	return ref.Identity, err
}

func TestLateCloudMatchResumesUnderOriginalPolicy(t *testing.T) {
	for _, mode := range []recoverymode.Mode{recoverymode.DryRun, recoverymode.Approval, recoverymode.Automatic} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			c, st, _ := autoFixture(t, mode, autoSettings(), 4)
			if mode == recoverymode.DryRun {
				cfg := *c.runtimeConfig(ctx)
				limits := *cfg.SafetyLimits
				limits.DryRun = false
				cfg.SafetyLimits = &limits
				if err := c.InstallRuntimeConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			p := &lateMatchPlatform{readErr: errors.New("node read failed")}
			c.platform = p
			n, err := st.GetNode(ctx, "n1")
			if err != nil {
				t.Fatal(err)
			}
			n.UID = "uid-n1"
			if err := st.UpsertNode(ctx, n); err != nil {
				t.Fatal(err)
			}
			id := openOn(t, c, "n1")
			held := step(t, c, st, id, 6)
			if held.CloudIdentity != nil || held.State != types.StateEvaluating || held.StepIndex != 0 || p.matches != 1 {
				t.Fatalf("held=%+v reads=%d", held, p.matches)
			}
			c.cloudAssessments[id].at = time.Now().Add(-time.Minute)
			p.readErr = nil
			got := step(t, c, st, id, 200)
			want := types.StateAwaitingApproval
			if mode == recoverymode.Automatic {
				want = types.StateResolved
			}
			if mode == recoverymode.DryRun && (!held.DryRun || !got.DryRun) {
				t.Fatal("late matching lost the policy's dry-run setting on a live installation")
			}
			if got.State != want || got.CloudIdentity == nil || p.matches != 2 {
				t.Fatalf("state=%s want=%s pin=%+v reads=%d", got.State, want, got.CloudIdentity, p.matches)
			}
		})
	}
}

func TestRecordedApprovalCannotAcquireLateCloudTarget(t *testing.T) {
	ctx := context.Background()
	c, st, _ := autoFixture(t, recoverymode.Approval, autoSettings(), 4)
	p := &lateMatchPlatform{}
	c.platform = p
	inc := &types.Incident{ID: "held", Target: types.Target{Node: "n1"}, State: types.StateAwaitingApproval, RepairNodeUID: "uid-n1", ApprovalEpoch: 1}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordApproval(ctx, &types.Approval{IncidentID: inc.ID, ParkEpoch: 1, At: time.Now(), StepName: "replace", Actor: "alice", Decision: types.ApprovalApproved}); err != nil {
		t.Fatal(err)
	}
	held, _, err := c.holdUnavailableCloudRecovery(ctx, inc, &playbook.Step{Action: "platform.replace_node"})
	if err != nil || !held || p.matches != 0 {
		t.Fatalf("held=%v reads=%d err=%v", held, p.matches, err)
	}
	got, err := st.GetIncident(ctx, inc.ID)
	if err != nil || got.CloudIdentity != nil {
		t.Fatalf("incident=%+v err=%v", got, err)
	}
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil || len(trail) != 1 || !strings.Contains(trail[0].Result, "kubeneuronctl resolve held --actor <who>") {
		t.Fatalf("trail=%+v err=%v", trail, err)
	}
}
