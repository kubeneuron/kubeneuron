// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/interruption"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/cloud"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Audit actions of a cloud-interruption incident.
const (
	// InterruptionAction records one notice the cloud announced: its kind,
	// deadline, source and key. The cloud's message text is never recorded.
	InterruptionAction = "interruption"
	// interruptionWarnedAction records the checkpoint warning sent: how many
	// workloads were asked and the deadline they were given.
	interruptionWarnedAction = "interruption-warned"
	// interruptionOutcomeAction records how the warned workloads answered by
	// the deadline.
	interruptionOutcomeAction = "interruption-outcome"
	// interruptionEndedAction closes an incident whose notice is gone from a
	// node that is still there.
	interruptionEndedAction = "interruption-ended"
	// interruptionNoticeEndedAction records that the cloud reported one
	// queued notice over; the incident closes once all of them are.
	interruptionNoticeEndedAction = "interruption-notice-ended"
)

const (
	// interruptionMargin is kept between the deadline a workload is given and
	// the cloud's, so a checkpoint finished at its deadline is written before
	// the instance stops.
	interruptionMargin = 30 * time.Second
	// interruptionOverdue is how long after its deadline a cloud-interruption
	// incident may wait for the cloud. A node that is still there with the
	// notice still on it is handed to a person, cordon kept: the signal may
	// be stuck.
	interruptionOverdue = time.Hour
)

// interruptionReader is the platform side: the interruptions announced on
// the cluster's nodes.
type interruptionReader interface {
	NodeInterruptions(ctx context.Context) ([]interruption.Notice, error)
}

// cloudInterruptionQueue is the platform side of a cloud provider that reads
// announced interruptions from a queue (cloud.InterruptionQueue): it delivers
// each notice once, and deletes it only when acknowledged.
type cloudInterruptionQueue interface {
	CloudInterruptionQueueConfigured() bool
	CloudInterruptions(ctx context.Context) ([]interruption.Notice, error)
	ReceiveCloudInterruptions(ctx context.Context) (bool, error)
	AckCloudInterruption(ctx context.Context, key string) error
}

// interruptionQueueRetry is how long the receive loop waits after a failed
// receive.
const interruptionQueueRetry = 10 * time.Second

