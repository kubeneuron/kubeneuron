// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Add-first replacement. Before the first step of a rung that disrupts the
// node (cordon, drain, GPU-workload eviction) and that ends in a replacement,
// the controller has the cloud provider bring up a successor in the node's
// pool and waits until it is Ready, schedulable and reporting its
// accelerators. Only then is the node cordoned, drained and removed. A
// successor that does not get there in time is removed again, if nothing else
// runs on it, and the node is left untouched.
//
// The operation is the incident's: it is recorded in the incident before the
// cloud is asked for anything, after every phase, and when it ends, so a
// restarted controller continues it and never asks twice.

// defaultSuccessorPoll is how often a waiting successor is looked at again.
const defaultSuccessorPoll = 15 * time.Second

// addFirstCheckTTL bounds how long one provider answer about add-first is
// reused across reconcile passes.
const addFirstCheckTTL = time.Minute

const successorReleaseOwnerPrefix = "successor-release/"

// addFirstChecks caches CheckAddFirst answers per incident attempt.
type addFirstChecks struct {
	mu      sync.Mutex
	entries map[string]addFirstCheck
}

type addFirstCheck struct {
	at  time.Time
	err error
}

func (c *Controller) successorPollInterval() time.Duration {
	if c.successorPoll > 0 {
		return c.successorPoll
	}
	return defaultSuccessorPoll
}

func successorOperation(inc *types.Incident) string {
	return fmt.Sprintf("%s-a%d", inc.ID, inc.Attempt)
}

// successorReady reports whether this attempt has a successor that was
// accepted and is still open.
func successorReady(inc *types.Incident) bool {
	s := inc.Successor
	return s.Open() && s.Operation == successorOperation(inc) && s.ReadyAt != nil
}

func disruptiveNodeStep(action string) bool {
	switch action {
	case "platform.cordon", "platform.drain", "platform.evict_gpu_workload":
		return true
	}
	return false
}

// successorStep reports whether step is where this incident's add-first
// successor has to be prepared: the policy adds first, the step disrupts the
// node, the same rung replaces the node after it without a cloud restart
// first, and no successor has been accepted yet.
func (c *Controller) successorStep(ctx context.Context, inc *types.Incident, step *playbook.Step) bool {
	if !disruptiveNodeStep(step.Action) || successorReady(inc) {
		return false
	}
	pol := c.incidentPolicy(ctx, inc)
	if pol.ReplacementStrategy.Effective() != recoverymode.AddFirst {
		return false
	}
	return c.rungAddsFirst(ctx, inc, pol)
}

// rungAddsFirst reports whether the incident's current rung replaces the node
// after its current step, with no enabled cloud restart on the way.
func (c *Controller) rungAddsFirst(ctx context.Context, inc *types.Incident, pol playbook.Policy) bool {
	engine := c.runtimeConfig(ctx).Engine
	if engine == nil {
		return false
	}
	book, ok := engine.Playbook(inc.Playbook)
	if !ok || inc.StepIndex >= len(book.Steps) || !replaceAfter(book, inc.StepIndex) {
		return false
	}
	if pol.RecycleEnabled {
		for _, s := range book.Steps[inc.StepIndex:] {
			if s.Action == "platform.recycle_node" {
				return false
			}
		}
	}
	return true
}

// addFirstHold is why the step must wait under an add-first policy, or "".
func (c *Controller) addFirstHold(ctx context.Context, inc *types.Incident, step *playbook.Step) string {
	if successorReady(inc) {
		return ""
	}
	if step.Action == "platform.replace_node" {
		// A simulation prepares no successor; it replaces wherever the
		// live run could have prepared one.
		if c.simulating(ctx, inc) && c.addFirstAvailable(ctx, inc) == nil {
			return ""
		}
		return "recovery unavailable: no prepared replacement is ready for this node, and the policy does not name remove-first"
	}
	if !c.successorStep(ctx, inc, step) {
		return "recovery unavailable: " + addFirstUnavailable + "; the node is not cordoned or drained"
	}
	if err := c.addFirstAvailable(ctx, inc); err != nil {
		return "recovery unavailable: " + addFirstReason(err) + "; the node is not cordoned or drained"
	}
	return ""
}

