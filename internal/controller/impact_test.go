package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/impact"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// contextNotifier records approval requests with their context.
type contextNotifier struct {
	recordingNotifier
	mu       sync.Mutex
	contexts []notify.ApprovalContext
}

func (n *contextNotifier) RequestApprovalWithContext(ctx context.Context, inc *types.Incident, step string, ac notify.ApprovalContext) error {
	n.mu.Lock()
	n.contexts = append(n.contexts, ac)
	n.mu.Unlock()
	return n.RequestApproval(ctx, inc, step)
}

func (n *contextNotifier) seen() []notify.ApprovalContext {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.ApprovalContext(nil), n.contexts...)
}

func impactPolicy(require bool) impact.Policy {
	return impact.Policy{Enabled: true, Budget: impact.DefaultBudget, MaxItems: impact.DefaultMaxItems, RequireForApproval: require}
}

// impactFixture parks an incident of the gated playbook with the impact
// preview on; the platform lists three workloads, one eligible.
func impactFixture(t *testing.T, require bool) (*Controller, *types.Incident, *checkpointPlatform, *contextNotifier) {
	c, inc, plat, notifier, _ := impactFixtureClock(t, require)
	return c, inc, plat, notifier
}

func impactFixtureClock(t *testing.T, require bool) (*Controller, *types.Incident, *checkpointPlatform, *contextNotifier, *fakeClock) {
	t.Helper()
	plat := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{
		optedIn("trainer", true),
		{Name: "bystander", Namespace: "training", Kind: "Pod", UID: "uid-bystander", UsesGPU: true},
		{Name: "agent", Namespace: "kube-system", Kind: "Pod", UID: "uid-agent", DrainExclusion: platform.DrainExclusionInfrastructure},
	}}}
	c, inc, clock := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	notifier := &contextNotifier{}
	c.notifier = notifier
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Impact = impactPolicy(require) })
	return c, inc, plat, notifier, clock
}

func impactCounts() map[string]float64 {
	out := map[string]float64{}
	for _, r := range []string{"computed", "unavailable", "truncated"} {
		out[r] = testutil.ToFloat64(metrics.ImpactPreviews.WithLabelValues(r))
	}
	for _, b := range []string{"round", "round_and_impact", "none"} {
		out["bound:"+b] = testutil.ToFloat64(metrics.ApprovalsBound.WithLabelValues(b))
	}
	return out
}

// A park computes a durable preview for its round, counts and audits it,
// and hands its summary and digest to the notifier.
func TestImpactPreviewIsComputedAtPark(t *testing.T) {
	c, inc, _, notifier := impactFixture(t, false)
	before := impactCounts()
	fresh := park(t, c, inc)

	previews := c.store.(store.ImpactPreviewStore)
	rec, err := previews.LatestImpactPreview(context.Background(), inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatalf("no preview stored for round %d: %v", fresh.ApprovalEpoch, err)
	}
	if rec.UnavailableReason != "" || !strings.HasPrefix(rec.Digest, "sha256:") || rec.Version != impact.Version {
		t.Fatalf("record = %+v", rec)
	}
	p, err := impact.Parse(rec)
	if err != nil {
		t.Fatal(err)
	}
	if p.PendingStep != "cordon" || len(p.DownstreamDisruptions) != 1 || p.DownstreamDisruptions[0] != "platform.drain" {
		t.Fatalf("pending/downstream = %q %v", p.PendingStep, p.DownstreamDisruptions)
	}
	if p.Workloads.Total != 3 || p.Workloads.GPUHolders != 2 || p.Workloads.EligibleForCheckpoint != 1 {
		t.Fatalf("workloads = %+v", p.Workloads)
	}
	if p.Workloads.Ineligible["not-opted-in"] != 1 || p.Workloads.Ineligible["drain-excluded"] != 1 {
		t.Fatalf("ineligible = %v", p.Workloads.Ineligible)
	}
	if !p.CheckpointPolicy.Enabled || !p.CheckpointPolicy.Advisory {
		t.Fatalf("policy view = %+v", p.CheckpointPolicy)
	}
	after := impactCounts()
	if after["computed"]-before["computed"] != 1 {
		t.Fatalf("computed moved by %v, want 1", after["computed"]-before["computed"])
	}
	requireAudit(t, auditResults(t, c, inc), "round 1: computed at park "+rec.Digest)
	seen := notifier.seen()
	if len(seen) != 1 || seen[0].ImpactDigest != rec.Digest || !strings.Contains(seen[0].ImpactSummary, "3 total") {
		t.Fatalf("notifier context = %+v, want the summary and digest", seen)
	}
	// The API read answers the same record for the current round.
	got, err := c.ImpactPreview(context.Background(), inc.ID, 0)
	if err != nil || got.Digest != rec.Digest {
		t.Fatalf("ImpactPreview = %+v, %v", got, err)
	}
	if _, err := c.ImpactPreview(context.Background(), inc.ID, 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown round err = %v, want ErrNotFound", err)
	}
}