// receiveCloudInterruptions long-polls the provider's interruption queue
// until ctx ends, which it does when this controller stops leading, and wakes
// the interruption pass as soon as a notice arrives.
func (c *Controller) receiveCloudInterruptions(ctx context.Context, q cloudInterruptionQueue) {
	for ctx.Err() == nil {
		fresh, err := q.ReceiveCloudInterruptions(ctx)
		if fresh {
			select {
			case c.interruptionWake <- struct{}{}:
			default:
			}
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if c.logOnce.first("interruption-queue/" + err.Error()) {
			c.log.Warn("could not read the cloud's interruption queue", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interruptionQueueRetry):
		}
	}
}

// interruptionState is what the poller remembers between passes: the keys it
// has recorded, so a notice read on every pass is recorded once. A key lost
// to a restart is harmless: the incident's trail is checked before a notice
// is recorded again.
type interruptionState struct {
	mu   sync.Mutex
	keys map[string]bool
	// settled are the open incidents whose warning outcome is recorded, or
	// whose warning asked nobody, so their trail is not read again.
	settled map[string]bool
}

// interruptionNotice is a recorded notice, as the audit row keeps it.
type interruptionNotice struct {
	Kind     cloud.InterruptionKind `json:"kind"`
	Deadline time.Time              `json:"deadline,omitempty"`
	Source   string                 `json:"source"`
	Key      string                 `json:"key"`
	// SeenAt is when KubeNeuron first read the notice; a notice without a
	// deadline is timed from it.
	SeenAt time.Time `json:"seen_at"`
	// Once marks a notice from a source that delivers it once: the incident
	// does not end when it is no longer read.
	Once bool `json:"once,omitempty"`
	// At is when the cloud sent the notice, when its source says.
	At time.Time `json:"at,omitempty"`
}

// due is when the cloud acts, as far as KubeNeuron can tell.
func (n interruptionNotice) due() time.Time {
	if !n.Deadline.IsZero() {
		return n.Deadline
	}
	return n.SeenAt.Add(interruption.AssumedNotice(n.Source))
}

// provider names the cloud of a notice's source, for the metric.
func (n interruptionNotice) provider() string {
	switch {
	case strings.HasPrefix(n.Source, "gke:"):
		return "gcp"
	case strings.HasPrefix(n.Source, "aks:"):
		return "azure"
	case strings.HasPrefix(n.Source, "aws-node-termination-handler:"), strings.HasPrefix(n.Source, "aws-eventbridge:"):
		return "aws"
	}
	return "unknown"
}

// pollInterruptions reads the announced interruptions and records each new
// one: it opens a cloud-interruption incident for the node, or adds the notice
// to the one already open. Incidents whose notices are all gone are closed,
// unless a notice came from a queue, which delivers it once; those close when
// the queue says the interruption ended. A queued notice is acknowledged only
// once it is recorded, so one that could not be is delivered again. A failed
// read changes nothing.
func (c *Controller) pollInterruptions(ctx context.Context) {
	reader, ok := c.platform.(interruptionReader)
	if !ok {
		return
	}
	notices, err := reader.NodeInterruptions(ctx)
	if err != nil {
		c.log.Warn("could not read announced interruptions", "err", err)
		return
	}
	queue, queued := c.platform.(cloudInterruptionQueue)
	if queued && queue.CloudInterruptionQueueConfigured() {
		fromQueue, err := queue.CloudInterruptions(ctx)
		if err != nil && c.logOnce.first("interruption-queue-read/"+err.Error()) {
			c.log.Warn("could not read the notices of the cloud's interruption queue", "err", err)
		}
		notices = append(notices, fromQueue...)
	} else {
		queued = false
	}
	ack := func(n interruption.Notice) {
		if !queued || !n.Once {
			return
		}
		if err := queue.AckCloudInterruption(ctx, n.Key); err != nil && c.logOnce.first("interruption-ack/"+n.Key+"/"+err.Error()) {
			c.log.Warn("could not acknowledge a notice on the cloud's interruption queue", "node", n.Node, "source", n.Source, "err", err)
		}
	}
	current := map[string]bool{}
	recorded := map[string]bool{}
	// ended are the queue's reports that an interruption is over, by node.
	ended := map[string][]interruption.Notice{}
	for _, n := range notices {
		if n.Ended {
			ended[n.Node] = append(ended[n.Node], n)
			continue
		}
		if _, err := c.store.GetNode(ctx, n.Node); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				// The store could not say: the notice stays in force, so
				// the node's incident is not closed on a failed read.
				current[n.Node] = true
				recorded[n.Key] = c.interruptions.seen(n.Key)
				if c.logOnce.first("interruption-node/" + n.Node + "/" + err.Error()) {
					c.log.Warn("could not read the node of an announced interruption", "node", n.Node, "err", err)
				}
			}
			// Otherwise not a node KubeNeuron manages.
			ack(n)
			continue
		}
		current[n.Node] = true
		// A queued notice is read again only when it was delivered again or
		// changed, so it is checked against the trail every time.
		if !n.Once && c.interruptions.seen(n.Key) {
			recorded[n.Key] = true
			ack(n)
			continue
		}
		if err := c.recordInterruption(ctx, n); err != nil {
			// Retried on every pass; logged once per notice and error.
			if c.logOnce.first("interruption-record/" + n.Key + "/" + err.Error()) {
				c.log.Warn("could not record an announced interruption", "node", n.Node, "source", n.Source, "err", err)
			}
			continue
		}
		recorded[n.Key] = true
		ack(n)
	}
	c.interruptions.keep(recorded)
	for node, reports := range ended {
		if err := c.endNotices(ctx, node, reports); err != nil {
			if c.logOnce.first("interruption-end/" + node + "/" + err.Error()) {
				c.log.Warn("could not record the end of an announced interruption", "node", node, "err", err)
			}
			continue
		}
		for _, n := range reports {
			ack(n)
		}
	}
	incidents, err := c.openInterruptionIncidents(ctx)
	if err != nil {
		c.log.Warn("could not list the open cloud-interruption incidents", "err", err)
		return
	}
	c.endInterruptions(ctx, incidents, current)
	c.observeInterruptionWarnings(ctx, incidents)
}

