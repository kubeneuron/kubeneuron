// SPDX-License-Identifier: LicenseRef-KubeNeuron

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/action"
	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/impact"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The impact preview of an approval round (docs/workload-safe-recovery-design.md,
// S4). At every park and re-park the controller gathers, under the policy
// budget, what the pending step and the disruptions downstream of it would
// touch — the node's topology classification, its workloads and which of
// them can checkpoint — and renders a durable impact-preview/v1 record sealed
// with a digest. The preview is advisory: it cannot block an approve. With
// requireForApproval the approve is bound to the digest the human saw, the
// same way it is bound to the round.

const (
	auditActionImpactComputed    = "impact-preview-computed"
	auditActionImpactUnavailable = "impact-preview-unavailable"
	auditActionImpactRecompute   = "impact-preview-recompute"

	impactAuditTimeout = 2 * time.Second
)

// Errors an approve can be refused with when the policy requires a preview.
// They are distinct so the API can say which, and all map to 409: the human
// must re-read and decide again.
var (
	// ErrImpactDigestRequired: the policy requires the digest and the client
	// sent none.
	ErrImpactDigestRequired = errors.New("impact preview: the policy requires the decision to carry the impact_digest the human was shown")
	// ErrImpactDigestMismatch: the digest is not the round's current preview.
	ErrImpactDigestMismatch = errors.New("impact preview: the impact_digest does not match the round's current preview; re-read the incident and decide again")
	// ErrImpactPreviewUnavailable: the round's current preview is unavailable
	// (or none exists) and the policy requires one; recompute first.
	ErrImpactPreviewUnavailable = errors.New("impact preview: the round's preview is unavailable; recompute it before deciding")
)

// impactStore returns the preview store when the workflow store has one.
func (c *Controller) impactStore() (store.ImpactPreviewStore, bool) {
	s, ok := c.store.(store.ImpactPreviewStore)
	return s, ok && s != nil
}

// previewImpact gathers and renders the preview of the incident's current
// round and stores it. It is called after the park committed, before the
// human is notified. It never returns an error to its caller: every failure
// is a stored unavailable record (when the store allows), a count and a log
// line, and a preview the store refused is still handed to the notification
// — the human is told what was seen even when the record did not last. The
// recompute path uses previewImpactDurable, which does not forgive that.
func (c *Controller) previewImpact(ctx context.Context, inc *types.Incident, trigger string) *impact.Preview {
	p, _ := c.previewImpactDurable(ctx, inc, trigger)
	return p
}

// previewImpactDurable is previewImpact with the persistence failure
// reported: the preview computed (nil when the policy is off or the
// incident has no round) and the store's refusal, if any.
func (c *Controller) previewImpactDurable(ctx context.Context, inc *types.Incident, trigger string) (*impact.Preview, error) {
	policy := c.runtimeConfig(ctx).Impact
	if !policy.Enabled || inc.ApprovalEpoch <= 0 {
		return nil, nil
	}
	budget := policy.Budget
	if budget <= 0 {
		budget = impact.DefaultBudget
	}
	started := time.Now()
	roundCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	in := impact.Input{
		IncidentID: inc.ID, Round: inc.ApprovalEpoch, ComputedAt: c.checkpointNowFunc()(), Node: inc.Target.Node,
		Checkpoint: c.runtimeConfig(ctx).Checkpoint, Class: inc.Class, MaxItems: policy.MaxItems,
	}
	step, done, err := c.runtimeConfig(ctx).Engine.NextStep(inc)
	if err != nil || done || step == nil {
		return c.storeImpact(roundCtx, inc, impact.Unavailable(in, impact.UnavailableNoPendingStep, "the round has no pending step"), trigger, started)
	}
	in.PendingStep = step.Name
	in.Downstream, in.Op, in.Force, in.StepBudget = c.downstreamDisruptions(ctx, inc)

	if classification, err := c.BuildTopologyClassification(roundCtx, inc.Target.Node); err == nil {
		in.Topology = &classification
	} else {
		c.log.Warn("impact preview: topology classification unavailable", "incident", inc.ID, "node", inc.Target.Node, "err", err)
		in.Topology = nil
	}
	workloads, err := c.platform.NodeWorkloads(roundCtx, inc.Target.Node)
	if err != nil {
		reason := impact.UnavailableListingFailed
		if errors.Is(err, context.DeadlineExceeded) || roundCtx.Err() != nil {
			reason = impact.UnavailableListingTimeout
		}
		return c.storeImpact(roundCtx, inc, impact.Unavailable(in, reason, "list workloads on "+inc.Target.Node), trigger, started)
	}
	in.Workloads = workloads
	if cp := in.Checkpoint; cp.Groups.Enabled {
		in.Groups = &impact.GroupPolicy{MaxGroupSize: cp.Groups.MaxGroupSize, CrossNode: cp.Groups.CrossNode}
		in.GroupTotals, in.GroupTruncated = c.previewGroupTotals(roundCtx, cp, workloads)
	}
	if policy.Context.Any() {
		in.Contexts, in.Owners, in.ContextNotObserved = c.previewContexts(roundCtx, policy.Context, workloads, policy.MaxItems)
	}
	return c.storeImpact(roundCtx, inc, impact.Compute(in), trigger, started)
}

