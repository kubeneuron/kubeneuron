// Package checkpoint holds the pure policy core of checkpoint-aware
// remediation (docs/checkpoint-coordination-design.md): the annotation
// contract a workload uses to opt in, the operator policy that bounds how long
// a disruption may wait for it, and the classification of what the workload
// did with the request.
//
// Nothing here talks to a cluster. There is no Kubernetes client, no clock
// beyond the caller's `now`, and no goroutine, so every decision is a table
// test. The platform adapter that patches annotations and the controller step
// that waits are built on top of this package, not inside it.
//
// The one rule every function enforces: a workload may influence how it dies,
// never whether. The policy owns the clock; annotations can shorten a wait and
// can never lengthen one.
package checkpoint

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Annotation keys of the protocol. Every key KubeNeuron reads or writes on a
// workload lives under AnnotationPrefix, which is what lets the platform
// adapter enforce in code the scope RBAC cannot express.
const (
	// AnnotationPrefix is the common prefix of every protocol key.
	AnnotationPrefix = "kubeneuron.io/checkpoint"

	// AnnotationOptIn is set by the workload. Only the exact value OptInValue
	// opts in; anything else, including "True" and "yes", is "no".
	AnnotationOptIn = "kubeneuron.io/checkpoint"
	// AnnotationMaxWait is the workload's REQUESTED wait, a Go duration. It is
	// clamped by policy and can only ever shorten the wait.
	AnnotationMaxWait = "kubeneuron.io/checkpoint-max-wait"

	// AnnotationRequestedAt is written by KubeNeuron: when the request went out.
	AnnotationRequestedAt = "kubeneuron.io/checkpoint-requested-at"
	// AnnotationDeadlineAt is written by KubeNeuron: the ABSOLUTE deadline. It
	// is durable on the object so a controller restart resumes the same window.
	AnnotationDeadlineAt = "kubeneuron.io/checkpoint-deadline-at"
	// AnnotationIncident is written by KubeNeuron: the incident the request
	// belongs to. A deadline is only ever reused for the same incident.
	AnnotationIncident = "kubeneuron.io/checkpoint-incident"
	// AnnotationReason is written by KubeNeuron: the problem class.
	AnnotationReason = "kubeneuron.io/checkpoint-reason"
	// AnnotationNextAction is written by KubeNeuron: the disruption that follows.
	AnnotationNextAction = "kubeneuron.io/checkpoint-next-action"

	// AnnotationState is patched back by a workload that stays alive after
	// checkpointing. Only the value StateCompleteFor renders for the incident
	// named in the workload's own AnnotationIncident acknowledges; there is no
	// "extend".
	AnnotationState = "kubeneuron.io/checkpoint-state"

	// OptInValue is the only value of AnnotationOptIn that opts a workload in.
	OptInValue = "true"
	// StateCompletePrefix is the prefix of an acknowledging AnnotationState
	// value; the rest is the incident ID the acknowledgement is bound to. See
	// StateCompleteFor and Acknowledges.
	StateCompletePrefix = "complete:"
)

// StateCompleteFor renders the one AnnotationState value that acknowledges
// the request of the given incident: "complete:<incident-id>". The incident
// ID is the value of AnnotationIncident the workload read off its own object,
// which is what binds the answer to the question. A plain "complete", or a
// value bound to another incident, never acknowledges: without the binding, a
// stale acknowledgement left on a long-lived workload by an earlier incident
// would read as an answer to every later one.
func StateCompleteFor(incidentID string) string {
	return StateCompletePrefix + incidentID
}

// Acknowledges reports whether state, as read off a workload, acknowledges
// the request of incidentID. The comparison is exact and case-sensitive, like
// the opt-in: the annotation is a contract. An empty incident ID matches
// nothing, so a request that named no incident can never be acknowledged.
func Acknowledges(state, incidentID string) bool {
	return incidentID != "" && state == StateCompleteFor(incidentID)
}