// endNotices records, on the node's open cloud-interruption incident, which
// of its queued notices the cloud's end reports close. A report with a kind
// (AWS Health closing its event) ends the notice of the same key. One without
// (the instance is running again) ends the maintenance notices sent before
// it: a queue does not keep order, so an older report ends nothing newer, and
// a spot reclaim or rebalance hint ends only with the node. A report that
// matches nothing changes nothing.
func (c *Controller) endNotices(ctx context.Context, node string, reports []interruption.Notice) error {
	inc, err := c.openIncidentFor(ctx, types.Target{Node: node}, types.ClassCloudInterruption)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	notices, closed, err := c.interruptionRecord(ctx, inc)
	if err != nil {
		return err
	}
	for _, r := range reports {
		for _, n := range notices {
			if !n.Once || closed[n.Key] || !reportEnds(r, n) {
				continue
			}
			if err := c.store.AppendAudit(ctx, &types.AuditEntry{
				IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: interruptionNoticeEndedAction,
				FromState: inc.State, ToState: inc.State, DryRun: inc.DryRun,
				Result: fmt.Sprintf("the cloud reported the %s from %s over (%s)", n.Kind, n.Source, r.Source),
				Params: map[string]string{"key": n.Key},
			}); err != nil {
				return err
			}
			closed[n.Key] = true
		}
	}
	return nil
}

// reportEnds reports whether the end report r closes the recorded notice n.
func reportEnds(r interruption.Notice, n interruptionNotice) bool {
	if r.Kind != "" {
		return r.Key == n.Key
	}
	if n.Kind != cloud.InterruptionScheduledMaintenance && n.Kind != cloud.InterruptionHostMaintenance {
		return false
	}
	return !n.At.IsZero() && !r.At.IsZero() && n.At.Before(r.At)
}

// openInterruptionIncidents are the open cloud-interruption incidents, read
// once per pass.
func (c *Controller) openInterruptionIncidents(ctx context.Context) ([]*types.Incident, error) {
	all, err := c.store.ListIncidents(ctx, store.IncidentFilter{States: openStates})
	if err != nil {
		return nil, err
	}
	var out []*types.Incident
	for _, inc := range all {
		if inc.Class == types.ClassCloudInterruption {
			out = append(out, inc)
		}
	}
	return out, nil
}

func (s *interruptionState) seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

// keep replaces the remembered keys with the ones announced now, so the set
// is never larger than the notices in force.
func (s *interruptionState) keep(keys map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = keys
}

func (s *interruptionState) isSettled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled[id]
}

// settle remembers that incident id's warning needs no more reading. open
// are the open incidents; any other remembered one is forgotten.
func (s *interruptionState) settle(id string, open []*types.Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]bool{}
	for _, inc := range open {
		if s.settled[inc.ID] || inc.ID == id {
			next[inc.ID] = true
		}
	}
	s.settled = next
}

// recordInterruption opens the node's cloud-interruption incident, or finds
// the open one, and appends the notice to it unless it is already there.
func (c *Controller) recordInterruption(ctx context.Context, n interruption.Notice) error {
	target := types.Target{Node: n.Node}
	if n.Once {
		// A queued notice read again is a redelivery or the cloud's update
		// of it: only a moved deadline is recorded, and the incident's
		// signal is not counted again.
		done, err := c.updateQueuedNotice(ctx, target, n)
		if err != nil || done {
			return err
		}
	}
	sig := types.Signal{
		Target: target, Class: types.ClassCloudInterruption, Severity: types.SeverityWarning,
		Source: types.SourceCloudNotice, ObservedAt: time.Now(),
		Evidence: map[string]string{"kind": string(n.Kind), "source": n.Source},
	}
	if err := c.IngestSignal(ctx, sig); err != nil {
		return err
	}
	inc, err := c.openIncidentFor(ctx, target, types.ClassCloudInterruption)
	if err != nil {
		return err
	}
	recorded, err := c.interruptionNotices(ctx, inc)
	if err != nil {
		return err
	}
	for _, r := range recorded {
		if r.Key == n.Key {
			return nil
		}
	}
	notice := interruptionNotice{Kind: n.Kind, Deadline: n.Deadline, Source: n.Source, Key: n.Key, SeenAt: c.checkpointNowFunc()().UTC(), Once: n.Once, At: n.At}
	if err := c.appendNotice(ctx, inc, notice, interruptionSummary(notice)); err != nil {
		return err
	}
	metrics.CloudInterruptions.WithLabelValues(notice.provider(), string(notice.Kind), "announced").Inc()
	c.noteInterruptionOnOtherIncidents(ctx, inc, notice)
	return nil
}