func addFirstReason(err error) string {
	if errors.Is(err, cloud.ErrAddFirstUnavailable) {
		return err.Error()
	}
	return "whether the node's pool can add a replacement first could not be read: " + err.Error()
}

// addFirstAvailable asks the provider, at most once a minute per attempt,
// whether the pinned node's pool can prepare a successor.
func (c *Controller) addFirstAvailable(ctx context.Context, inc *types.Incident) error {
	plat, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok {
		return fmt.Errorf("%w: the platform cannot prepare a successor", cloud.ErrAddFirstUnavailable)
	}
	if inc.CloudIdentity == nil {
		return fmt.Errorf("%w: the incident has no pinned cloud identity", cloud.ErrAddFirstUnavailable)
	}
	if s := inc.Successor; s.Open() && s.Operation == successorOperation(inc) && s.Phase != "" {
		return nil // already under way; the provider settles it from here
	}
	key := successorOperation(inc)
	c.addFirst.mu.Lock()
	entry, cached := c.addFirst.entries[key]
	c.addFirst.mu.Unlock()
	if cached && time.Since(entry.at) < addFirstCheckTTL {
		return entry.err
	}
	err := plat.AddFirstAvailable(ctx, *inc.CloudIdentity)
	c.addFirst.mu.Lock()
	if c.addFirst.entries == nil {
		c.addFirst.entries = map[string]addFirstCheck{}
	}
	for k, e := range c.addFirst.entries {
		if time.Since(e.at) >= addFirstCheckTTL {
			delete(c.addFirst.entries, k)
		}
	}
	c.addFirst.entries[key] = addFirstCheck{at: time.Now(), err: err}
	c.addFirst.mu.Unlock()
	return err
}

// stepNeedsApproval is step.NeedsApproval, and also true for the step that
// prepares a successor: the successor is a billed instance, so it is started
// only under a person's approval or an automatic policy, within the policy's
// limits.
func (c *Controller) stepNeedsApproval(ctx context.Context, inc *types.Incident, step *playbook.Step) bool {
	if step.NeedsApproval() {
		return true
	}
	return !c.simulating(ctx, inc) && c.successorStep(ctx, inc, step)
}

// successorApprovalNote is what the approval round, and a simulated step,
// says about the successor.
func successorApprovalNote(inc *types.Incident) string {
	pool := "the node's pool"
	if s := inc.Successor; s != nil && s.Pool != "" {
		pool = "pool " + s.Pool
	}
	return fmt.Sprintf("This step creates one temporary additional node in %s. The cloud provider bills it until the old node is deleted.", pool)
}

// planSuccessor records this attempt's operation and the provider's plan for
// it. Planning reads the pool and changes nothing in the cloud.
func (c *Controller) planSuccessor(ctx context.Context, inc *types.Incident) error {
	if s := inc.Successor; s != nil && s.Operation == successorOperation(inc) {
		return nil
	}
	if s := inc.Successor; s.Open() {
		return fmt.Errorf("the successor operation %s of an earlier attempt is still open", s.Operation)
	}
	plat, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok || inc.CloudIdentity == nil {
		return fmt.Errorf("%w: the platform cannot prepare a successor", cloud.ErrAddFirstUnavailable)
	}
	pin := *inc.CloudIdentity
	planned, err := plat.PrepareSuccessor(ctx, pin, cloud.Successor{Operation: successorOperation(inc), Instance: pin.InstanceID})
	if err != nil {
		return err
	}
	if planned.Phase != cloud.SuccessorPlanned || planned.Operation != successorOperation(inc) || planned.Instance != pin.InstanceID {
		return fmt.Errorf("the provider returned phase %q for operation %q when asked to plan", planned.Phase, planned.Operation)
	}
	rec := &types.SuccessorRecord{Successor: planned}
	return c.saveSuccessor(ctx, inc, rec, "successor-planned",
		fmt.Sprintf("operation %s: a successor of %s is planned in pool %s", planned.Operation, inc.Target.Node, planned.Pool))
}