// AcknowledgesInForce reports whether a live observation of a workload's
// annotations acknowledges the request in force on it. Three things must hold
// at once: the observed annotations carry a request ParseRequest accepts, that
// request names the same incident as inForce, and the state is the exact
// StateCompleteFor value of that incident.
//
// The second condition is what makes a retained acknowledgement safe to read
// across a request race. A workload answers the request it read off its own
// object; when the live object now carries a request from a DIFFERENT incident
// than the one this step bound itself to, the state on it, even one that
// names the bound incident, is an answer to a question that is no longer the
// one in force, and it must not settle this step. A missing or malformed live
// request fails closed for the same reason: there is no question on the
// object for the state to be an answer to.
//
// Only the incident is compared, never the timestamps: a same-incident resume
// keeps the original request time and may have had its deadline capped, so
// the stamp this step holds and the stamp on the object legitimately differ
// in those while still being one request.
func AcknowledgesInForce(inForce Request, annotations map[string]string) bool {
	if inForce.IncidentID == "" {
		return false
	}
	observed, ok := ParseRequest(annotations)
	if !ok || observed.IncidentID != inForce.IncidentID {
		return false
	}
	return Acknowledges(annotations[AnnotationState], inForce.IncidentID)
}

// Policy bounds. MaxWaitCeiling is the hard installation-wide limit no
// configuration can raise; the defaults mirror the CRD defaults so the same
// numbers are in force at every layer that could be reached first.
const (
	DefaultWait    = 5 * time.Minute
	DefaultMaxWait = 15 * time.Minute
	MaxWaitCeiling = 30 * time.Minute
)

// DefaultSkipClasses returns the problem classes an enabled policy skips when
// the configuration names none: the classes in which the device is already
// gone, so there is no running job left to warn and every second of waiting
// only delays recovery. A fresh slice is returned each call so no caller can
// alter the default for another.
//
// Defaulting applies to an ABSENT list only. A configuration that spells out
// an explicit list, including an explicit empty one, is taken as written; the
// operator and the controller both preserve that distinction.
func DefaultSkipClasses() []types.ProblemClass {
	return []types.ProblemClass{types.ClassFellOffBus, types.ClassGPULost}
}

// IsOwnedAnnotation reports whether key is one of the protocol's keys. The
// platform adapter uses it to refuse to write anything else on a pod, which is
// the in-code half of the "pods: patch" privilege boundary.
func IsOwnedAnnotation(key string) bool {
	return key == AnnotationPrefix || strings.HasPrefix(key, AnnotationPrefix+"-")
}

// Policy is the compiled spec.safety.checkpointCoordination. The zero value is
// disabled, which is what every configuration written before the feature
// existed decodes to: coordination can only arrive by being asked for.
//
// A Policy is a value; nothing mutates one after construction, so a snapshot
// may be read from any goroutine.
type Policy struct {
	Enabled bool
	// DefaultWait is granted to an opted-in workload that requests nothing, or
	// whose request cannot be parsed.
	DefaultWait time.Duration
	// MaxWait is the ceiling. A workload request is clamped to it, and the
	// shared deadline of a disruption step never exceeds it.
	MaxWait time.Duration
	// SkipClasses are problem classes for which no coordination is attempted:
	// a device that has fallen off the bus has no job left to warn. The
	// configuration layers fill an absent list with DefaultSkipClasses before
	// a Policy is built; the Policy itself applies exactly what it holds.
	SkipClasses []types.ProblemClass
	// Namespaces is the explicit allowlist of namespaces whose workloads may
	// opt in. Self-declaration is a trust decision, so it is never fleet-wide
	// by default.
	Namespaces []string
}