// previewContexts consults the read-only resolvers for the workloads the
// preview itemises (at most maxItems, in the renderer's order), under the
// round's budget. A resolver that answered not-observed for any workload is
// named once in notObserved; a budget that runs out leaves the rest without
// context and names every selected resolver.
func (c *Controller) previewContexts(ctx context.Context, which platform.ContextSelection, workloads []platform.Workload, maxItems int) (map[string]json.RawMessage, map[string][]string, []string) {
	resolver, ok := c.platform.(platform.WorkloadContextResolver)
	contexts, owners := map[string]json.RawMessage{}, map[string][]string{}
	missing := map[string]struct{}{}
	if !ok {
		for _, name := range which.Names() {
			missing[name] = struct{}{}
		}
		return contexts, owners, sortedKeys(missing)
	}
	if maxItems <= 0 {
		maxItems = impact.DefaultMaxItems
	}
	sorted := append([]platform.Workload(nil), workloads...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})
	for i, w := range sorted {
		if i >= maxItems {
			break
		}
		if ctx.Err() != nil {
			for _, name := range which.Names() {
				missing[name] = struct{}{}
			}
			break
		}
		wc := resolver.ResolveWorkloadContext(ctx, w, which)
		key := w.Namespace + "/" + w.Name
		if len(wc.OwnerChain) > 0 {
			owners[key] = wc.OwnerChain
		}
		if len(wc.Context) > 0 {
			raw, err := json.Marshal(wc.Context)
			if err == nil {
				contexts[key] = raw
			}
		}
		for name := range wc.NotObserved {
			missing[name] = struct{}{}
		}
	}
	return contexts, owners, sortedKeys(missing)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// downstreamDisruptions lists the coordinating platform ops at or after the
// current step as wire names ("platform.drain"), with the first one's op,
// force flag and coordination budget: the step's intended timeout minus the
// reserve for the disruption itself, the same arithmetic the step will do
// (checkpointBudget) less the live context it does not have yet.
func (c *Controller) downstreamDisruptions(ctx context.Context, inc *types.Incident) (downstream []string, op string, force bool, budget time.Duration) {
	book, ok := c.runtimeConfig(ctx).Engine.Playbook(inc.Playbook)
	if !ok || inc.StepIndex < 0 {
		return nil, "", false, 0
	}
	for i := inc.StepIndex; i < len(book.Steps); i++ {
		step := &book.Steps[i]
		def, known := action.ByWire(step.Action)
		if !known || def.Kind != action.KindPlatform || !checkpointOpCoordinates(def.Op) {
			continue
		}
		downstream = append(downstream, step.Action)
		if op == "" {
			op, force = def.Op, drainForce(step)
			budget = effectiveStepTimeout(step) - checkpointBudgetReserve
		}
	}
	return downstream, op, force, budget
}