// updateQueuedNotice handles a queued notice already recorded on the node's
// open incident: a moved deadline is recorded as the notice's new row, which
// supersedes the old one. done is false when the notice is not recorded yet.
func (c *Controller) updateQueuedNotice(ctx context.Context, target types.Target, n interruption.Notice) (bool, error) {
	inc, err := c.openIncidentFor(ctx, target, types.ClassCloudInterruption)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	notices, _, err := c.interruptionRecord(ctx, inc)
	if err != nil {
		return false, err
	}
	for _, r := range notices {
		if r.Key != n.Key {
			continue
		}
		if r.Deadline.Equal(n.Deadline) {
			return true, nil
		}
		r.Deadline, r.At = n.Deadline, n.At
		when := "no time"
		if !r.Deadline.IsZero() {
			when = r.Deadline.UTC().Format(time.RFC3339)
		}
		return true, c.appendNotice(ctx, inc, r, fmt.Sprintf("the cloud moved the deadline of the %s read from %s to %s", r.Kind, r.Source, when))
	}
	return false, nil
}

func (c *Controller) appendNotice(ctx context.Context, inc *types.Incident, n interruptionNotice, result string) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: InterruptionAction,
		FromState: inc.State, ToState: inc.State, DryRun: inc.DryRun,
		Result: result, Params: map[string]string{"interruption": string(data)},
	})
}

func interruptionSummary(n interruptionNotice) string {
	when := "no time given; treated as imminent"
	if !n.Deadline.IsZero() {
		when = "deadline " + n.Deadline.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("the cloud announced %s (%s), read from %s", n.Kind, when, n.Source)
}

func (c *Controller) openIncidentFor(ctx context.Context, target types.Target, class types.ProblemClass) (*types.Incident, error) {
	var inc *types.Incident
	err := c.store.WithTx(ctx, func(tx store.Tx) error {
		var err error
		inc, err = tx.GetOpenIncident(ctx, target, class)
		return err
	})
	return inc, err
}

// noteInterruptionOnOtherIncidents adds the notice to every other open
// incident of the node, so a person working on it sees that the cloud will
// take the node. Those incidents are not changed otherwise.
func (c *Controller) noteInterruptionOnOtherIncidents(ctx context.Context, inc *types.Incident, n interruptionNotice) {
	others, err := c.otherOpenIncidents(ctx, inc)
	if err != nil {
		c.log.Warn("could not read the node's other incidents", "incident", inc.ID, "err", err)
		return
	}
	for _, other := range others {
		if err := c.appendAudit(ctx, other, "system", InterruptionAction,
			interruptionSummary(n)+"; see incident "+inc.ID); err != nil {
			c.log.Warn("could not note the interruption on another incident", "incident", other.ID, "err", err)
		}
	}
}

// otherOpenIncidents are the node's open incidents other than inc.
func (c *Controller) otherOpenIncidents(ctx context.Context, inc *types.Incident) ([]*types.Incident, error) {
	all, err := c.store.ListIncidents(ctx, store.IncidentFilter{Node: inc.Target.Node, States: openStates})
	if err != nil {
		return nil, err
	}
	var out []*types.Incident
	for _, other := range all {
		if other.ID != inc.ID && other.Class != types.ClassCloudInterruption {
			out = append(out, other)
		}
	}
	return out, nil
}

// openStates are the states of an incident that is not over.
var openStates = []types.IncidentState{
	types.StateOpen, types.StateObserving, types.StateEvaluating,
	types.StateAwaitingApproval, types.StateExecuting, types.StateVerifying,
	types.StateNeedsHuman,
}

// interruptionNotices are the notices recorded on inc, oldest first.
func (c *Controller) interruptionNotices(ctx context.Context, inc *types.Incident) ([]interruptionNotice, error) {
	notices, _, err := c.interruptionRecord(ctx, inc)
	return notices, err
}

// interruptionRecord reads inc's trail: its notices in the order first
// recorded, each as its latest row says (a moved deadline supersedes the
// earlier one), and the keys of the queued notices the cloud reported over.
func (c *Controller) interruptionRecord(ctx context.Context, inc *types.Incident) ([]interruptionNotice, map[string]bool, error) {
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return nil, nil, err
	}
	var out []interruptionNotice
	at := map[string]int{}
	closed := map[string]bool{}
	for _, e := range trail {
		switch {
		case e.Action == interruptionNoticeEndedAction && e.Params["key"] != "":
			closed[e.Params["key"]] = true
		case e.Action == InterruptionAction && e.Params["interruption"] != "":
			var n interruptionNotice
			if err := json.Unmarshal([]byte(e.Params["interruption"]), &n); err != nil {
				continue
			}
			if i, ok := at[n.Key]; ok {
				out[i] = n
				continue
			}
			at[n.Key] = len(out)
			out = append(out, n)
		}
	}
	return out, closed, nil
}