// saveSuccessor persists rec on the incident with one audit row, and keeps
// inc in step with what was written.
func (c *Controller) saveSuccessor(ctx context.Context, inc *types.Incident, rec *types.SuccessorRecord, action, result string) error {
	now := time.Now()
	var saved *types.Incident
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		fresh, err := tx.GetIncident(ctx, inc.ID)
		if err != nil {
			return err
		}
		if fresh.Successor != nil && fresh.Successor.Operation != rec.Operation && fresh.Successor.Open() {
			return store.ErrConflict
		}
		fresh.Successor = rec.Clone()
		fresh.UpdatedAt = now
		if err := tx.UpdateIncident(ctx, fresh); err != nil {
			return err
		}
		saved = fresh
		return tx.AppendAudit(ctx, &types.AuditEntry{
			IncidentID: inc.ID, Time: now, FromState: fresh.State, ToState: fresh.State,
			Actor: "system", Action: action, Result: result, DryRun: fresh.DryRun,
			Params: map[string]string{"operation": rec.Operation, "phase": string(rec.Phase), "outcome": rec.Outcome},
		})
	})
	if err != nil {
		return err
	}
	inc.Successor = saved.Successor.Clone()
	inc.Version = saved.Version
	inc.UpdatedAt = saved.UpdatedAt
	return nil
}

// awaitSuccessor runs the operation until its successor is accepted. It
// returns an error, and leaves the node alone, when there is no capacity, when
// the provider cannot tie a new instance to the operation, or when the
// successor is not ready within the policy's timeout; a successor created by
// then is removed if nothing runs on it.
func (c *Controller) awaitSuccessor(ctx context.Context, inc *types.Incident) error {
	plat, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok || inc.CloudIdentity == nil {
		return fmt.Errorf("%w: the platform cannot prepare a successor", cloud.ErrAddFirstUnavailable)
	}
	if err := c.planSuccessor(ctx, inc); err != nil {
		return fmt.Errorf("add-first replacement: %w", err)
	}
	pin := *inc.CloudIdentity
	timeout := recoverymode.EffectiveSuccessorTimeout(c.incidentPolicy(ctx, inc).SuccessorTimeout)
	for {
		rec := inc.Successor.Clone()
		if !rec.Open() {
			return fmt.Errorf("add-first replacement: the successor operation %s ended (%s): %s", rec.Operation, rec.Outcome, rec.Reason)
		}
		if rec.ReadyAt != nil {
			return nil
		}
		if !rec.RequestedAt.IsZero() && time.Since(rec.RequestedAt) > timeout {
			why := fmt.Sprintf("no replacement was ready within %s", timeout)
			if err := c.releaseSuccessor(ctx, inc, why); err != nil {
				return fmt.Errorf("add-first replacement: %s; removing the successor failed and is retried: %w", why, err)
			}
			return fmt.Errorf("add-first replacement: %s; %s was not cordoned or drained (%s)", why, inc.Target.Node, inc.Successor.Reason)
		}
		if err := c.advanceSuccessor(ctx, inc, plat, pin, rec); err != nil {
			return err
		}
		if successorReady(inc) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("add-first replacement: waiting for the successor: %w", ctx.Err())
		case <-time.After(c.successorPollInterval()):
		}
	}
}

