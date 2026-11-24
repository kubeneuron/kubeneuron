// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/impact"
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

// successorSweepTimeout bounds the cloud and cluster calls that release one
// abandoned successor from the reconcile pass.
const successorSweepTimeout = 2 * time.Minute

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
func (c *Controller) addFirstHold(ctx context.Context, inc *types.Incident, step *playbook.Step) (string, string) {
	if successorReady(inc) {
		return "", ""
	}
	if step.Action == "platform.replace_node" {
		// A simulation prepares no successor; it replaces wherever the
		// live run could have prepared one.
		if c.simulating(ctx, inc) && c.addFirstAvailable(ctx, inc) == nil {
			return "", ""
		}
		return "recovery unavailable: no prepared replacement is ready for this node, and the policy does not name remove-first", ""
	}
	if !c.successorStep(ctx, inc, step) {
		return "recovery unavailable: " + addFirstUnavailable + "; the node is not cordoned or drained", ""
	}
	if err := c.addFirstAvailable(ctx, inc); err != nil {
		reason := addFirstReason(err)
		var action string
		var refusal *cloud.PoolRefusal
		switch {
		case errors.As(err, &refusal):
			reason = refusal.Reason
			action = poolRefusalAction(refusal)
		case errors.Is(err, cloud.ErrAddFirstUnavailable):
			action = "Make this pool eligible for add-first, or set RemoveFirst in the remediation policy that applies to these nodes."
		}
		return "recovery unavailable: " + reason + "; the node is not cordoned or drained", action
	}
	return "", ""
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
		return fmt.Errorf("%w: %s", cloud.ErrAddFirstUnavailable, c.missingCloudIdentityReason(inc.ID))
	}
	if s := inc.Successor; s.Open() && s.Operation == successorOperation(inc) && s.Phase != "" {
		return nil // already under way; the provider settles it from here
	}
	if _, _, err := c.expectedAccelerators(ctx, inc); err != nil {
		return err
	}
	zone := c.incidentPolicy(ctx, inc).SuccessorZone
	key := successorOperation(inc)
	if zone == recoverymode.AnyPoolZone {
		key += "/" + string(zone)
	}
	c.addFirst.mu.Lock()
	entry, cached := c.addFirst.entries[key]
	c.addFirst.mu.Unlock()
	if cached && time.Since(entry.at) < addFirstCheckTTL {
		return entry.err
	}
	var err error
	if aware, ok := plat.(interface {
		AddFirstAvailableInZone(context.Context, cloud.NodeIdentity, recoverymode.SuccessorZone, *[]string) error
	}); ok {
		err = aware.AddFirstAvailableInZone(ctx, *inc.CloudIdentity, zone, nil)
	} else {
		err = plat.AddFirstAvailable(ctx, *inc.CloudIdentity)
	}
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