// urgentNotice is the notice that decides the incident's timing: any notice
// that will take the node before a rebalance hint, and among those the one
// due first. ok is false when only rebalance hints are recorded.
func urgentNotice(notices []interruptionNotice) (interruptionNotice, bool) {
	var best interruptionNotice
	found := false
	for _, n := range notices {
		if n.Kind == cloud.InterruptionRebalance {
			continue
		}
		if !found || n.due().Before(best.due()) {
			best, found = n, true
		}
	}
	return best, found
}

// holdInterruptionStep decides, before a cloud-interruption step is
// authorised, whether it waits: while only a rebalance hint is recorded, and
// while the deadline is further away than the policy's lead. A cordon also
// waits while another incident of the node is open; that incident owns the
// node. Holding changes nothing and is logged once.
func (c *Controller) holdInterruptionStep(ctx context.Context, inc *types.Incident, step *playbook.Step) (bool, error) {
	if inc.Class != types.ClassCloudInterruption {
		return false, nil
	}
	notices, err := c.interruptionNotices(ctx, inc)
	if err != nil {
		return true, err
	}
	hold := func(reason string) (bool, error) {
		key := fmt.Sprintf("interruption-hold/%s/%s/%s", inc.ID, step.Name, reason)
		if c.logOnce.first(key) {
			c.log.Info("cloud-interruption step waits", "incident", inc.ID, "step", step.Name, "reason", reason)
		}
		return true, nil
	}
	notice, ok := urgentNotice(notices)
	if !ok {
		return hold("only a rebalance hint is announced; nothing is asked of the workloads and the node is not cordoned")
	}
	lead := interruption.EffectiveLead(c.incidentPolicy(ctx, inc).InterruptionLead)
	if start := notice.due().Add(-lead); time.Now().Before(start) {
		return hold(fmt.Sprintf("the deadline %s is more than %s away; the step starts at %s",
			notice.due().UTC().Format(time.RFC3339), lead, start.UTC().Format(time.RFC3339)))
	}
	if step.Action == "platform.cordon" {
		others, err := c.otherOpenIncidents(ctx, inc)
		if err != nil {
			return true, err
		}
		if len(others) > 0 {
			return hold("incident " + others[0].ID + " of the node is open and owns its cordon")
		}
	}
	return false, nil
}