// advanceSuccessor takes one step of the operation and records it.
func (c *Controller) advanceSuccessor(ctx context.Context, inc *types.Incident, plat platform.SuccessorNodeRecycler, pin cloud.NodeIdentity, rec *types.SuccessorRecord) error {
	if rec.Phase == cloud.SuccessorCreated {
		return c.acceptSuccessor(ctx, inc, plat, pin, rec)
	}
	if rec.Phase == cloud.SuccessorPlanned && rec.RequestedAt.IsZero() {
		// Record the request time before the request, so the timeout runs
		// from the first attempt even across a restart.
		rec.RequestedAt = time.Now().UTC()
		if err := c.saveSuccessor(ctx, inc, rec, "successor-requesting",
			fmt.Sprintf("operation %s: requesting a successor of %s in pool %s", rec.Operation, inc.Target.Node, rec.Pool)); err != nil {
			return err
		}
	}
	next, err := plat.PrepareSuccessor(ctx, pin, rec.Successor)
	switch {
	case errors.Is(err, cloud.ErrReplacementCapacity):
		why := "no capacity or quota for a replacement: " + err.Error()
		if rerr := c.releaseSuccessor(ctx, inc, why); rerr != nil {
			return fmt.Errorf("add-first replacement: replacement capacity unavailable: %w; removing what was requested failed and is retried: %v", err, rerr)
		}
		if inc.Successor.Outcome == types.SuccessorNotCreated || inc.Successor.Outcome == types.SuccessorReleased {
			inc.Successor.Outcome = types.SuccessorCapacityDenied
			if serr := c.saveSuccessor(ctx, inc, inc.Successor.Clone(), "successor-capacity-denied", why); serr != nil {
				return serr
			}
		}
		return fmt.Errorf("add-first replacement: replacement capacity unavailable: %w; %s was not cordoned or drained", err, inc.Target.Node)
	case errors.Is(err, cloud.ErrNeedsOperator):
		if serr := c.endSuccessor(ctx, inc, types.SuccessorHandedOver, err.Error()); serr != nil {
			return serr
		}
		return fmt.Errorf("add-first replacement: %w; %s was not cordoned or drained", err, inc.Target.Node)
	case err != nil:
		return fmt.Errorf("add-first replacement: %w", err)
	}
	if next.Operation != rec.Operation || next.Instance != rec.Instance {
		return fmt.Errorf("add-first replacement: the provider answered for operation %q, not %q", next.Operation, rec.Operation)
	}
	if next.Phase == rec.Phase && next.ProviderID == rec.ProviderID {
		return nil
	}
	rec.Successor = next
	if rec.RequestedAt.IsZero() {
		rec.RequestedAt = time.Now().UTC()
	}
	result := fmt.Sprintf("operation %s: successor %s", rec.Operation, next.Phase)
	if next.ProviderID != "" {
		result += " as " + next.ProviderID
	}
	return c.saveSuccessor(ctx, inc, rec, "successor-"+string(next.Phase), result)
}

// acceptSuccessor accepts a created successor once its node is Ready and
// schedulable, joined after the request, can run at least every extended
// resource the replaced node advertised, and its agent has registered after
// the request with its accelerators. Inventory without allocatable resources,
// or a node in the pool that this operation did not create, never counts.
func (c *Controller) acceptSuccessor(ctx context.Context, inc *types.Incident, plat platform.SuccessorNodeRecycler, pin cloud.NodeIdentity, rec *types.SuccessorRecord) error {
	node, err := plat.SuccessorNode(ctx, rec.ProviderID)
	if err != nil || !node.Found {
		return err
	}
	if rec.Node != node.Name || rec.NodeUID != node.UID {
		rec.Node, rec.NodeUID = node.Name, node.UID
		if err := c.saveSuccessor(ctx, inc, rec, "successor-registered",
			fmt.Sprintf("operation %s: successor is node %s", rec.Operation, node.Name)); err != nil {
			return err
		}
	}
	if reason := c.successorNotReady(ctx, inc, pin, rec, node); reason != "" {
		if rec.Reason != reason {
			rec.Reason = reason
			return c.saveSuccessor(ctx, inc, rec, "successor-waiting", fmt.Sprintf("operation %s: %s", rec.Operation, reason))
		}
		return nil
	}
	now := time.Now().UTC()
	rec.ReadyAt, rec.Reason = &now, ""
	return c.saveSuccessor(ctx, inc, rec, "successor-ready",
		fmt.Sprintf("operation %s: successor %s is Ready with its accelerators; two nodes run until %s is removed", rec.Operation, node.Name, inc.Target.Node))
}