// A listing that fails is an unavailable record, not a missing one.
func TestImpactPreviewUnavailableIsExplicit(t *testing.T) {
	c, inc, plat, notifier := impactFixture(t, false)
	plat.mu.Lock()
	plat.listErr = errors.New("apiserver down")
	plat.mu.Unlock()
	before := impactCounts()
	fresh := park(t, c, inc)
	rec, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(context.Background(), inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if rec.UnavailableReason != impact.UnavailableListingFailed {
		t.Fatalf("record = %+v, want WorkloadListingFailed", rec)
	}
	if after := impactCounts(); after["unavailable"]-before["unavailable"] != 1 {
		t.Fatal("unavailable was not counted")
	}
	requireAudit(t, auditResults(t, c, inc), "unavailable (WorkloadListingFailed)")
	if seen := notifier.seen(); len(seen) != 1 || !strings.HasPrefix(seen[0].ImpactSummary, "impact preview unavailable") {
		t.Fatalf("notifier context = %+v", seen)
	}
}

// With requireForApproval an approve must carry the current digest: none,
// a stale one, or an unavailable preview is refused; a reject never is.
func TestApprovalBindsToTheImpactDigestWhenRequired(t *testing.T) {
	c, inc, plat, _, clock := impactFixtureClock(t, true)
	ctx := context.Background()
	fresh := park(t, c, inc)
	rec, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(ctx, inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", ""); !errors.Is(err, ErrImpactDigestRequired) {
		t.Fatalf("approve without digest err = %v, want ErrImpactDigestRequired", err)
	}
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", "sha256:stale"); !errors.Is(err, ErrImpactDigestMismatch) {
		t.Fatalf("approve with stale digest err = %v, want ErrImpactDigestMismatch", err)
	}
	// A recompute after the listing changed makes the earlier digest stale.
	plat.mu.Lock()
	plat.workloads = plat.workloads[:1]
	plat.mu.Unlock()
	clock.Advance(time.Minute)
	again, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "alice", "key-1")
	if err != nil || replayed || again.Digest == rec.Digest {
		t.Fatalf("recompute = %+v replayed=%v err=%v, want a new digest", again, replayed, err)
	}
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", rec.Digest); !errors.Is(err, ErrImpactDigestMismatch) {
		t.Fatalf("approve with the pre-recompute digest err = %v, want mismatch", err)
	}
	// Replay of the recompute key answers the same record without computing.
	replay, replayed, err := c.RecomputeImpactPreview(ctx, inc.ID, "alice", "key-1")
	if err != nil || !replayed || replay.Digest != again.Digest {
		t.Fatalf("replay = %+v replayed=%v err=%v", replay, replayed, err)
	}
	requireAudit(t, auditResults(t, c, inc), "recompute requested; new preview "+again.Digest)

	before := impactCounts()
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", again.Digest); err != nil {
		t.Fatalf("approve with the current digest: %v", err)
	}
	decision, err := c.store.LatestApprovalDecision(ctx, inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ImpactDigest != again.Digest {
		t.Fatalf("recorded digest = %q, want %q", decision.ImpactDigest, again.Digest)
	}
	if after := impactCounts(); after["bound:round_and_impact"]-before["bound:round_and_impact"] != 1 {
		t.Fatal("the bound decision was not counted as round_and_impact")
	}
}

