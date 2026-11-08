package checkpoint

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Advisory keys. An advisory is the Phase 2 notice stamped on an opted-in
// workload when its incident parks for a human decision and a disruption
// still lies downstream: "a drain may follow; if you can checkpoint, now is a
// good time". It promises nothing and waits for nothing. The Phase 1 request,
// with its deadline, still arrives at the disruption step exactly as before.
//
// The keys are deliberately disjoint from the request keys: a request is a
// question with a deadline, an advisory is information, and a workload that
// reads its own annotations must never mistake one for the other. Every key
// satisfies IsOwnedAnnotation.
const (
	// AnnotationAdvisory is written by KubeNeuron with the value
	// AdvisoryValue when an advisory is in force on the workload.
	AnnotationAdvisory = "kubeneuron.io/checkpoint-advisory"
	// AnnotationAdvisoryAt is when the advisory was stamped.
	AnnotationAdvisoryAt = "kubeneuron.io/checkpoint-advisory-at"
	// AnnotationAdvisoryIncident is the incident the advisory belongs to.
	AnnotationAdvisoryIncident = "kubeneuron.io/checkpoint-advisory-incident"
	// AnnotationAdvisoryRound is the approval round (the incident's approval
	// epoch) the advisory was stamped for, as a decimal integer. One advisory
	// goes out per round; a re-park is a new round and a new stamp.
	AnnotationAdvisoryRound = "kubeneuron.io/checkpoint-advisory-round"
	// AnnotationAdvisoryReason is the problem class.
	AnnotationAdvisoryReason = "kubeneuron.io/checkpoint-advisory-reason"
	// AnnotationAdvisoryNextAction is the disruption that may follow, in the
	// same wire form a request uses ("platform.drain").
	AnnotationAdvisoryNextAction = "kubeneuron.io/checkpoint-advisory-next-action"
	// AnnotationAdvisoryWithdrawnAt is written, best effort, when the round
	// ended without the disruption: rejected, expired or resolved by hand.
	// The advisory keys stay on the object as history; this one says the
	// notice no longer stands.
	AnnotationAdvisoryWithdrawnAt = "kubeneuron.io/checkpoint-advisory-withdrawn-at"

	// AdvisoryValue is the only value of AnnotationAdvisory that means an
	// advisory is in force.
	AdvisoryValue = "true"
)

// Advisory budget bounds. The budget is the whole of what an advisory round
// may spend on listing and stamping; the park is already committed when the
// round starts, so the budget delays nothing a human is waiting on.
const (
	DefaultAdvisoryBudget = 10 * time.Second
	AdvisoryBudgetCeiling = 60 * time.Second
)

// AdvisoryPolicy is the compiled spec.safety.checkpointCoordination.advisory.
// The zero value is off: an advisory can only arrive by being asked for, and
// only inside an enabled coordination policy.
type AdvisoryPolicy struct {
	Enabled bool
	// Budget bounds one advisory round end to end. Zero on an enabled policy
	// is filled with DefaultAdvisoryBudget by the configuration layers.
	Budget time.Duration
}

// validate is called by Policy.Validate with the enclosing policy's state.
func (a AdvisoryPolicy) validate(coordinationEnabled bool) error {
	if !a.Enabled {
		return nil
	}
	if !coordinationEnabled {
		return fmt.Errorf("checkpoint coordination: advisory.enabled requires checkpointCoordination.enabled")
	}
	if a.Budget <= 0 {
		return fmt.Errorf("checkpoint coordination: advisory.budget must be positive, got %v", a.Budget)
	}
	if a.Budget > AdvisoryBudgetCeiling {
		return fmt.Errorf("checkpoint coordination: advisory.budget %v exceeds the %v ceiling", a.Budget, AdvisoryBudgetCeiling)
	}
	return nil
}

// AdvisoryOutcome is what one advisory attempt came to. The set is closed; it
// labels the advisories counter and nothing else.
type AdvisoryOutcome string

const (
	// AdvisoryStamped: the notice is on the object (or already was, for this
	// incident and round, which counts the same).
	AdvisoryStamped AdvisoryOutcome = "stamped"
	// AdvisoryWithdrawn: the round ended without the disruption and the
	// withdrawal was stamped.
	AdvisoryWithdrawn AdvisoryOutcome = "withdrawn"
	// AdvisoryUnreachable: the stamp (or the withdrawal) could not be written
	// within the budget, or the object could not be read.
	AdvisoryUnreachable AdvisoryOutcome = "unreachable"
	// AdvisorySkipped: no advisory went out for the round at all — the
	// feature is off, the class is skipped, nothing downstream disrupts,
	// no workload was eligible, the platform cannot stamp, or the incident
	// is a dry run.
	AdvisorySkipped AdvisoryOutcome = "skipped"
)

// AdvisoryOutcomes lists every value, for the metric's closed label set.
func AdvisoryOutcomes() []AdvisoryOutcome {
	return []AdvisoryOutcome{AdvisoryStamped, AdvisoryWithdrawn, AdvisoryUnreachable, AdvisorySkipped}
}

// Advisory is what KubeNeuron stamps on a workload when its incident parks
// for approval with a disruption still downstream. It is the parsed form of
// the advisory annotations.
type Advisory struct {
	IncidentID string
	// Round is the approval round, strictly positive: round 0 is the
	// pre-upgrade population and is never advised.
	Round     int
	AdvisedAt time.Time
	// Reason is the problem class; NextAction the disruption that may follow.
	Reason     string
	NextAction string
}