// storeImpact persists the rendered preview, counts it and audits it. The
// audit row outlives the round's budget the way the advisory's does. The
// returned error is the persistence failure, when the store refused the
// record: the preview is still returned (it was computed and is what the
// notification may carry), and the caller decides whether a preview that
// is not durable is an answer — the park says so and goes on; a recompute
// fails the request, because the digest it would hand back names nothing.
func (c *Controller) storeImpact(ctx context.Context, inc *types.Incident, p impact.Preview, trigger string, started time.Time) (*impact.Preview, error) {
	metrics.ImpactPreviewSeconds.Observe(time.Since(started).Seconds())
	result := "computed"
	switch {
	case p.Unavailable != nil:
		result = "unavailable"
	case p.Limits.ItemsTruncated:
		result = "truncated"
	}
	rec, err := impact.Record(p)
	if err != nil {
		c.log.Error("impact preview: render failed", "incident", inc.ID, "err", err)
		metrics.ImpactPreviews.WithLabelValues("unavailable").Inc()
		return nil, err
	}
	if rec.UnavailableReason != "" {
		result = "unavailable"
		p.Unavailable = &impact.UnavailableView{Reason: rec.UnavailableReason}
	}
	metrics.ImpactPreviews.WithLabelValues(result).Inc()
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), impactAuditTimeout)
	defer cancel()
	var stored error
	if previews, ok := c.impactStore(); ok {
		switch err := previews.PutImpactPreview(auditCtx, rec); {
		case err == nil:
		case errors.Is(err, store.ErrConflict):
			// The same (incident, round, computed_at) is already there. It is
			// this record only if its digest matches; a different document at
			// the same instant was NOT stored and must not be reported as such.
			if existing, getErr := c.impactPreviewByDigest(auditCtx, previews, inc.ID, inc.ApprovalEpoch, rec.Digest); getErr != nil || existing == nil {
				stored = fmt.Errorf("impact preview %s collides with another record computed at the same instant", rec.Digest)
				c.log.Error("impact preview: store refused a colliding record", "incident", inc.ID, "round", inc.ApprovalEpoch, "err", stored)
			}
		default:
			c.log.Error("impact preview: store failed", "incident", inc.ID, "round", inc.ApprovalEpoch, "err", err)
			stored = err
		}
	} else {
		c.log.Warn("impact preview: the workflow store retains no previews; nothing durable", "incident", inc.ID)
		stored = fmt.Errorf("%w: the workflow store retains no impact previews", ErrImpactPreviewUnavailable)
	}
	actionName := auditActionImpactComputed
	text := fmt.Sprintf("round %d: %s %s; %d workload(s), %d GPU holder(s), %d eligible for checkpoint, topology %s",
		p.Round, trigger, p.Digest, p.Workloads.Total, p.Workloads.GPUHolders, p.Workloads.EligibleForCheckpoint, impactTopology(p))
	if p.Unavailable != nil {
		actionName = auditActionImpactUnavailable
		text = fmt.Sprintf("round %d: %s %s; unavailable (%s)", p.Round, trigger, p.Digest, p.Unavailable.Reason)
	}
	if len(p.Warnings) > 0 {
		text += "; warnings " + strings.Join(p.Warnings, ", ")
	}
	if err := c.appendAudit(auditCtx, inc, "system", actionName, text); err != nil {
		c.log.Error("impact preview audit append failed", "incident", inc.ID, "err", err)
	}
	return &p, stored
}

func impactTopology(p impact.Preview) string {
	if p.Node.Topology == nil {
		return string(topology.TopologyUnknown) + " (unclassified)"
	}
	return string(p.Node.Topology.Topology) + "/" + string(p.Node.Topology.DeviceActions)
}

// approvalContext renders what a notification may carry beside the round.
func approvalContext(p *impact.Preview) notify.ApprovalContext {
	if p == nil {
		return notify.ApprovalContext{}
	}
	return notify.ApprovalContext{ImpactSummary: impact.Summary(*p), ImpactDigest: p.Digest}
}