func TestRejectIsNeverHeldBackByThePreview(t *testing.T) {
	c, inc, plat, _ := impactFixture(t, true)
	ctx := context.Background()
	plat.mu.Lock()
	plat.listErr = errors.New("apiserver down")
	plat.mu.Unlock()
	fresh := park(t, c, inc)
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", "sha256:any"); !errors.Is(err, ErrImpactPreviewUnavailable) {
		t.Fatalf("approve on an unavailable preview err = %v, want ErrImpactPreviewUnavailable", err)
	}
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalRejected, fresh.ApprovalEpoch, "not now", ""); err != nil {
		t.Fatalf("reject must not be held back: %v", err)
	}
}

// Without the requirement the preview is advisory: an approve without a
// digest is recorded bound to the round; a digest that matches is recorded;
// a stale one is still refused, because the client said what it saw.
func TestApprovalWithoutRequirementRecordsWhatWasSeen(t *testing.T) {
	c, inc, _, _ := impactFixture(t, false)
	ctx := context.Background()
	fresh := park(t, c, inc)
	rec, _ := c.store.(store.ImpactPreviewStore).LatestImpactPreview(ctx, inc.ID, fresh.ApprovalEpoch)
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", "sha256:stale"); !errors.Is(err, ErrImpactDigestMismatch) {
		t.Fatalf("stale digest err = %v, want mismatch even without the requirement", err)
	}
	before := impactCounts()
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", rec.Digest); err != nil {
		t.Fatal(err)
	}
	decision, _ := c.store.LatestApprovalDecision(ctx, inc.ID, fresh.ApprovalEpoch)
	if decision.ImpactDigest != rec.Digest {
		t.Fatalf("recorded digest = %q", decision.ImpactDigest)
	}
	if after := impactCounts(); after["bound:round_and_impact"]-before["bound:round_and_impact"] != 1 {
		t.Fatal("binding not counted")
	}
}

// Off is off: no preview, no record, no notifier context, and an approve
// carrying a digest is recorded bound to the round only.
func TestImpactPreviewOffIsSilent(t *testing.T) {
	c, inc, plat, notifier := impactFixture(t, false)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Impact = impact.Policy{} })
	ctx := context.Background()
	fresh := park(t, c, inc)
	if _, err := c.ImpactPreview(ctx, inc.ID, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("preview err = %v, want ErrNotFound", err)
	}
	if seen := notifier.seen(); len(seen) != 1 || seen[0].ImpactDigest != "" {
		t.Fatalf("notifier context = %+v, want empty", seen)
	}
	for _, r := range auditResults(t, c, inc) {
		if strings.Contains(r, "preview") {
			t.Fatalf("a disabled preview wrote %q", r)
		}
	}
	_ = plat
	if err := c.DecideApproval(ctx, inc.ID, "alice", "cli", types.ApprovalApproved, fresh.ApprovalEpoch, "", "sha256:ignored"); err != nil {
		t.Fatal(err)
	}
	decision, _ := c.store.LatestApprovalDecision(ctx, inc.ID, fresh.ApprovalEpoch)
	if decision.ImpactDigest != "" {
		t.Fatalf("recorded digest = %q, want none when the preview is off", decision.ImpactDigest)
	}
	if _, _, err := c.RecomputeImpactPreview(ctx, inc.ID, "alice", "k"); !errors.Is(err, ErrImpactPreviewUnavailable) {
		t.Fatalf("recompute with the preview off err = %v", err)
	}
}