// warnCheckpoint is the checkpoint warning step: it asks the workloads the
// node's interruption would stop to checkpoint before the deadline the cloud
// announced, less a margin. It only stamps the requests; the answers are read
// by the interruption poller until that deadline. It never fails the step: a
// warning that reached nobody is recorded as such.
func (c *Controller) warnCheckpoint(ctx context.Context, inc *types.Incident) (*types.ActionResult, error) {
	notices, err := c.interruptionNotices(ctx, inc)
	if err != nil {
		return nil, err
	}
	notice, ok := urgentNotice(notices)
	if !ok {
		return okResult(inc, "no interruption that takes the node is announced; nothing was asked"), nil
	}
	now := c.checkpointNowFunc()()
	deadline := notice.due().Add(-interruptionMargin)
	if notice.Deadline.IsZero() && !deadline.After(now) {
		// The notice gives no time and the assumed one leaves no room for
		// the margin: the workloads are asked at once, against the cloud's
		// assumed deadline itself, rather than not at all.
		deadline = notice.due()
	}
	outcome := func(label, text string) (*types.ActionResult, error) {
		metrics.CloudInterruptions.WithLabelValues(notice.provider(), string(notice.Kind), label).Inc()
		c.recordWarning(ctx, inc, notice, now, deadline, 0, text)
		return okResult(inc, text), nil
	}
	if !deadline.After(now) {
		return outcome("missed", "the cloud's deadline leaves no time for a checkpoint; nothing was asked")
	}
	policy := c.runtimeConfig(ctx).Checkpoint
	if !policy.Enabled {
		return outcome("not-asked", "checkpoint coordination is not enabled; no workload was asked")
	}
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		return outcome("not-asked", "platform "+c.platform.Name()+" does not support checkpoint coordination; no workload was asked")
	}
	candidates, listed, err := c.checkpointCandidates(ctx, policy, inc, "drain", true)
	if err != nil {
		c.log.Warn("checkpoint warning: workload listing failed", "incident", inc.ID, "node", inc.Target.Node, "err", err)
		return outcome("not-asked", "the node's workloads could not be listed; no workload was asked")
	}
	if len(candidates) == 0 {
		return outcome("no-jobs", fmt.Sprintf("no workload on %s takes part in checkpoint coordination; %d listed", inc.Target.Node, listed))
	}
	asked := 0
	var names []string
	for _, w := range candidates {
		req := checkpoint.Request{
			IncidentID: inc.ID, RequestedAt: now, DeadlineAt: deadline,
			Reason: inc.Class, NextAction: "cloud." + string(notice.Kind),
		}
		if _, got := c.requestCheckpoint(ctx, checkpointer, policy, inc, inc.Target.Node, w, req); got == "" {
			asked++
			names = append(names, checkpointWorkloadID(w))
		}
	}
	metrics.CloudInterruptions.WithLabelValues(notice.provider(), string(notice.Kind), "warned").Inc()
	ahead := "at the cloud's assumed deadline"
	if margin := notice.due().Sub(deadline); margin > 0 {
		ahead = margin.String() + " ahead of the cloud's deadline"
	}
	text := fmt.Sprintf("asked %d of %d workload(s) %s to checkpoint before %s, %s",
		asked, len(candidates), checkpointJoin(names), deadline.UTC().Format(time.RFC3339), ahead)
	c.recordWarning(ctx, inc, notice, now, deadline, asked, text)
	return okResult(inc, text), nil
}

// interruptionWarning is the warning a step sent, as its audit row keeps it.
type interruptionWarning struct {
	Kind        cloud.InterruptionKind `json:"kind"`
	Source      string                 `json:"source"`
	RequestedAt time.Time              `json:"requested_at"`
	DeadlineAt  time.Time              `json:"deadline_at"`
	Asked       int                    `json:"asked"`
}

func (c *Controller) recordWarning(ctx context.Context, inc *types.Incident, n interruptionNotice, at, deadline time.Time, asked int, text string) {
	data, err := json.Marshal(interruptionWarning{Kind: n.Kind, Source: n.Source, RequestedAt: at, DeadlineAt: deadline, Asked: asked})
	if err != nil {
		return
	}
	if err := c.store.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: time.Now(), Actor: "system", Action: interruptionWarnedAction,
		FromState: inc.State, ToState: inc.State, DryRun: inc.DryRun,
		Result: text, Params: map[string]string{"warning": string(data)},
	}); err != nil {
		c.log.Warn("could not record the checkpoint warning", "incident", inc.ID, "err", err)
	}
}