// removeFirstHold is why a remove-first replacement must wait before the
// node is disrupted, or "". The provider's answer is reused for a minute per
// attempt, as for add-first.
func (c *Controller) removeFirstHold(ctx context.Context, inc *types.Incident, step *playbook.Step) string {
	plat, ok := c.platform.(platform.RemoveFirstChecker)
	if !ok || inc.CloudIdentity == nil {
		return ""
	}
	key := successorOperation(inc)
	c.removeFirst.mu.Lock()
	entry, cached := c.removeFirst.entries[key]
	c.removeFirst.mu.Unlock()
	err := entry.err
	if !cached || time.Since(entry.at) >= addFirstCheckTTL {
		err = plat.RemoveFirstAvailable(ctx, *inc.CloudIdentity)
		c.removeFirst.mu.Lock()
		if c.removeFirst.entries == nil {
			c.removeFirst.entries = map[string]addFirstCheck{}
		}
		for k, e := range c.removeFirst.entries {
			if time.Since(e.at) >= addFirstCheckTTL {
				delete(c.removeFirst.entries, k)
			}
		}
		c.removeFirst.entries[key] = addFirstCheck{at: time.Now(), err: err}
		c.removeFirst.mu.Unlock()
	}
	if err == nil {
		return ""
	}
	// Normally this answers before the cordon; a pool that changed since is
	// refused at the replacement, which removes nothing.
	suffix := "; the node is not cordoned or drained"
	if step.Action == "platform.replace_node" {
		suffix = "; nothing is removed"
	}
	if errors.Is(err, cloud.ErrRemoveFirstUnavailable) {
		return "recovery unavailable: " + err.Error() + suffix
	}
	return "recovery unavailable: whether the node can be replaced by removing it first could not be read: " + err.Error() + suffix
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
	expected, gpus, err := c.expectedAccelerators(ctx, inc)
	if err != nil {
		return err
	}
	pin := *inc.CloudIdentity
	request := cloud.Successor{Operation: successorOperation(inc), Instance: pin.InstanceID}
	if c.incidentPolicy(ctx, inc).SuccessorZone == recoverymode.AnyPoolZone {
		request.State = map[string]string{"successor_zone": string(recoverymode.AnyPoolZone)}
	}
	planned, err := plat.PrepareSuccessor(ctx, pin, request)
	if err != nil {
		return err
	}
	if planned.Phase != cloud.SuccessorPlanned || planned.Operation != successorOperation(inc) || planned.Instance != pin.InstanceID {
		return fmt.Errorf("the provider returned phase %q for operation %q when asked to plan", planned.Phase, planned.Operation)
	}
	rec := &types.SuccessorRecord{Successor: planned, Expected: expected, ExpectedGPUs: gpus}
	if request.State["successor_zone"] == string(recoverymode.AnyPoolZone) {
		old, err := plat.SuccessorNode(ctx, pin.ProviderID)
		if err != nil {
			return err
		}
		if old.Found && old.UID == pin.NodeUID {
			rec.OriginalZone = old.Zone
			reader, supported := c.platform.(interface {
				NodeZoneConstraints(context.Context, string, string) (int, int, error)
			})
			if supported && old.Zone != "" && strings.Contains(planned.State["pool_zones"], ",") {
				budgetCtx, cancel := context.WithTimeout(ctx, impact.DefaultBudget)
				pods, volumes, readErr := reader.NodeZoneConstraints(budgetCtx, pin.Node, old.Zone)
				cancel()
				if readErr == nil {
					rec.ZoneConstrainedPods, rec.ZoneConstrainedVolumes = pods, volumes
				} else {
					rec.ZoneConstraintsUnreadable = true
					c.log.Warn("zone constraints unavailable", "incident", inc.ID)
				}
			}
		}
	}
	return c.saveSuccessor(ctx, inc, rec, "successor-planned",
		fmt.Sprintf("operation %s: a successor of %s is planned in pool %s", planned.Operation, inc.Target.Node, planned.Pool))
}

// expectedAccelerators is what a successor of the incident's node must offer:
// each accelerator resource of the node at the larger of its capacity and
// allocatable count, and the number of accelerators its agent last reported.
// A node with a single accelerator resource takes the agent's count for it
// when that is larger, as after a device fell off the bus and the device
// plugin stopped counting it. With nothing to compare against, add-first is
// unavailable: no successor could be accepted.
func (c *Controller) expectedAccelerators(ctx context.Context, inc *types.Incident) (map[string]int64, int, error) {
	node := inc.Target.Node
	observer, ok := c.platform.(platform.NodeResourceObserver)
	if !ok {
		return nil, 0, fmt.Errorf("%w: the platform cannot read node resources", cloud.ErrAddFirstUnavailable)
	}
	res, found, err := observer.NodeResources(ctx, node)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the resources of %s: %w", node, err)
	}
	if !found {
		return nil, 0, fmt.Errorf("%w: node %s is not in the cluster", cloud.ErrAddFirstUnavailable, node)
	}
	gpus := 0
	if registered, err := c.store.GetNode(ctx, node); err == nil && registered != nil {
		gpus = len(registered.GPUs)
	}
	seen := map[string]int64{}
	for _, counts := range []map[string]int64{res.Capacity, res.Allocatable} {
		for name, n := range counts {
			seen[name] = max(seen[name], n)
		}
	}
	if len(seen) == 1 {
		for name := range seen {
			seen[name] = max(seen[name], int64(gpus))
		}
	}
	expected := map[string]int64{}
	for name, n := range seen {
		if n > 0 {
			expected[name] = n
		}
	}
	if len(expected) == 0 && gpus == 0 {
		return nil, 0, fmt.Errorf("%w: %s advertises no accelerator resources and its agent reported none, so no replacement could be checked against it",
			cloud.ErrAddFirstUnavailable, node)
	}
	return expected, gpus, nil
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
			if successorStops(ctx, err) {
				return err
			}
			// A failed cloud or cluster read is retried until the timeout:
			// the request may already have been accepted.
			c.log.Warn("add-first replacement: a step of the successor operation failed; retrying",
				"incident", inc.ID, "operation", rec.Operation, "err", err)
			if reason := "retrying after: " + err.Error(); inc.Successor.Open() && inc.Successor.Reason != reason {
				retry := inc.Successor.Clone()
				retry.Reason = reason
				if serr := c.saveSuccessor(ctx, inc, retry, "successor-retrying", fmt.Sprintf("operation %s: %s", rec.Operation, reason)); serr != nil {
					c.log.Warn("recording a successor retry failed", "incident", inc.ID, "err", serr)
				}
			}
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

