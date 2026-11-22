// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestCloudEligibilityDoesNotAuthorizeMissingOrForeignPin(t *testing.T) {
	c := &Controller{}
	inc := &types.Incident{Target: types.Target{Node: "worker"}}
	v, err := c.cloudRecoveryEligibility(context.Background(), inc)
	if err != nil || v.ReplaceAllowed || v.RecycleAllowed || v.Reason == "" {
		t.Fatalf("legacy incident: verdict=%+v err=%v", v, err)
	}
	inc.CloudIdentity = &cloud.NodeIdentity{Node: "other-worker", NodeUID: "uid", ProviderID: "provider", InstanceID: "instance", BootID: "boot"}
	v, err = c.cloudRecoveryEligibility(context.Background(), inc)
	if err == nil || v.ReplaceAllowed || v.RecycleAllowed {
		t.Fatalf("foreign pin: verdict=%+v err=%v", v, err)
	}
}

type namedRecyclePlatform struct {
	recyclePlatform
	name string
}

func (p *namedRecyclePlatform) CloudProviderName() string { return p.name }

func TestMissingCloudIdentityGuidance(t *testing.T) {
	for _, name := range []string{"", "aws", "azure", "gcp"} {
		p := &namedRecyclePlatform{name: name}
		p.configured = name != ""
		c := &Controller{platform: p}
		v, err := c.cloudRecoveryEligibility(context.Background(), &types.Incident{ID: "inc-held"})
		want := "no cloud provider is configured, so this node cannot be replaced — set spec.cloud.provider (see docs/install.md)"
		if name != "" {
			want = "KubeNeuron could not match this node to a cloud instance, so it cannot be replaced — the incident has no record of which instance belonged to the node when it opened; check that the node has a provider ID and that the controller can read the Kubernetes node (see docs/cloud-" + name + ".md), then resolve this incident with kubeneuronctl resolve inc-held --actor <who> so that the next fault opens a new one"
		}
		if err != nil || v.Reason != want || v.ReplaceAllowed || v.RecycleAllowed {
			t.Errorf("provider %q: verdict=%+v err=%v", name, v, err)
		}
	}
}

type checkpointRepairPlatform struct {
	checkpointPlatform
	active bool
}

func (p *checkpointRepairPlatform) CloudNode(context.Context, string) (cloud.NodeRef, error) {
	return cloud.NodeRef{}, nil
}
func (p *checkpointRepairPlatform) RecoveryEligibility(_ context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	return cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairOff, ReplaceAllowed: !p.active, Reason: "native repair became active"}, nil
}
func (p *checkpointRepairPlatform) ReplacePinnedNode(context.Context, cloud.NodeIdentity) error {
	return nil
}
func (p *checkpointRepairPlatform) RecyclePinnedNode(context.Context, cloud.NodeIdentity) error {
	return nil
}

func TestCloudRepairStartingDuringCheckpointPreventsDrain(t *testing.T) {
	p := &checkpointRepairPlatform{}
	p.workloads = []platform.Workload{optedIn("trainer", true)}
	c, inc, clock := checkpointFixture(t, p, enabledPolicy())
	inc.CloudIdentity = recycleTestPin()
	inc.CloudIdentity.Node = inc.Target.Node
	if err := c.requireCloudRecovery(context.Background(), inc, drainStep()); err != nil {
		t.Fatal(err)
	}
	p.observe = func(w platform.Workload, pass int) checkpoint.Observation {
		p.active = true
		return stampedAck(w, fixtureIncidentID, clock.Now().Add(time.Minute))
	}
	_, err := c.executePlatformStep(context.Background(), inc, "drain", drainStep())
	if !errors.Is(err, errCloudRecoveryUnavailable) {
		t.Fatalf("wanted refusal after checkpoint, got %v", err)
	}
	for _, call := range p.Calls() {
		if strings.HasPrefix(call, "drain ") {
			t.Fatalf("drain executed after repair began: %v", p.Calls())
		}
	}
	if !p.active {
		t.Fatal("checkpoint observation did not run")
	}
}

func TestCloudRefusalHoldsStepWithoutEscalation(t *testing.T) {
	p := &checkpointRepairPlatform{active: true}
	c, inc, _ := checkpointFixture(t, p, checkpoint.Policy{})
	inc.CloudIdentity = recycleTestPin()
	inc.CloudIdentity.Node = inc.Target.Node
	inc.State = types.StateExecuting
	if err := c.store.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	book, index := inc.Playbook, inc.StepIndex
	c.runStep(context.Background(), c.runtimeConfig(context.Background()).Engine, inc, drainStep())
	got, err := c.store.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateEvaluating || got.Playbook != book || got.StepIndex != index {
		t.Fatalf("refusal changed ladder or advanced step: %+v", got)
	}
	for _, call := range p.Calls() {
		if strings.HasPrefix(call, "drain ") {
			t.Fatal("refusal reached drain")
		}
	}
}

func TestCloudRefusalPreventsStepAdmission(t *testing.T) {
	p := &checkpointRepairPlatform{active: true}
	c, inc, _ := checkpointFixture(t, p, checkpoint.Policy{})
	inc.CloudIdentity = recycleTestPin()
	inc.CloudIdentity.Node = inc.Target.Node
	before := inc.State
	beforeDeferrals := snapshotDeferrals()
	if err := c.startStep(context.Background(), inc, drainStep(), "test"); err != nil {
		t.Fatal(err)
	}
	assertDeferred(t, beforeDeferrals, metrics.DeferRecoveryUnavailable)
	if inc.State != before || len(p.Calls()) != 0 {
		t.Fatalf("unavailable step admitted: state=%s calls=%v", inc.State, p.Calls())
	}
}