// ImpactPreview returns the current (newest) preview of one round, or of the
// incident's current round when round is 0. store.ErrNotFound when the
// incident is unknown or the round has no preview; ErrImpactPreviewUnavailable
// is NOT returned here — an unavailable record is a readable answer.
func (c *Controller) ImpactPreview(ctx context.Context, id string, round int) (*types.ImpactPreview, error) {
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	previews, ok := c.impactStore()
	if !ok {
		return nil, fmt.Errorf("%w: the workflow store retains no impact previews", store.ErrNotFound)
	}
	if round <= 0 {
		round = inc.ApprovalEpoch
	}
	if round <= 0 {
		return nil, store.ErrNotFound
	}
	return previews.LatestImpactPreview(ctx, id, round)
}

// RecomputeImpactPreview recomputes the preview of the incident's current
// round on an operator's request. It is idempotent per key through the
// operational idempotency table when the store has one, audited under the
// actor, and returns the new record. The incident must be awaiting approval.
//
// The order is compute, persist, THEN take the key: a key is a promise that
// its result exists, so it is written only once the preview is durable,
// carrying that preview's digest. A replay answers with the preview the key
// computed — not the round's newest, which after another operator's
// recompute is a different document — and a recompute whose record the
// store refused fails the request without consuming the key, so the same
// key can retry once the store is back.
func (c *Controller) RecomputeImpactPreview(ctx context.Context, id, actor, idempotencyKey string) (*types.ImpactPreview, bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return nil, false, fmt.Errorf("incident ID, actor, and idempotency key are required")
	}
	if !c.runtimeConfig(ctx).Impact.Enabled {
		return nil, false, fmt.Errorf("%w: impact preview is not enabled", ErrImpactPreviewUnavailable)
	}
	previews, ok := c.impactStore()
	if !ok {
		return nil, false, fmt.Errorf("%w: the workflow store retains no impact previews", ErrImpactPreviewUnavailable)
	}
	inc, err := c.store.GetIncident(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if inc.State != types.StateAwaitingApproval || inc.ApprovalEpoch <= 0 {
		return nil, false, fmt.Errorf("%w: incident %s is in state %s, not awaiting approval", store.ErrConflict, id, inc.State)
	}
	operational, hasOperational := c.store.(store.OperationalStore)
	digest := incidentOperationDigest("impact-recompute", id, actor, "", inc.ApprovalEpoch)
	storageKey := incidentOperationKey("impact-recompute", idempotencyKey)
	replay := func(record *types.OperationalIdempotencyRecord) (*types.ImpactPreview, bool, error) {
		if record.RequestDigest != digest || record.ResourceID != id {
			return nil, false, store.ErrOperationalConflict
		}
		rec, getErr := c.impactPreviewByDigest(ctx, previews, id, inc.ApprovalEpoch, record.ResultDigest)
		return rec, true, getErr
	}
	if hasOperational {
		if existing, lookupErr := operational.GetOperationalIdempotency(ctx, types.ResourceIncidentOperation, actor, storageKey); lookupErr == nil {
			return replay(existing)
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			return nil, false, lookupErr
		}
	}
	p, storeErr := c.previewImpactDurable(ctx, inc, "recomputed by "+actor)
	if p == nil {
		return nil, false, fmt.Errorf("%w: recompute produced no record", ErrImpactPreviewUnavailable)
	}
	if storeErr != nil {
		return nil, false, fmt.Errorf("%w: recompute computed %s but the store refused the record: %v", ErrImpactPreviewUnavailable, p.Digest, storeErr)
	}
	if err := c.appendAudit(ctx, inc, actor, auditActionImpactRecompute, fmt.Sprintf("round %d: recompute requested; new preview %s", inc.ApprovalEpoch, p.Digest)); err != nil {
		c.log.Error("impact recompute audit append failed", "incident", id, "err", err)
	}
	if hasOperational {
		record, created, putErr := operational.PutOperationalIdempotency(ctx, &types.OperationalIdempotencyRecord{
			Kind: types.ResourceIncidentOperation, Actor: actor, Key: storageKey,
			RequestDigest: digest, ResourceID: id, ResultDigest: p.Digest,
		})
		if putErr != nil {
			return nil, false, putErr
		}
		if err := operational.AppendOperationalAudit(ctx, &types.OperationalAuditEvent{
			Kind: types.ResourceIncidentOperation, ResourceID: id, Time: time.Now(),
			Actor: actor, Action: "impact-recompute", RequestID: idempotencyKey,
			Params: map[string]string{"round": fmt.Sprint(inc.ApprovalEpoch)}, Result: p.Digest,
		}); err != nil {
			c.log.Error("operational impact recompute audit append failed", "incident", id, "err", err)
		}
		if !created {
			// A concurrent request under the same key took it first; its
			// result is the key's result, and this one is a replay of it.
			return replay(record)
		}
	}
	rec, err := c.impactPreviewByDigest(ctx, previews, id, inc.ApprovalEpoch, p.Digest)
	return rec, false, err
}

