// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestCloudAuthorizationHoldsBeforeCordonAndApprovalResume(t *testing.T) {
	for _, state := range []types.IncidentState{types.StateEvaluating, types.StateAwaitingApproval} {
		t.Run(string(state), func(t *testing.T) {
			c, st := stackTestController(t, &janitorPlatform{})
			ctx := context.Background()
			cordon := playbook.Step{Name: "cordon", Action: "platform.cordon"}
			engine, err := playbook.NewEngine(map[string]*playbook.Playbook{"cloud": {Name: "cloud", Steps: []playbook.Step{cordon, {Name: "replace", Action: "platform.replace_node"}}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.SetEngine(engine)
			inc := &types.Incident{ID: "no-pin", Playbook: "cloud", State: state, Target: types.Target{Node: "node-a"}}
			if err := st.CreateIncident(ctx, inc); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := c.authorizeAndStartStep(ctx, inc, &cordon); err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.GetIncident(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != state || got.StepIndex != 0 {
				t.Fatalf("incident advanced: %+v", got)
			}
			trail, err := st.AuditTrail(ctx, inc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(trail) != 1 || trail[0].Action != "provider-recovery-unavailable" || trail[0].Result == "" {
				t.Fatalf("audit=%+v", trail)
			}
		})
	}
}

// Embed the remaining methods only to satisfy the contract; this read-only
// gate must never invoke an observer or a mutation through this fake.
type changingEligibilityPlatform struct {
	janitorPlatform
	platform.GuardedNodeRecycler
	verdict cloud.RecoveryEligibility
	err     error
}

func (p *changingEligibilityPlatform) RecoveryEligibility(context.Context, cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	return p.verdict, p.err
}
func TestCloudEligibilityRestoredOnlyForPinnedIdentity(t *testing.T) {
	p := &changingEligibilityPlatform{}
	c, st := stackTestController(t, p)
	ctx := context.Background()
	pin := cloud.NodeIdentity{Node: "node-a", NodeUID: "uid", ProviderID: "aws:///zone/i-1", InstanceID: "i-1", BootID: "boot"}
	inc := &types.Incident{ID: "restore", State: types.StateAwaitingApproval, Target: types.Target{Node: "node-a"}, CloudIdentity: &pin}
	if err := st.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	step := &playbook.Step{Action: "platform.replace_node"}
	check := func(want bool) {
		t.Helper()
		held, err := c.holdUnavailableCloudRecovery(ctx, inc, step)
		if err != nil || held != want {
			t.Fatalf("held=%v err=%v", held, err)
		}
		got, err := st.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if *got.CloudIdentity != pin || got.State != types.StateAwaitingApproval {
			t.Fatalf("identity/state changed: %+v", got)
		}
	}
	p.err = errors.New("temporary read failure")
	check(true)
	p.err = nil
	p.verdict = cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairOff, ReplaceAllowed: true}
	check(false)
	check(false)
	trail, err := st.AuditTrail(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trail) != 2 || trail[0].Action != "provider-recovery-unavailable" || trail[1].Action != "provider-recovery-available" {
		t.Fatalf("audit=%+v", trail)
	}
}

// eligibleApprovalPlatform keeps the original platform's behavior while
// supplying the explicit cloud preconditions needed by approval-only tests.
type eligibleApprovalPlatform struct {
	platform.Platform
	platform.GuardedNodeRecycler
}

func (p eligibleApprovalPlatform) RecoveryEligibility(_ context.Context, pin cloud.NodeIdentity) (cloud.RecoveryEligibility, error) {
	return cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairOff, ReplaceAllowed: true, RecycleAllowed: true}, nil
}
func allowCloudApprovalFixture(c *Controller, inc *types.Incident) {
	inc.CloudIdentity = &cloud.NodeIdentity{Node: inc.Target.Node, NodeUID: "test-uid", ProviderID: "aws:///zone/i-test", InstanceID: "i-test", BootID: "test-boot"}
	c.platform = eligibleApprovalPlatform{Platform: c.platform}
}

func TestCloudAuthorizationSupersedesOnlyConfirmedMismatch(t *testing.T) {
	for _, state := range []types.IncidentState{types.StateEvaluating, types.StateAwaitingApproval} {
		for _, scenario := range []string{"typed", "uid", "boot", "read-error", "missing"} {
			t.Run(string(state)+"/"+scenario, func(t *testing.T) {
				pin := cloud.NodeIdentity{Node: "node-a", NodeUID: "uid", ProviderID: "aws:///zone/i-1", InstanceID: "i-1", BootID: "boot"}
				p := &changingEligibilityPlatform{verdict: cloud.RecoveryEligibility{Identity: pin, Mode: cloud.RepairOff, ReplaceAllowed: true}}
				want := types.StateSuperseded
				switch scenario {
				case "typed":
					p.err = cloud.ErrIdentityChanged
				case "uid":
					p.verdict.Identity.NodeUID = "other"
				case "boot":
					p.verdict.Identity.BootID = "other"
				case "read-error":
					p.err = errors.New("read failed")
					want = state
				case "missing":
					p.verdict.Identity = cloud.NodeIdentity{}
					want = state
				}
				c, st := stackTestController(t, p)
				ctx := context.Background()
				inc := &types.Incident{ID: "target", State: state, Target: types.Target{Node: pin.Node}, CloudIdentity: &pin}
				if err := st.CreateIncident(ctx, inc); err != nil {
					t.Fatal(err)
				}
				if err := c.authorizeAndStartStep(ctx, inc, &playbook.Step{Action: "platform.replace_node"}); err != nil {
					t.Fatal(err)
				}
				got, err := st.GetIncident(ctx, inc.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != want || got.ResolvedAt != nil || *got.CloudIdentity != pin {
					t.Fatalf("unexpected outcome: %+v", got)
				}
				trail, err := st.AuditTrail(ctx, inc.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(trail) != 1 {
					t.Fatalf("unexpected audit: %+v", trail)
				}
				action := "provider-recovery-unavailable"
				if want == types.StateSuperseded {
					action = "cloud-target-changed"
				}
				if trail[0].Action != action {
					t.Fatalf("audit action=%s", trail[0].Action)
				}
			})
		}
	}
}