func (c *Controller) successorNotReady(ctx context.Context, inc *types.Incident, pin cloud.NodeIdentity, rec *types.SuccessorRecord, node platform.SuccessorNode) string {
	switch {
	case node.Name == pin.Node:
		return "the provider named the node being replaced as its successor"
	case !node.CreatedAt.After(rec.RequestedAt):
		return fmt.Sprintf("node %s existed before the request", node.Name)
	case !node.Ready:
		return fmt.Sprintf("node %s is not Ready", node.Name)
	case node.Unschedulable:
		return fmt.Sprintf("node %s is not schedulable", node.Name)
	}
	observer, ok := c.platform.(platform.NodeResourceObserver)
	if !ok {
		return "the platform cannot read node resources"
	}
	old, found, err := observer.NodeResources(ctx, pin.Node)
	if err != nil || !found {
		return fmt.Sprintf("the resources of %s could not be read", pin.Node)
	}
	wanted := 0
	for name, count := range old.Capacity {
		if count <= 0 {
			continue
		}
		wanted++
		if node.Allocatable[name] < count {
			return fmt.Sprintf("node %s has %d allocatable %s, fewer than the %d of %s", node.Name, node.Allocatable[name], name, count, pin.Node)
		}
	}
	if wanted == 0 {
		return fmt.Sprintf("%s advertises no accelerator resources to compare against", pin.Node)
	}
	registered, err := c.store.GetNode(ctx, node.Name)
	if err != nil || registered == nil {
		return fmt.Sprintf("no agent has registered node %s yet", node.Name)
	}
	if registered.UID != node.UID || !registered.AgentLastSeen.After(rec.RequestedAt) || len(registered.GPUs) == 0 {
		return fmt.Sprintf("the agent on %s has not reported its accelerators since the request", node.Name)
	}
	return ""
}

// endSuccessor closes the operation with an outcome and records how long two
// nodes ran.
func (c *Controller) endSuccessor(ctx context.Context, inc *types.Incident, outcome, reason string) error {
	rec := inc.Successor.Clone()
	if rec == nil || !rec.Open() {
		return nil
	}
	now := time.Now().UTC()
	rec.Outcome, rec.Reason, rec.EndedAt = outcome, reason, &now
	result := fmt.Sprintf("operation %s ended %s", rec.Operation, outcome)
	if reason != "" {
		result += ": " + reason
	}
	if err := c.saveSuccessor(ctx, inc, rec, "successor-"+outcome, result); err != nil {
		return err
	}
	if rec.ReadyAt != nil {
		metrics.ReplacementOverlapSeconds.WithLabelValues(providerOf(inc), rec.Pool, outcome).Observe(now.Sub(*rec.ReadyAt).Seconds())
	}
	return nil
}

func providerOf(inc *types.Incident) string {
	if inc.CloudIdentity == nil {
		return ""
	}
	scheme, _, ok := strings.Cut(inc.CloudIdentity.ProviderID, "://")
	if !ok {
		return ""
	}
	return scheme
}