// impactPreviewByDigest returns the round's record with the given digest, or
// the round's newest record when the digest is empty (an idempotency row
// written before result digests were recorded).
func (c *Controller) impactPreviewByDigest(ctx context.Context, previews store.ImpactPreviewStore, id string, round int, digest string) (*types.ImpactPreview, error) {
	if digest == "" {
		return previews.LatestImpactPreview(ctx, id, round)
	}
	all, err := previews.ListImpactPreviews(ctx, id, round)
	if err != nil {
		return nil, err
	}
	for _, rec := range all {
		if rec.Digest == digest {
			return rec, nil
		}
	}
	return nil, fmt.Errorf("%w: no impact preview %s on round %d", store.ErrNotFound, digest, round)
}

// bindImpactDigest decides what an approve/reject may carry and record. With
// requireForApproval an approve must carry the digest of the round's current,
// available preview; a reject is never held back by the preview (refusing is
// always safe). Without the requirement, a digest the client sends is recorded
// as what was seen when it matches the current preview, and an approve with a
// stale digest is still refused — the client said what it saw, and it was
// not this. The returned binding labels the metric.
func (c *Controller) bindImpactDigest(ctx context.Context, inc *types.Incident, decision types.ApprovalDecision, sent string) (recorded, binding string, err error) {
	policy := c.runtimeConfig(ctx).Impact
	sent = strings.TrimSpace(sent)
	if !policy.Enabled {
		return "", "round", nil
	}
	var current *types.ImpactPreview
	if previews, ok := c.impactStore(); ok {
		current, _ = previews.LatestImpactPreview(ctx, inc.ID, inc.ApprovalEpoch)
	}
	if decision == types.ApprovalApproved && policy.RequireForApproval {
		if current == nil || current.UnavailableReason != "" {
			return "", "", ErrImpactPreviewUnavailable
		}
		if sent == "" {
			return "", "", ErrImpactDigestRequired
		}
	}
	if sent == "" {
		return "", "round", nil
	}
	if current == nil || current.Digest != sent {
		if decision == types.ApprovalApproved {
			return "", "", ErrImpactDigestMismatch
		}
		return "", "round", nil // a reject with a stale digest is still a reject
	}
	return sent, "round_and_impact", nil
}

// previewGroupTotals lists each checkpoint group seen on the node, bounded
// by the policy, so the preview can say how many members the group has
// beyond this node. A listing that fails or runs out of budget leaves the
// group out of the totals: the renderer then shows the total as unknown.
func (c *Controller) previewGroupTotals(ctx context.Context, policy checkpoint.Policy, workloads []platform.Workload) (map[string]int, map[string]bool) {
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		return nil, nil
	}
	maxSize := policy.Groups.MaxGroupSize
	if maxSize <= 0 {
		maxSize = checkpoint.DefaultMaxGroupSize
	}
	totals, truncated := map[string]int{}, map[string]bool{}
	for _, w := range workloads {
		key, ok := checkpoint.GroupOf(w.Checkpoint())
		if !ok {
			continue
		}
		if _, done := totals[key.String()]; done || ctx.Err() != nil {
			continue
		}
		members, more, err := checkpointer.ListGroupMembers(ctx, policy, key.Namespace, key.Name, maxSize)
		if err != nil {
			continue
		}
		totals[key.String()] = len(members)
		if more {
			truncated[key.String()] = true
		}
	}
	return totals, truncated
}
