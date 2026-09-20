package operations

import (
	"fmt"
	"time"
)

// RuntimeContractQualificationRequirementsView is the operator-facing shape
// of the evidence bar. The durable payload keeps MinDuration as nanoseconds;
// the wire presents the same value as a human-readable Go duration string, the
// form the create request accepts, so what an operator reads back is what they
// asked for.
type RuntimeContractQualificationRequirementsView struct {
	MinSamples  int    `json:"min_samples"`
	MinDuration string `json:"min_duration"`
}

// RuntimeContractQualificationView is the read projection served to
// operators. It embeds the durable record unchanged and adds fields computed
// at read time from the wall clock, so a qualification that is stored as
// ReadyForApproval but whose expiry has passed is never presented as ready.
//
// The projection is pure: it is computed from the stored record and the clock
// and writes nothing. Only Observe persists the Expired transition; until it
// does, State still reads ReadyForApproval or Observing while EffectiveState,
// Expired, ExpiryPending, and ReadyForApproval all say the window has closed.
//
// Nothing here is authority. ReadyForApproval is the answer to "may a human
// consider this evidence for a separate, later decision", not an admission.
type RuntimeContractQualificationView struct {
	*RuntimeContractQualification
	Requirements RuntimeContractQualificationRequirementsView `json:"requirements"`
	// EvaluatedAt is the clock instant the effective fields were computed at.
	EvaluatedAt time.Time `json:"evaluated_at"`
	// EffectiveState is the lifecycle state at EvaluatedAt: the stored State,
	// except that a non-terminal qualification past ExpiresAt reads Expired.
	EffectiveState RuntimeContractQualificationState `json:"effective_state"`
	// Expired is true when the qualification is Expired at EvaluatedAt,
	// whether or not an observation has persisted that yet. It is false for
	// an Invalidated qualification: invalidation is its outcome, not expiry.
	Expired bool `json:"expired"`
	// ExpiryPending is true when the wall clock has passed ExpiresAt but the
	// stored State has not recorded Expired yet. The next Observe will.
	ExpiryPending bool `json:"expiry_pending"`
	// ReadyForApproval is true only when the stored State is
	// ReadyForApproval and the window has not closed. It is the single field
	// an operator should read to decide whether the evidence is current.
	ReadyForApproval bool `json:"ready_for_approval"`
	// Summary states the effective status in one sentence.
	Summary string `json:"summary"`
}

// NewRuntimeContractQualificationView projects one stored qualification at
// now. It never mutates the qualification it wraps.
func NewRuntimeContractQualificationView(qualification *RuntimeContractQualification, now time.Time) *RuntimeContractQualificationView {
	if qualification == nil {
		return nil
	}
	now = now.UTC()
	view := &RuntimeContractQualificationView{
		RuntimeContractQualification: qualification,
		Requirements: RuntimeContractQualificationRequirementsView{
			MinSamples: qualification.Requirements.MinSamples, MinDuration: qualification.Requirements.MinDuration.String(),
		},
		EvaluatedAt:    now,
		EffectiveState: qualification.State,
	}
	// The same clock comparison Observe uses to decide expiry, so the
	// projection and the persisted transition can never disagree.
	windowClosed := !now.Before(qualification.ExpiresAt)
	switch {
	case qualification.State == QualificationExpired:
		view.Expired = true
	case qualification.State.terminal():
		// Invalidated: the outcome is final and the window is irrelevant.
	case windowClosed:
		view.EffectiveState, view.Expired, view.ExpiryPending = QualificationExpired, true, true
	}
	view.ReadyForApproval = view.EffectiveState == QualificationReadyForApproval
	view.Summary = view.summary()
	return view
}

func (v *RuntimeContractQualificationView) summary() string {
	q := v.RuntimeContractQualification
	evidence := fmt.Sprintf("%d/%d successful samples over %d observations", q.SuccessfulSamples, q.Requirements.MinSamples, q.TotalObservations)
	switch {
	case v.ExpiryPending:
		return fmt.Sprintf("expired at %s while stored as %s; not ready for approval; the next observation records Expired (%s)",
			q.ExpiresAt.Format(time.RFC3339), q.State, evidence)
	case v.EffectiveState == QualificationExpired:
		return fmt.Sprintf("expired at %s without approval-ready evidence surviving the window (%s)", q.ExpiresAt.Format(time.RFC3339), evidence)
	case v.EffectiveState == QualificationInvalidated:
		return fmt.Sprintf("invalidated: %s (%s)", q.InvalidationReason, evidence)
	case v.EffectiveState == QualificationReadyForApproval:
		ready := ""
		if q.ReadyAt != nil {
			ready = " since " + q.ReadyAt.Format(time.RFC3339)
		}
		return fmt.Sprintf("ready for approval%s until %s; evidence only, not an authorization (%s)", ready, q.ExpiresAt.Format(time.RFC3339), evidence)
	default:
		return fmt.Sprintf("observing until %s; requires %d successful samples over at least %s (%s)",
			q.ExpiresAt.Format(time.RFC3339), q.Requirements.MinSamples, q.Requirements.MinDuration, evidence)
	}
}

// ViewRuntimeContractQualification projects a qualification at the manager's
// clock. It is the read path the API serves; it performs no write.
func (m *Manager) ViewRuntimeContractQualification(qualification *RuntimeContractQualification) *RuntimeContractQualificationView {
	return NewRuntimeContractQualificationView(qualification, m.now())
}

// ViewRuntimeContractQualifications projects a page at one shared instant so
// every item on the page is judged against the same clock.
func (m *Manager) ViewRuntimeContractQualifications(qualifications []*RuntimeContractQualification) []*RuntimeContractQualificationView {
	now := m.now()
	out := make([]*RuntimeContractQualificationView, 0, len(qualifications))
	for _, qualification := range qualifications {
		out = append(out, NewRuntimeContractQualificationView(qualification, now))
	}
	return out
}