// Validate rejects a policy that could not be enforced as written. A disabled
// policy is always valid: nothing in it is read.
func (p Policy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.DefaultWait <= 0 {
		return fmt.Errorf("checkpoint coordination: defaultWait must be positive, got %v", p.DefaultWait)
	}
	if p.MaxWait <= 0 {
		return fmt.Errorf("checkpoint coordination: maxWait must be positive, got %v", p.MaxWait)
	}
	if p.MaxWait > MaxWaitCeiling {
		return fmt.Errorf("checkpoint coordination: maxWait %v exceeds the %v ceiling", p.MaxWait, MaxWaitCeiling)
	}
	if p.DefaultWait > p.MaxWait {
		return fmt.Errorf("checkpoint coordination: defaultWait %v exceeds maxWait %v", p.DefaultWait, p.MaxWait)
	}
	if len(p.Namespaces) == 0 {
		return fmt.Errorf("checkpoint coordination: an enabled policy requires a non-empty namespaces allowlist")
	}
	for _, ns := range p.Namespaces {
		if strings.TrimSpace(ns) == "" {
			return fmt.Errorf("checkpoint coordination: namespaces must not contain a blank entry")
		}
	}
	for _, class := range p.SkipClasses {
		if strings.TrimSpace(string(class)) == "" {
			return fmt.Errorf("checkpoint coordination: skipClasses must not contain a blank entry")
		}
	}
	return nil
}

// Clone returns a policy that shares no memory with the receiver, sorted so
// two equivalent policies compare and serialize identically.
func (p Policy) Clone() Policy {
	out := p
	out.SkipClasses = nil
	out.Namespaces = nil
	if len(p.SkipClasses) > 0 {
		out.SkipClasses = append([]types.ProblemClass(nil), p.SkipClasses...)
		sort.Slice(out.SkipClasses, func(i, j int) bool { return out.SkipClasses[i] < out.SkipClasses[j] })
	}
	if len(p.Namespaces) > 0 {
		out.Namespaces = append([]string(nil), p.Namespaces...)
		sort.Strings(out.Namespaces)
	}
	return out
}

// AllowsNamespace reports whether workloads in ns may opt in. A disabled
// policy allows nothing.
func (p Policy) AllowsNamespace(ns string) bool {
	if !p.Enabled || ns == "" {
		return false
	}
	for _, allowed := range p.Namespaces {
		if allowed == ns {
			return true
		}
	}
	return false
}

// SkipsClass reports whether the policy declines to coordinate for class.
func (p Policy) SkipsClass(class types.ProblemClass) bool {
	for _, skipped := range p.SkipClasses {
		if skipped == class {
			return true
		}
	}
	return false
}

// Workload is the platform-agnostic view of one schedulable unit the policy
// decides about. Namespace and Name mirror platform.Workload; UID and
// Annotations are what the checkpoint protocol additionally needs, and the
// platform adapter fills them in from the object it already lists.
type Workload struct {
	Namespace string
	Name      string
	// UID identifies the object instance. A restarted pod is a new object with
	// a new UID and must never inherit the old one's request or deadline.
	UID         string
	Annotations map[string]string
}

// OptedIn reports whether the workload declared checkpoint support. The
// comparison is exact and case-sensitive on purpose: the annotation is a
// contract, and "True" is a typo, not a declaration.
func (w Workload) OptedIn() bool {
	return w.Annotations[AnnotationOptIn] == OptInValue
}