// observeInterruptionWarnings reads, for each warning whose outcome is not
// recorded yet, how the asked workloads answered. The outcome is recorded once
// every one has answered, or at the deadline. incidents are the pass's open
// cloud-interruption incidents.
func (c *Controller) observeInterruptionWarnings(ctx context.Context, incidents []*types.Incident) {
	checkpointer, ok := c.platform.(platform.WorkloadCheckpointer)
	if !ok {
		return
	}
	policy := c.runtimeConfig(ctx).Checkpoint
	for _, inc := range incidents {
		if c.interruptions.isSettled(inc.ID) {
			continue
		}
		warning, found, done, err := c.pendingWarning(ctx, inc)
		if err != nil || !found {
			continue
		}
		if done || warning.Asked == 0 {
			c.interruptions.settle(inc.ID, incidents)
			continue
		}
		now := c.checkpointNowFunc()()
		provider := interruptionNotice{Source: warning.Source}.provider()
		workloads, err := c.platform.NodeWorkloads(ctx, inc.Target.Node)
		if err != nil {
			if now.Before(warning.DeadlineAt) {
				continue
			}
			// Nothing is said of workloads that were not read.
			metrics.CloudInterruptions.WithLabelValues(provider, string(warning.Kind), "unknown").Inc()
			text := fmt.Sprintf("the workloads on %s could not be read at the deadline %s; how the %d asked workload(s) answered is unknown",
				inc.Target.Node, warning.DeadlineAt.UTC().Format(time.RFC3339), warning.Asked)
			if err := c.appendAudit(ctx, inc, "system", interruptionOutcomeAction, text); err != nil {
				c.log.Warn("could not record the checkpoint warning outcome", "incident", inc.ID, "err", err)
				continue
			}
			c.interruptions.settle(inc.ID, incidents)
			continue
		}
		inForce := checkpoint.Request{IncidentID: inc.ID, DeadlineAt: warning.DeadlineAt}
		counts := map[checkpoint.Outcome]int{}
		pending := 0
		for _, w := range workloads {
			obs := checkpointer.ObserveCheckpoint(ctx, policy, inc.Target.Node, w)
			if req, ok := checkpoint.ParseRequest(obs.Annotations); !obs.Found || !ok || req.IncidentID != inc.ID {
				continue
			}
			switch outcome := checkpoint.Classify(now, inForce, w.Checkpoint(), obs); {
			case outcome.Settled():
				counts[outcome]++
			default:
				pending++
			}
		}
		if pending > 0 && now.Before(warning.DeadlineAt) {
			continue
		}
		answered := counts[checkpoint.OutcomeAcknowledged] + counts[checkpoint.OutcomeExited]
		label := "checkpointed-in-time"
		if answered < warning.Asked {
			label = "missed"
		}
		metrics.CloudInterruptions.WithLabelValues(provider, string(warning.Kind), label).Inc()
		text := fmt.Sprintf("%d of %d asked workload(s) reported a checkpoint or finished before %s", answered, warning.Asked, warning.DeadlineAt.UTC().Format(time.RFC3339))
		if err := c.appendAudit(ctx, inc, "system", interruptionOutcomeAction, text); err != nil {
			c.log.Warn("could not record the checkpoint warning outcome", "incident", inc.ID, "err", err)
			continue
		}
		c.interruptions.settle(inc.ID, incidents)
	}
}

// pendingWarning is the incident's last warning, whether one was sent, and
// whether its outcome is already recorded.
func (c *Controller) pendingWarning(ctx context.Context, inc *types.Incident) (interruptionWarning, bool, bool, error) {
	trail, err := c.store.AuditTrail(ctx, inc.ID)
	if err != nil {
		return interruptionWarning{}, false, false, err
	}
	var w interruptionWarning
	found, done := false, false
	for _, e := range trail {
		switch e.Action {
		case interruptionWarnedAction:
			if err := json.Unmarshal([]byte(e.Params["warning"]), &w); err == nil {
				found, done = true, false
			}
		case interruptionOutcomeAction:
			done = true
		}
	}
	return w, found, done, nil
}