type countingCloudObserver struct {
	recyclePlatform
	reads int
}

func (p *countingCloudObserver) CloudNode(_ context.Context, node string) (cloud.NodeRef, error) {
	p.reads++
	pin := *recycleTestPin()
	pin.Node = node
	return cloud.NodeRef{Identity: pin}, nil
}
func TestRepeatedSignalsCaptureCloudIdentityOnlyOnce(t *testing.T) {
	c, st := newIngestTestController(t)
	p := &countingCloudObserver{}
	c.platform = p
	ctx := context.Background()
	sig := signal(types.ClassFellOffBus, "n1", "GPU-1")
	for range 5 {
		if err := c.ingest(ctx, sig); err != nil {
			t.Fatal(err)
		}
	}
	if p.reads != 1 {
		t.Fatalf("live identity reads=%d, want 1", p.reads)
	}
	inc, err := st.GetOpenIncident(ctx, sig.Target, sig.Class)
	if err != nil {
		t.Fatal(err)
	}
	if inc.CloudIdentity == nil || inc.CloudIdentity.InstanceID != "i-old" || inc.SignalSeen != 5 {
		t.Fatalf("incident=%+v", inc)
	}
	// Legacy incidents are not backfilled by later signals.
	inc.CloudIdentity = nil
	if err := st.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := c.ingest(ctx, sig); err != nil {
		t.Fatal(err)
	}
	if p.reads != 1 {
		t.Fatal("legacy incident triggered an identity lookup")
	}
}

type changedCloudPlatform struct {
	recyclePlatform
	observationErr error
}

func (p *changedCloudPlatform) RecoveryEligibility(context.Context, cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	return cloud.RecoveryEligibility{Reason: "identity observation"}, p.observationErr
}
func TestConfirmedCloudMismatchSupersedesButReadErrorHolds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		terminal bool
	}{
		{"confirmed reboot", cloud.ErrIdentityChanged, true},
		{"unavailable read", errors.New("node read failed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &changedCloudPlatform{observationErr: tc.err}
			c, inc, _ := checkpointFixture(t, p, checkpoint.Policy{})
			inc.CloudIdentity = recycleTestPin()
			inc.CloudIdentity.Node = inc.Target.Node
			before := inc.State
			if err := c.store.UpdateIncident(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			if err := c.startStep(context.Background(), inc, drainStep(), "test"); err != nil {
				t.Fatal(err)
			}
			got, err := c.store.GetIncident(context.Background(), inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.terminal {
				if got.State != types.StateSuperseded || got.ResolvedAt != nil {
					t.Fatalf("false recovery: %+v", got)
				}
				requireAudit(t, auditResults(t, c, got), "recovery unverified")
			} else if got.State != before {
				t.Fatalf("read error closed incident: %s", got.State)
			}
			if p.recycled != "" || p.replaced != "" {
				t.Fatal("mismatch reached cloud mutation")
			}
		})
	}
}

func TestVerifiedRecycleRepinPersistsAndRejectsStaleResult(t *testing.T) {
	ctx := context.Background()
	c, inc, _ := checkpointFixture(t, &changedCloudPlatform{}, checkpoint.Policy{})
	inc.CloudIdentity = recycleTestPin()
	inc.CloudIdentity.Node = inc.Target.Node
	if err := c.store.UpdateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	stale := inc.Clone()
	result := cloud.RecycleResult{Before: *inc.CloudIdentity, After: *inc.CloudIdentity, CompletedAt: time.Now()}
	result.After.BootID = "verified-new-boot"
	if err := c.repinAfterRecycle(ctx, inc, result); err != nil {
		t.Fatal(err)
	}
	saved, err := c.store.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *saved.CloudIdentity != result.After {
		t.Fatalf("pin not persisted: %+v", saved.CloudIdentity)
	}
	requireAudit(t, auditResults(t, c, saved), "pin updated after verified recycle")
	if err := c.repinAfterRecycle(ctx, stale, result); err == nil {
		t.Fatal("stale result overwrote current pin")
	}
}

// A changed cloud target is decided before authorization: an approval step
// that nobody authorised is superseded, not parked for a person to approve
// recovery of a node that is no longer the one that failed.
func TestConfirmedCloudMismatchSupersedesBeforeAuthorization(t *testing.T) {
	p := &changedCloudPlatform{observationErr: cloud.ErrIdentityChanged}
	c, inc, _ := checkpointFixture(t, p, checkpoint.Policy{})
	inc.CloudIdentity = recycleTestPin()
	inc.CloudIdentity.Node = inc.Target.Node
	if err := c.store.UpdateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	step := &playbook.Step{Name: "drain", Action: "platform.drain", Approval: "required"}
	if err := c.startStep(context.Background(), inc, step, "system"); err != nil {
		t.Fatal(err)
	}
	got, err := c.store.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.StateSuperseded {
		t.Fatalf("state=%s, want SUPERSEDED before any approval question", got.State)
	}
	if p.recycled != "" || p.replaced != "" {
		t.Fatal("mismatch reached cloud mutation")
	}
}
