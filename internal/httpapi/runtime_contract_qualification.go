package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The runtime contract qualification surface is evidence-only. It has exactly
// four routes: create, list, get, and observe. There is deliberately no
// approve, promote, delete, apply, or enable route: a ReadyForApproval
// qualification is evidence a human may act on through a separate, later
// workflow, and nothing served here is consumed as authority.

type runtimeContractQualificationRequirementsAPIRequest struct {
	MinSamples int `json:"min_samples"`
	// MinDuration is a human-readable positive Go duration string ("30m",
	// "12h"). The durable record keeps nanoseconds; the wire never does.
	MinDuration string `json:"min_duration"`
}

type runtimeContractQualificationAPIRequest struct {
	Actor        string                                             `json:"actor"`
	Nodes        []string                                           `json:"nodes"`
	Vendor       types.AcceleratorVendor                            `json:"vendor"`
	Tenant       string                                             `json:"tenant,omitempty"`
	Cluster      string                                             `json:"cluster,omitempty"`
	Requirements runtimeContractQualificationRequirementsAPIRequest `json:"requirements"`
	ExpiresAt    time.Time                                          `json:"expires_at"`
}

func (r runtimeContractQualificationAPIRequest) compile() (operations.RuntimeContractQualificationRequest, error) {
	minDuration, err := time.ParseDuration(r.Requirements.MinDuration)
	if err != nil || minDuration <= 0 {
		return operations.RuntimeContractQualificationRequest{}, errors.New("requirements.min_duration must be a positive Go duration")
	}
	return operations.RuntimeContractQualificationRequest{
		Nodes: r.Nodes, Vendor: r.Vendor, Tenant: r.Tenant, Cluster: r.Cluster,
		Requirements: operations.RuntimeContractQualificationRequirements{MinSamples: r.Requirements.MinSamples, MinDuration: minDuration},
		ExpiresAt:    r.ExpiresAt,
	}, nil
}

// runtimeContractQualificationObserveRequest is the whole observe body. An
// observation carries no evidence from the caller: the controller re-reads
// inventory and coverage itself, so any other field is rejected by strict
// decoding rather than silently ignored.
type runtimeContractQualificationObserveRequest struct {
	Actor           string `json:"actor"`
	ResourceVersion int    `json:"resource_version,omitempty"`
}

func (s *Server) handleCreateRuntimeContractQualification(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request runtimeContractQualificationAPIRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	qualificationRequest, err := request.compile()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	qualificationRequest.IdempotencyKey = key
	qualification, replayed, err := manager.CreateRuntimeContractQualification(r.Context(), actor, qualificationRequest)
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/api/v1/runtime-contract-qualifications/"+qualification.ID)
	writeOperationalJSON(w, http.StatusCreated, manager.ViewRuntimeContractQualification(qualification))
}

// handleGetRuntimeContractQualification serves the stored record projected at
// the current clock. It never writes: a qualification whose expiry has passed
// is reported with effective_state Expired and ready_for_approval false even
// while its stored state still awaits the observation that records Expired.
func (s *Server) handleGetRuntimeContractQualification(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	qualification, err := manager.GetRuntimeContractQualification(r.Context(), r.PathValue("id"))
	if err != nil {
		operationsError(w, err)
		return
	}
	writeJSON(w, manager.ViewRuntimeContractQualification(qualification))
}

func (s *Server) handleListRuntimeContractQualifications(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	options, err := operationalListOptions(r, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items, err := manager.ListRuntimeContractQualifications(r.Context(), options)
	if err != nil {
		operationsError(w, err)
		return
	}
	writeOperationalPage(w, manager.ViewRuntimeContractQualifications(items), options.Limit, func(item *operations.RuntimeContractQualificationView) (time.Time, string) {
		return item.CreatedAt, item.ID
	})
}

func (s *Server) handleObserveRuntimeContractQualification(w http.ResponseWriter, r *http.Request) {
	manager := s.operationsManager(w)
	if manager == nil {
		return
	}
	key, ok := operationalIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request runtimeContractQualificationObserveRequest
	if !decodeStrict(w, r, &request) {
		return
	}
	actor, err := s.resolveActor(r, request.Actor)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	qualification, replayed, err := manager.ObserveRuntimeContractQualification(r.Context(), actor, r.PathValue("id"), operations.RuntimeContractQualificationMutation{
		ExpectedVersion: request.ResourceVersion, IdempotencyKey: key,
	})
	if err != nil {
		operationsError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeOperationalJSON(w, http.StatusOK, manager.ViewRuntimeContractQualification(qualification))
}