// endInterruptions closes the cloud-interruption incidents of nodes that no
// longer carry any notice and are still there. KubeNeuron cannot tell a
// cancelled notice from maintenance completed in place (a reboot, a frozen
// VM, a VM restarted under the same Node), so the incident ends without
// saying which. A notice from a queue is delivered once, so its absence ends
// nothing: an incident with one closes when the cloud has reported each of
// them over (endNotices), a rebalance hint counting as over once no notice
// that takes the node has followed it for interruptionOverdue. Only the
// incident's own cordon is released. A node that is gone is closed by the
// vanished-node janitor instead. incidents are the pass's open
// cloud-interruption incidents.
func (c *Controller) endInterruptions(ctx context.Context, incidents []*types.Incident, current map[string]bool) {
	now := c.checkpointNowFunc()()
	for _, inc := range incidents {
		node := inc.Target.Node
		if current[node] || c.isInFlight(inc.ID) {
			continue
		}
		notices, closed, err := c.interruptionRecord(ctx, inc)
		if err != nil {
			continue
		}
		queued, open := false, false
		for _, n := range notices {
			if !n.Once {
				continue
			}
			queued = true
			stale := n.Kind == cloud.InterruptionRebalance && now.After(n.SeenAt.Add(interruptionOverdue))
			if !closed[n.Key] && !stale {
				open = true
			}
		}
		if open {
			continue
		}
		if presence, ok := c.platform.(platform.NodePresence); ok {
			// A node that is gone was taken, not spared: that is the
			// vanished-node janitor's to close. One that cannot be read waits.
			if exists, err := presence.NodeExists(ctx, node); err != nil || !exists {
				continue
			}
		}
		if _, _, err := c.platform.ReleaseCordonOwners(ctx, node, []string{inc.ID}); err != nil {
			c.log.Warn("could not release an ended interruption's cordon", "incident", inc.ID, "err", err)
			continue
		}
		if inc.State == types.StateExecuting {
			if err := c.transition(ctx, inc, types.StateEvaluating, "system", interruptionEndedAction,
				"the announced interruption ended while a step was orphaned", nil); err != nil {
				continue
			}
		}
		message := "the notice is no longer present on " + node + ": cancelled or completed; the incident is closed as ended"
		switch {
		case queued && len(closed) > 0:
			message = "the cloud reported each announced interruption of " + node + " over; the incident is closed as ended"
		case queued:
			message = "no notice that takes " + node + " followed the rebalance hint within " + interruptionOverdue.String() + "; the incident is closed as ended"
		}
		if err := c.transition(ctx, inc, types.StateResolved, "system", interruptionEndedAction, message, nil); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				c.log.Warn("closing an ended interruption failed", "incident", inc.ID, "err", err)
			}
			continue
		}
		for _, n := range notices {
			metrics.CloudInterruptions.WithLabelValues(n.provider(), string(n.Kind), "ended").Inc()
		}
	}
}

// overdueInterruption hands a VERIFYING cloud-interruption incident to a
// person when the cloud has not taken the node an hour after the deadline
// although the node is there and the notice is still on it: the signal may be
// stuck, and the cordon is kept rather than released on a guess. It reports
// whether the incident was handed over.
func (c *Controller) overdueInterruption(ctx context.Context, inc *types.Incident) (bool, error) {
	notices, err := c.interruptionNotices(ctx, inc)
	if err != nil {
		return false, err
	}
	notice, ok := urgentNotice(notices)
	if !ok || time.Now().Before(notice.due().Add(interruptionOverdue)) {
		return false, nil
	}
	if presence, ok := c.platform.(platform.NodePresence); ok {
		// A node that is gone, or cannot be read, is the janitors' to close.
		if exists, err := presence.NodeExists(ctx, inc.Target.Node); err != nil || !exists {
			return false, nil
		}
	}
	reason := fmt.Sprintf("the cloud announced %s for %s, but %s after the deadline %s the node is still there and the notice is still on it; the signal may be stuck, and the cordon is kept",
		notice.Kind, inc.Target.Node, interruptionOverdue, notice.due().UTC().Format(time.RFC3339))
	return true, c.quarantine(ctx, inc, reason)
}