// successorStops reports whether err ends the wait for a successor rather
// than being retried on the next poll.
func successorStops(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, cloud.ErrReplacementCapacity) ||
		errors.Is(err, cloud.ErrNeedsOperator) ||
		errors.Is(err, cloud.ErrAddFirstUnavailable)
}

// advanceSuccessor takes one step of the operation and records it.
func (c *Controller) advanceSuccessor(ctx context.Context, inc *types.Incident, plat platform.SuccessorNodeRecycler, pin cloud.NodeIdentity, rec *types.SuccessorRecord) error {
	if rec.Phase == cloud.SuccessorCreated {
		return c.acceptSuccessor(ctx, inc, plat, pin, rec)
	}
	if rec.UnrequestedPlan() {
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
	rec.SuccessorZone = node.Zone
	// Without the old node's device inventory, no device of the successor's was checked either.
	ready := "is Ready with its accelerators;"
	if rec.ExpectedGPUs == 0 {
		ready = "is Ready and advertises the same accelerator capacity. Device health on it was not checked;"
	}
	placement := successorPlacement(rec, inc.Target.Node)
	if placement != "" {
		placement = "." + placement
	}
	return c.saveSuccessor(ctx, inc, rec, "successor-ready",
		fmt.Sprintf("operation %s: successor %s %s two nodes run until %s is removed", rec.Operation, node.Name, ready, inc.Target.Node)+placement)
}

// successorPlacement uses the node labels captured before the original node was removed.
func successorPlacement(rec *types.SuccessorRecord, original string) string {
	if rec.SuccessorZone == "" || rec.OriginalZone == "" {
		return ""
	}
	if rec.SuccessorZone == rec.OriginalZone {
		return fmt.Sprintf(" Successor %s is in zone %s, the same as %s.", rec.Node, rec.SuccessorZone, original)
	}
	return fmt.Sprintf(" Successor %s is in zone %s; %s was in zone %s (policy allows any zone of the pool).", rec.Node, rec.SuccessorZone, original, rec.OriginalZone)
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
	if len(rec.Expected) == 0 && rec.ExpectedGPUs == 0 {
		return fmt.Sprintf("no accelerators of %s were recorded to compare against", pin.Node)
	}
	for name, count := range rec.Expected {
		if node.Allocatable[name] < count {
			return fmt.Sprintf("node %s has %d allocatable %s, fewer than the %d of %s", node.Name, node.Allocatable[name], name, count, pin.Node)
		}
	}
	if len(rec.Expected) == 0 {
		// Only the agent's count is on record: the successor must still make
		// that many devices allocatable, under whatever resource name.
		var allocatable int64
		for _, n := range node.Allocatable {
			allocatable += max(n, 0)
		}
		if allocatable < int64(rec.ExpectedGPUs) {
			return fmt.Sprintf("node %s has %d allocatable accelerator resources, fewer than the %d accelerators the agent of %s reported", node.Name, allocatable, rec.ExpectedGPUs, pin.Node)
		}
	}
	registered, err := c.store.GetNode(ctx, node.Name)
	if err != nil || registered == nil {
		return fmt.Sprintf("no agent has registered node %s yet", node.Name)
	}
	if registered.UID != node.UID || !registered.AgentLastSeen.After(rec.RequestedAt) {
		return fmt.Sprintf("the agent on %s has not registered since the request", node.Name)
	}
	// An agent that saw no device on the old node (no inventory for this
	// vendor) is not asked for one on the successor: allocatable and a fresh
	// registration are the whole check then.
	if rec.ExpectedGPUs > 0 && len(registered.GPUs) == 0 {
		return fmt.Sprintf("the agent on %s has not reported its accelerators since the request", node.Name)
	}
	if len(registered.GPUs) < rec.ExpectedGPUs {
		return fmt.Sprintf("the agent on %s reports %d accelerators, fewer than the %d of %s", node.Name, len(registered.GPUs), rec.ExpectedGPUs, pin.Node)
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
	if rec.Phase == "" || rec.UnrequestedPlan() {
		// Nothing was asked of the cloud: the request time is recorded
		// before the request.
		return c.endSuccessor(ctx, inc, types.SuccessorNotCreated, why)
	}
	plat, ok := c.platform.(platform.SuccessorNodeRecycler)
	if !ok {
		return fmt.Errorf("the platform cannot release a successor")
	}
	if rec.Phase == cloud.SuccessorRequested && inc.CloudIdentity != nil {
		// Poll the existing request before cleanup so a newly visible node goes
		// through the same persisted identity and workload checks as any other.
		next, err := plat.PrepareSuccessor(ctx, *inc.CloudIdentity, rec.Successor)
		if err == nil {
			if next.Operation != rec.Operation || next.Instance != rec.Instance || next.Pool != rec.Pool ||
				(next.Phase != cloud.SuccessorRequested && next.Phase != cloud.SuccessorCreated) ||
				(next.Phase == cloud.SuccessorCreated && next.ProviderID == "") {
				return c.endSuccessor(ctx, inc, types.SuccessorHandedOver, why+"; cannot confirm successor identity during cleanup")
			}
			if next.Phase == cloud.SuccessorCreated {
				rec.Successor = next
				if err := c.saveSuccessor(ctx, inc, rec, "successor-created", "identified successor before cleanup: "+next.ProviderID); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, cloud.ErrReplacementCapacity) && !errors.Is(err, cloud.ErrNeedsOperator) {
			return err
		}
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
				reason := fmt.Sprintf("Replacement node %s in pool %s is running %s, so KubeNeuron did not remove it. Pool %s may now have one node more than planned, and that node is billed. Check its workloads and the size of the pool, and remove the extra node when it is safe.", node.Name, rec.Pool, busy, rec.Pool)
				return c.handOverSuccessorCleanup(ctx, inc, reason)
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
		reason := fmt.Sprintf("KubeNeuron could not confirm that the replacement node for %s was removed from pool %s. The pool may have one node more than planned, and that node is billed. Check which machines remain in pool %s and its size.", inc.Target.Node, rec.Pool, rec.Pool)
		return c.handOverSuccessorCleanup(ctx, inc, reason)
	}
	if err != nil {
		return err
	}
	if rec.Phase == cloud.SuccessorCreated {
		return c.endSuccessor(ctx, inc, types.SuccessorReleased, why)
	}
	return c.endSuccessor(ctx, inc, types.SuccessorNotCreated, why)
}

func (c *Controller) handOverSuccessorCleanup(ctx context.Context, inc *types.Incident, reason string) error {
	reason += fmt.Sprintf(` Then run kubeneuronctl resolve %s --cloud-operation-settled --reason "<what you found>"`, inc.ID)
	current := inc.Successor.Operation == successorOperation(inc)
	if err := c.endSuccessor(ctx, inc, types.SuccessorHandedOver, reason); err != nil {
		return err
	}
	if current && !inc.State.Halted() {
		return c.quarantineAfterStep(ctx, inc, reason, false)
	}
	return nil
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
		c.sweepSuccessor(ctx, inc, why)
	}
}

// sweepSuccessor releases one abandoned successor within a bounded time, so a
// cloud API that does not answer cannot stall the reconcile pass.
func (c *Controller) sweepSuccessor(ctx context.Context, inc *types.Incident, why string) {
	ctx, cancel := context.WithTimeout(ctx, successorSweepTimeout)
	defer cancel()
	if presence, ok := c.platform.(platform.NodePresence); ok && inc.CloudIdentity != nil {
		exists, err := presence.NodeExists(ctx, inc.CloudIdentity.Node)
		if err != nil {
			return
		}
		if !exists {
			if err := c.endSuccessor(ctx, inc, types.SuccessorHandedOver,
				why+"; the node it was to replace is gone, so the successor is kept"); err != nil {
				c.log.Error("recording a kept successor failed", "incident", inc.ID, "err", err)
			}
			return
		}
	}
	if err := c.releaseSuccessor(ctx, inc, why); err != nil {
		c.log.Warn("removing an abandoned successor failed; retrying", "incident", inc.ID, "operation", inc.Successor.Operation, "err", err)
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