// The preview runs under its budget: a hanging listing is an unavailable
// record within the budget, and the park has already happened.
func TestImpactPreviewIsBoundedByItsBudget(t *testing.T) {
	c, inc, plat, _ := impactFixture(t, false)
	plat.mu.Lock()
	plat.blockOn = map[string]bool{"list": true}
	plat.mu.Unlock()
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) {
		rc.Impact.Budget = 50 * time.Millisecond
		rc.Checkpoint = checkpoint.Policy{} // no advisory listing either
	})
	start := time.Now()
	fresh := park(t, c, inc)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the park took %v with a 50ms preview budget", elapsed)
	}
	rec, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(context.Background(), inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if rec.UnavailableReason != impact.UnavailableListingTimeout {
		t.Fatalf("reason = %q, want WorkloadListingTimeout", rec.UnavailableReason)
	}
}

// contextPlatform adds the optional resolver to the checkpoint fake.
type contextPlatform struct {
	*checkpointPlatform
	resolve func(w platform.Workload, which platform.ContextSelection) platform.WorkloadContext
}

func (p *contextPlatform) ResolveWorkloadContext(ctx context.Context, w platform.Workload, which platform.ContextSelection) platform.WorkloadContext {
	p.record(ctx, "resolve "+w.Namespace+"/"+w.Name)
	return p.resolve(w, which)
}

// Selected resolvers add context and owner chains to the itemised workloads;
// a resolver that could not observe is named once as a warning.
func TestImpactPreviewCarriesWorkloadContext(t *testing.T) {
	inner := &checkpointPlatform{basePlatform: basePlatform{workloads: []platform.Workload{optedIn("trainer", true), optedIn("other", true)}}}
	plat := &contextPlatform{checkpointPlatform: inner, resolve: func(w platform.Workload, which platform.ContextSelection) platform.WorkloadContext {
		wc := platform.WorkloadContext{OwnerChain: []string{"Job/" + w.Name}, Context: map[string]json.RawMessage{}, NotObserved: map[string]string{}}
		if which.BatchJobs {
			wc.Context["batch_job"] = json.RawMessage(`{"name":"` + w.Name + `","indexed":true}`)
		}
		if which.Kueue {
			wc.NotObserved["kueue"] = "api-unavailable"
		}
		return wc
	}}
	c, inc, _ := advisoryFixture(t, gatedPlaybook(), plat, advisoryPolicy())
	c.notifier = &contextNotifier{}
	policy := impactPolicy(false)
	policy.Context = platform.ContextSelection{BatchJobs: true, Kueue: true}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Impact = policy })
	fresh := park(t, c, inc)
	rec, err := c.store.(store.ImpactPreviewStore).LatestImpactPreview(context.Background(), inc.ID, fresh.ApprovalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	p, err := impact.Parse(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Workloads.Items) != 2 {
		t.Fatalf("items = %+v", p.Workloads.Items)
	}
	for _, it := range p.Workloads.Items {
		if len(it.OwnerChain) != 1 || it.OwnerChain[0] != "Job/"+it.Name {
			t.Fatalf("owner chain = %v", it.OwnerChain)
		}
		if !strings.Contains(string(it.Context), `"batch_job"`) || strings.Contains(string(it.Context), `"kueue"`) {
			t.Fatalf("context = %s", it.Context)
		}
	}
	found := false
	for _, w := range p.Warnings {
		if w == impact.WarningContextNotResolved {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %s", p.Warnings, impact.WarningContextNotResolved)
	}
	resolved := 0
	for _, call := range plat.Calls() {
		if strings.HasPrefix(call, "resolve ") {
			resolved++
		}
	}
	if resolved != 2 {
		t.Fatalf("resolver called %d times, want once per itemised workload", resolved)
	}
}