// releaseSuccessor removes the successor of an abandoned replacement. A
// created successor is cordoned first and removed only if no workload other
// than DaemonSet and mirror pods runs on it; otherwise it is returned to
// service and left for an operator. Work that landed on it is never evicted
// to clean up. An error leaves the operation open, to be retried.
func (c *Controller) releaseSuccessor(ctx context.Context, inc *types.Incident, why string) error {
	rec := inc.Successor.Clone()
	if !rec.Open() {
		return nil
	}
	if rec.Phase == "" || rec.Phase == cloud.SuccessorPlanned && rec.RequestedAt.IsZero() {
		// Nothing was asked of the cloud: the request time is recorded
		// before the request.
		return c.endSuccessor(ctx, inc, types.SuccessorNotCreated, why)
	}
	plat, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok {
		return fmt.Errorf("the platform cannot release a successor")
	}
	owner := successorReleaseOwnerPrefix + inc.ID
	cordoned := ""
	if rec.Phase == cloud.SuccessorCreated {
		node, err := plat.SuccessorNode(ctx, rec.ProviderID)
		if err != nil {
			return err
		}
		if node.Found {
			if err := c.platform.CordonForOwner(ctx, node.Name, owner, "kubeneuron: removing an unused replacement"); err != nil {
				return err
			}
			cordoned = node.Name
			workloads, err := c.platform.NodeWorkloads(ctx, node.Name)
			if err != nil {
				return err
			}
			if busy := runningWork(workloads); busy != "" {
				if _, _, err := c.platform.ReleaseCordonOwners(ctx, node.Name, []string{owner}); err != nil {
					return err
				}
				return c.endSuccessor(ctx, inc, types.SuccessorHandedOver,
					fmt.Sprintf("%s; the pool is one node larger: successor %s runs %s and was not removed", why, node.Name, busy))
			}
		}
	}
	err := plat.ReleaseSuccessor(ctx, rec.Successor)
	if errors.Is(err, cloud.ErrNeedsOperator) {
		if cordoned != "" {
			if _, _, rerr := c.platform.ReleaseCordonOwners(ctx, cordoned, []string{owner}); rerr != nil {
				return rerr
			}
		}
		return c.endSuccessor(ctx, inc, types.SuccessorHandedOver, why+"; "+err.Error())
	}
	if err != nil {
		return err
	}
	if rec.Phase == cloud.SuccessorCreated {
		return c.endSuccessor(ctx, inc, types.SuccessorReleased, why)
	}
	return c.endSuccessor(ctx, inc, types.SuccessorNotCreated, why)
}

// runningWork names a workload on the node that is neither infrastructure nor
// finished, or "".
func runningWork(workloads []platform.Workload) string {
	for _, w := range workloads {
		switch w.DrainExclusion {
		case platform.DrainExclusionTerminal, platform.DrainExclusionInfrastructure:
			continue
		}
		return "workload " + w.Namespace + "/" + w.Name
	}
	return ""
}

// sweepSuccessors settles open successor operations nobody is waiting on any
// more: the incident ended or moved to another attempt, or it waits outside
// a step while its successor is still not ready past the timeout. While the
// node being replaced is gone, nothing is removed: the successor may be all
// that is left of the pool's capacity.
func (c *Controller) sweepSuccessors(ctx context.Context) {
	open, err := c.store.ListIncidents(ctx, store.IncidentFilter{OpenSuccessor: true})
	if err != nil {
		c.log.Error("listing open successor operations failed", "err", err)
		return
	}
	for _, inc := range open {
		if c.isInFlight(inc.ID) {
			continue
		}
		why := c.successorAbandoned(ctx, inc)
		if why == "" {
			continue
		}
		if presence, ok := c.platform.(platform.NodePresence); ok && inc.CloudIdentity != nil {
			exists, err := presence.NodeExists(ctx, inc.CloudIdentity.Node)
			if err != nil {
				continue
			}
			if !exists {
				if err := c.endSuccessor(ctx, inc, types.SuccessorHandedOver,
					why+"; the node it was to replace is gone, so the successor is kept"); err != nil {
					c.log.Error("recording a kept successor failed", "incident", inc.ID, "err", err)
				}
				continue
			}
		}
		if err := c.releaseSuccessor(ctx, inc, why); err != nil {
			c.log.Warn("removing an abandoned successor failed; retrying", "incident", inc.ID, "operation", inc.Successor.Operation, "err", err)
		}
	}
}

// successorAbandoned says why the incident's open successor is no longer
// wanted, or "".
func (c *Controller) successorAbandoned(ctx context.Context, inc *types.Incident) string {
	s := inc.Successor
	switch {
	case inc.State.Halted():
		return fmt.Sprintf("the incident ended %s before the replacement finished", inc.State)
	case s.Operation != successorOperation(inc):
		return "the incident moved on to another attempt"
	case inc.State == types.StateExecuting:
		return ""
	case s.ReadyAt == nil && !s.RequestedAt.IsZero():
		timeout := recoverymode.EffectiveSuccessorTimeout(c.incidentPolicy(ctx, inc).SuccessorTimeout)
		if time.Since(s.RequestedAt) > timeout {
			return fmt.Sprintf("no replacement was ready within %s", timeout)
		}
	}
	return ""
}