// RequestedMaxWait returns the workload's requested wait when it is present,
// parseable and positive. A missing, malformed, zero or negative request
// returns false, and the caller falls back to the policy default: a request
// can only ever be honored downward, so a bad one is simply not a request.
func (w Workload) RequestedMaxWait() (time.Duration, bool) {
	raw, ok := w.Annotations[AnnotationMaxWait]
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// Ineligibility explains why a workload is not coordinated with. Empty means
// eligible. The values are stable strings so they can label an audit row.
type Ineligibility string

const (
	// IneligiblePolicyDisabled: the installation has not turned the feature on.
	IneligiblePolicyDisabled Ineligibility = "policy-disabled"
	// IneligibleClassSkipped: the problem class is in skipClasses.
	IneligibleClassSkipped Ineligibility = "class-skipped"
	// IneligibleNamespace: the workload's namespace is not on the allowlist.
	IneligibleNamespace Ineligibility = "namespace-not-allowed"
	// IneligibleNotOptedIn: the workload did not declare checkpoint support.
	IneligibleNotOptedIn Ineligibility = "not-opted-in"
)

// Eligibility decides whether w is coordinated with for an incident of the
// given class. The checks run cheapest-and-broadest first, so a disabled
// installation costs one boolean per workload.
func (p Policy) Eligibility(w Workload, class types.ProblemClass) Ineligibility {
	switch {
	case !p.Enabled:
		return IneligiblePolicyDisabled
	case p.SkipsClass(class):
		return IneligibleClassSkipped
	case !p.AllowsNamespace(w.Namespace):
		return IneligibleNamespace
	case !w.OptedIn():
		return IneligibleNotOptedIn
	}
	return ""
}

// Eligible filters workloads down to the ones Eligibility accepts, preserving
// order. It never returns a slice aliasing the input.
func (p Policy) Eligible(workloads []Workload, class types.ProblemClass) []Workload {
	var out []Workload
	for _, w := range workloads {
		if p.Eligibility(w, class) == "" {
			out = append(out, w)
		}
	}
	return out
}

// WaitFor is the wait granted to one workload: its request when it made a
// valid one, the policy default otherwise, and in every case no more than
// MaxWait. A disabled policy grants nothing.
func (p Policy) WaitFor(w Workload) time.Duration {
	if !p.Enabled {
		return 0
	}
	wait := p.DefaultWait
	if requested, ok := w.RequestedMaxWait(); ok {
		wait = requested
	}
	return clamp(wait, p.MaxWait)
}

// BoundedWait is the wait one workload is granted by a disruption step: its
// own WaitFor grant, further bounded by the step's remaining budget. This is
// the per-workload deadline the step stamps, so a workload asking for 30s gets
// 30s even when a neighbour on the same node is granted the 5m default; a
// request can only ever shorten THAT workload's window. A budget of zero or
// less means "no budget known" and only the policy bound applies.
func (p Policy) BoundedWait(w Workload, budget time.Duration) time.Duration {
	wait := p.WaitFor(w)
	if budget > 0 {
		wait = clamp(wait, budget)
	}
	return wait
}

// SharedWait is the longest of the BoundedWait grants across the eligible
// workloads: the overall time a disruption step may spend in its pre-phase,
// since the step waits until the LATEST per-workload deadline in force. It is
// a bound on the step, not a grant to any workload: each workload's own
// deadline is BoundedWait, never this. Each grant is already bounded by
// MaxWait in WaitFor, so the longest is too. A budget of zero or less means
// "no budget known" and only the policy bound applies. No workloads means no
// wait.
func (p Policy) SharedWait(workloads []Workload, budget time.Duration) time.Duration {
	if !p.Enabled || len(workloads) == 0 {
		return 0
	}
	var longest time.Duration
	for _, w := range workloads {
		if wait := p.WaitFor(w); wait > longest {
			longest = wait
		}
	}
	if budget > 0 {
		longest = clamp(longest, budget)
	}
	return longest
}

// clamp bounds d to at most limit, and never below zero.
func clamp(d, limit time.Duration) time.Duration {
	if d > limit {
		d = limit
	}
	if d < 0 {
		return 0
	}
	return d
}

// Request is what KubeNeuron stamps on a workload when it asks for a
// checkpoint. It is the parsed form of the request annotations.
type Request struct {
	IncidentID  string
	RequestedAt time.Time
	DeadlineAt  time.Time
	// Reason is the problem class; NextAction the disruption that follows.
	Reason     types.ProblemClass
	NextAction string
}

// TimestampLayout is the wire format of the two timestamps a request stamps:
// RFC 3339 in UTC, with the fractional second kept when there is one, so the
// object reads the same from any time zone and any replica and a window
// shorter than a second (a valid checkpoint-max-wait of 500ms) survives the
// round trip instead of being cut to the whole second. ParseRequest accepts
// both this and a whole-second RFC 3339 value.
const TimestampLayout = time.RFC3339Nano

// Annotations renders the request as the annotation set to patch. Every key
// satisfies IsOwnedAnnotation; timestamps are TimestampLayout.
func (r Request) Annotations() map[string]string {
	return map[string]string{
		AnnotationRequestedAt: r.RequestedAt.UTC().Format(TimestampLayout),
		AnnotationDeadlineAt:  r.DeadlineAt.UTC().Format(TimestampLayout),
		AnnotationIncident:    r.IncidentID,
		AnnotationReason:      string(r.Reason),
		AnnotationNextAction:  r.NextAction,
	}
}

// ParseRequest reads a previously stamped request back off a workload. It
// returns false when there is none, or when what is there is not a complete,
// well-formed request: a half-written or tampered stamp is not a request and
// must not be reused as one.
func ParseRequest(annotations map[string]string) (Request, bool) {
	incident := strings.TrimSpace(annotations[AnnotationIncident])
	if incident == "" {
		return Request{}, false
	}
	// time.RFC3339 parses a fractional second when one is present, so a
	// TimestampLayout stamp and an older whole-second one both read back.
	deadline, err := time.Parse(time.RFC3339, annotations[AnnotationDeadlineAt])
	if err != nil {
		return Request{}, false
	}
	req := Request{
		IncidentID: incident,
		DeadlineAt: deadline,
		Reason:     types.ProblemClass(annotations[AnnotationReason]),
		NextAction: annotations[AnnotationNextAction],
	}
	if requested, err := time.Parse(time.RFC3339, annotations[AnnotationRequestedAt]); err == nil {
		req.RequestedAt = requested
	}
	return req, true
}

// ResolveDeadline picks the deadline for a request about to be stamped.
//
// proposed is the fresh deadline the caller computed from the policy (the
// request time plus this workload's BoundedWait). A stamp already on the
// workload is the durable record of the
// window this incident was granted, so when it belongs to the same incident
// the result is the EARLIER of the stamp and proposed:
//
//   - a stamp still in the future is resumed as-is, so a controller restart
//     mid-wait continues the window it granted instead of granting a new one;
//   - a stamp that has already expired is returned unchanged, so the restart
//     finds the wait over and disrupts immediately rather than starting a
//     fresh countdown for a job that already had its whole window;
//   - a stamp later than proposed — a shrunken step budget, a stale annotation,
//     or a workload with patch rights editing its own deadline — is cut down
//     to proposed, so nothing on the object can ever extend a wait.
//
// A stamp from another incident, or none at all, takes proposed: the earlier
// incident's window says nothing about this one. Callers must only pass an
// existing stamp that ParseRequest accepted; a half-written or malformed stamp
// is not a request and must not be resolved against.
func ResolveDeadline(proposed time.Time, incidentID string, existing *Request) time.Time {
	if existing == nil || existing.IncidentID == "" || existing.IncidentID != incidentID {
		return proposed
	}
	if existing.DeadlineAt.After(proposed) {
		return proposed
	}
	return existing.DeadlineAt
}

// RequestStart picks the durable start of a request's window for the wait
// metric: the time the request first went out, as recorded on the object,
// rather than the time this step re-issued it.
//
// durable is the RequestedAt read back from the in-force request (zero when
// the stamp carried none); fresh is the time this step issued its own request
// and is always trusted. The result is durable when it is present and no
// later than fresh, so a resumed same-incident request and an honored foreign
// request both measure from the moment the workload was first asked. A zero
// or future durable value falls back to fresh, so a tampered stamp cannot put
// the start after the request; a durable value more than MaxWaitCeiling
// before fresh is floored there, so a stale or tampered stamp cannot put the
// start more than one ceiling before it.
//
// This bounds only the START. The outcome settles at or after fresh, so the
// elapsed time from a floored start can still exceed MaxWaitCeiling by the
// part of the window this step itself waited; ObservedWait bounds the elapsed
// time itself and is what the metric must go through.
func RequestStart(durable, fresh time.Time) time.Time {
	if durable.IsZero() || durable.After(fresh) {
		return fresh
	}
	if floor := fresh.Add(-MaxWaitCeiling); durable.Before(floor) {
		return floor
	}
	return durable
}

// ObservedWait is the elapsed time the wait metric observes for one request:
// from its start (RequestStart) until its outcome settled, bounded to the
// range [0, MaxWaitCeiling]. A settlement before the start, which no clock the
// controller uses should produce, observes zero; anything longer than the
// ceiling observes exactly the ceiling. The upper bound holds regardless of
// the start: a stale or tampered requested-at floored one ceiling before the
// request, followed by a wait that ends after the request, would otherwise
// exceed the range any window can legitimately span and the histogram's top
// bucket. A valid same-incident resume within the ceiling is unaffected, as
// its window never exceeds MaxWait, which is at most MaxWaitCeiling.
func ObservedWait(start, settled time.Time) time.Duration {
	return clamp(settled.Sub(start), MaxWaitCeiling)
}

// Outcome is what a workload did with a checkpoint request, as far as one
// observation can tell. The names are the metric label values.
type Outcome string

const (
	// OutcomeAcknowledged: the workload patched checkpoint-state with the
	// acknowledgement bound to the request's incident (StateCompleteFor).
	OutcomeAcknowledged Outcome = "acknowledged"
	// OutcomeExited: the workload terminated or is gone, which acknowledges by
	// the zero-RBAC path. Counted separately from an explicit acknowledgement
	// because it also covers a job that simply crashed.
	OutcomeExited Outcome = "exited"
	// OutcomePending: still running, not acknowledged, deadline not reached.
	OutcomePending Outcome = "pending"
	// OutcomeExpired: the deadline passed without an acknowledgement.
	OutcomeExpired Outcome = "expired"
	// OutcomeUnreachable: the workload could not be observed. The caller
	// keeps to the same bounded wait; a read failure must never block a
	// disruption and never lengthen the wait.
	OutcomeUnreachable Outcome = "unreachable"
	// OutcomeSkipped: no request was made at all, because the policy, class,
	// namespace, or the workload itself said no. Not produced by Classify.
	OutcomeSkipped Outcome = "skipped"
)

// Settled reports whether the outcome ends the wait for this workload.
func (o Outcome) Settled() bool {
	switch o {
	case OutcomeAcknowledged, OutcomeExited, OutcomeExpired, OutcomeSkipped:
		return true
	}
	return false
}

// Observation is one look at a workload after a request was stamped on it.
type Observation struct {
	// Err is set when the workload could not be read at all.
	Err error
	// Found is false when the object no longer exists.
	Found bool
	// UID is the observed object's UID; a different UID from the requested
	// workload means the original is gone and this is a replacement.
	UID string
	// Terminal is true once the workload has finished (for a pod: phase
	// Succeeded or Failed).
	Terminal bool
	// Annotations are the observed object's current annotations.
	Annotations map[string]string
}

// Classify turns one observation into an outcome, deterministically. inForce
// is the request the workload is being held to: its IncidentID is what an
// acknowledgement must be bound to and its DeadlineAt is the (already
// bounded) deadline.
//
//  1. an unreadable workload is unreachable, whatever the clock says;
//  2. a workload that is gone, replaced by a new object, or finished has exited;
//  3. a checkpoint-state that acknowledges the in-force incident, on an
//     object whose live request still names that incident
//     (AcknowledgesInForce), is acknowledged, even after the deadline,
//     because a late answer is still the cleaner disruption;
//  4. otherwise the deadline decides between pending and expired.
//
// Anything a workload writes to checkpoint-state other than the exact value
// StateCompleteFor(inForce.IncidentID), including "in-progress", a plain
// "complete", or an acknowledgement bound to another incident, is not an
// acknowledgement. Neither is the exact value when the live request on the
// object is missing, malformed, or from another incident than inForce: the
// answer is then to a question that is not the one in force.
func Classify(now time.Time, inForce Request, requested Workload, observed Observation) Outcome {
	if observed.Err != nil {
		return OutcomeUnreachable
	}
	if !observed.Found || observed.Terminal {
		return OutcomeExited
	}
	if requested.UID != "" && observed.UID != "" && observed.UID != requested.UID {
		return OutcomeExited
	}
	if AcknowledgesInForce(inForce, observed.Annotations) {
		return OutcomeAcknowledged
	}
	if !now.Before(inForce.DeadlineAt) {
		return OutcomeExpired
	}
	return OutcomePending
}