// Annotations renders the advisory as the annotation set to patch. Every key
// satisfies IsOwnedAnnotation; the timestamp is TimestampLayout.
func (a Advisory) Annotations() map[string]string {
	return map[string]string{
		AnnotationAdvisory:           AdvisoryValue,
		AnnotationAdvisoryAt:         a.AdvisedAt.UTC().Format(TimestampLayout),
		AnnotationAdvisoryIncident:   a.IncidentID,
		AnnotationAdvisoryRound:      strconv.Itoa(a.Round),
		AnnotationAdvisoryReason:     a.Reason,
		AnnotationAdvisoryNextAction: a.NextAction,
	}
}

// Validate rejects an advisory that could not be read back unambiguously
// after a restart: a blank or padded incident, a non-positive round, a blank
// reason or next action, or a timestamp that does not survive the
// TimestampLayout round trip.
func (a Advisory) Validate() error {
	switch {
	case strings.TrimSpace(a.IncidentID) == "":
		return fmt.Errorf("advisory names no incident")
	case strings.TrimSpace(a.IncidentID) != a.IncidentID:
		return fmt.Errorf("advisory incident ID %q has surrounding whitespace", a.IncidentID)
	case a.Round <= 0:
		return fmt.Errorf("advisory round must be positive, got %d", a.Round)
	case strings.TrimSpace(a.Reason) == "":
		return fmt.Errorf("advisory names no reason")
	case strings.TrimSpace(a.NextAction) == "":
		return fmt.Errorf("advisory names no next action")
	case a.AdvisedAt.IsZero():
		return fmt.Errorf("advisory has no advised-at time")
	}
	rendered := a.AdvisedAt.UTC().Format(TimestampLayout)
	if parsed, err := time.Parse(time.RFC3339, rendered); err != nil || !parsed.Equal(a.AdvisedAt) {
		return fmt.Errorf("advisory advised-at %v is not representable as RFC 3339", a.AdvisedAt)
	}
	return nil
}

// ParseAdvisory reads a previously stamped advisory back off a workload. It
// returns false when there is none, or when what is there is not a complete,
// well-formed advisory: a half-written or tampered stamp is not an advisory.
// A withdrawn advisory still parses (the withdrawal is reported separately by
// AdvisoryWithdrawn), because "which round was advised" stays true after the
// notice is withdrawn and is what makes the stamp idempotent per round.
func ParseAdvisory(annotations map[string]string) (Advisory, bool) {
	if annotations[AnnotationAdvisory] != AdvisoryValue {
		return Advisory{}, false
	}
	incident := strings.TrimSpace(annotations[AnnotationAdvisoryIncident])
	if incident == "" {
		return Advisory{}, false
	}
	round, err := strconv.Atoi(strings.TrimSpace(annotations[AnnotationAdvisoryRound]))
	if err != nil || round <= 0 {
		return Advisory{}, false
	}
	adv := Advisory{
		IncidentID: incident,
		Round:      round,
		Reason:     annotations[AnnotationAdvisoryReason],
		NextAction: annotations[AnnotationAdvisoryNextAction],
	}
	if at, err := time.Parse(time.RFC3339, annotations[AnnotationAdvisoryAt]); err == nil {
		adv.AdvisedAt = at
	}
	return adv, true
}

// SameRound reports whether an advisory already on a workload is the one the
// caller is about to stamp: same incident, same round. The timestamp is not
// compared, so a restart that re-runs the round finds its own stamp and
// writes nothing.
func (a Advisory) SameRound(existing Advisory) bool {
	return a.IncidentID != "" && a.IncidentID == existing.IncidentID && a.Round == existing.Round
}

// IsAdvisoryWithdrawn reports whether the advisory on a workload carries a
// withdrawal: the notice was stamped and then withdrawn. Only a parseable
// advisory can be withdrawn; a withdrawn-at key with no advisory beside it is
// residue and reads as false. So is a withdrawal OLDER than the advisory's
// own stamp: the withdrawn-at key is left in place when a later round stamps
// the object (KubeNeuron never deletes a protocol key it wrote), and a
// withdrawal that predates the notice belongs to the round before it, not to
// this one. The two timestamps come from the same controller clock, and a
// withdrawal is only ever written after its stamp, so "not before" is the
// test; an advisory whose stamp time did not parse keeps the old reading.
func IsAdvisoryWithdrawn(annotations map[string]string) bool {
	adv, ok := ParseAdvisory(annotations)
	if !ok {
		return false
	}
	withdrawnAt, err := time.Parse(time.RFC3339, annotations[AnnotationAdvisoryWithdrawnAt])
	if err != nil {
		return false
	}
	if adv.AdvisedAt.IsZero() {
		return true
	}
	return !withdrawnAt.Before(adv.AdvisedAt)
}

// WithdrawalAnnotations renders the one key a withdrawal writes. The advisory
// keys themselves are left in place: they are the record of which round was
// advised, and KubeNeuron never deletes a protocol key it wrote.
func WithdrawalAnnotations(at time.Time) map[string]string {
	return map[string]string{
		AnnotationAdvisoryWithdrawnAt: at.UTC().Format(TimestampLayout),
	}
}
