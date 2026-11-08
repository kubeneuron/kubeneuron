package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// impactOperator adds the optional ImpactPreviewBackend to the fake.
type impactOperator struct {
	*fakeOperator
	previews   map[int]*types.ImpactPreview // by round; 0 = current
	recomputed []string
	err        error
}

func (o *impactOperator) ImpactPreview(_ context.Context, id string, round int) (*types.ImpactPreview, error) {
	if o.err != nil {
		return nil, o.err
	}
	if round == 0 || round == 2 { // the fake incident's current round is 2
		round = 0
	}
	p, ok := o.previews[round]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (o *impactOperator) RecomputeImpactPreview(_ context.Context, id, actor, key string) (*types.ImpactPreview, bool, error) {
	if o.err != nil {
		return nil, false, o.err
	}
	o.recomputed = append(o.recomputed, id+":"+actor+":"+key)
	replayed := len(o.recomputed) > 1 && strings.HasSuffix(o.recomputed[len(o.recomputed)-2], ":"+key)
	return o.previews[0], replayed, nil
}

func (o *impactOperator) IncidentDetail(_ context.Context, id string) (*types.Incident, []*types.AuditEntry, error) {
	return &types.Incident{ID: id, ApprovalEpoch: 2}, nil, nil
}

func impactServer(previews map[int]*types.ImpactPreview) (*impactOperator, http.Handler) {
	op := &impactOperator{fakeOperator: &fakeOperator{}, previews: previews}
	return op, operatorServer(op, "secret")
}

func TestImpactPreviewRouteServesTheStoredDocument(t *testing.T) {
	doc := `{"version":"impact-preview/v1","incident":"inc-1","round":2,"digest":"sha256:cur","workloads":{"total":3}}`
	op, handler := impactServer(map[int]*types.ImpactPreview{
		0: {IncidentID: "inc-1", Epoch: 2, Digest: "sha256:cur", Payload: []byte(doc)},
		1: {IncidentID: "inc-1", Epoch: 1, Digest: "sha256:old", UnavailableReason: "WorkloadListingTimeout"},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview", "secret", ""))
	if rec.Code != http.StatusOK || rec.Body.String() != doc {
		t.Fatalf("current = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview?round=1", "secret", ""))
	var envelope types.ImpactPreview
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &envelope) != nil || envelope.UnavailableReason != "WorkloadListingTimeout" {
		t.Fatalf("round 1 = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview?round=7", "secret", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing round = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview?round=zero", "secret", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad round = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview", "wrong", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}

	// The detail carries the current digest.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1", "secret", ""))
	var detail IncidentDetail
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	if detail.ImpactPreviewDigest != "sha256:cur" {
		t.Fatalf("detail digest = %q", detail.ImpactPreviewDigest)
	}
	if detail.Decision != "" || detail.DecisionImpactDigest != "" {
		t.Fatalf("a backend without round decisions produced %q/%q", detail.Decision, detail.DecisionImpactDigest)
	}
	_ = op
}

type decisionOperator struct {
	*impactOperator
	decision *types.Approval
}

func (o *decisionOperator) RoundDecision(_ context.Context, _ string, _ int) (*types.Approval, error) {
	if o.decision == nil {
		return nil, store.ErrNotFound
	}
	return o.decision, nil
}

// The detail carries the decision recorded on the current round and the
// digest THAT row persisted, beside the newest preview's digest, so a reader
// can tell a decision bound to the current preview from one that is not.
func TestIncidentDetailCarriesTheDecisionAndItsDigest(t *testing.T) {
	op := &decisionOperator{impactOperator: &impactOperator{fakeOperator: &fakeOperator{}, previews: map[int]*types.ImpactPreview{
		0: {IncidentID: "inc-1", Epoch: 2, Digest: "sha256:cur", Version: "impact-preview/v1", Payload: []byte(`{}`)},
	}}}
	handler := operatorServer(op, "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1", "secret", ""))
	var detail IncidentDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Decision != "" || detail.DecisionImpactDigest != "" {
		t.Fatalf("no decision yet, got %q/%q", detail.Decision, detail.DecisionImpactDigest)
	}
	op.decision = &types.Approval{IncidentID: "inc-1", Decision: types.ApprovalApproved, ParkEpoch: 2, ImpactDigest: "sha256:old"}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1", "secret", ""))
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Decision != "approved" || detail.DecisionImpactDigest != "sha256:old" || detail.ImpactPreviewDigest != "sha256:cur" {
		t.Fatalf("detail = %+v, want the decision row's own digest beside the current preview's", detail)
	}
}

func TestImpactPreviewRouteIs503WithoutTheBackend(t *testing.T) {
	handler := operatorServer(&fakeOperator{}, "secret")
	for _, r := range []*http.Request{
		operatorRequest("GET", "/api/v1/incidents/inc-1/impact-preview", "secret", ""),
		func() *http.Request {
			req := operatorRequest("POST", "/api/v1/incidents/inc-1/impact-preview/recompute", "secret", `{"actor":"alice"}`)
			req.Header.Set("Idempotency-Key", "k")
			return req
		}(),
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503", r.Method, r.URL.Path, rec.Code)
		}
	}
	// A plain detail still works and simply carries no digest.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("GET", "/api/v1/incidents/inc-1", "secret", ""))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "impact_preview_digest") {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecomputeRequiresAnIdempotencyKeyAndForwardsTheActor(t *testing.T) {
	op, handler := impactServer(map[int]*types.ImpactPreview{0: {IncidentID: "inc-1", Epoch: 2, Digest: "sha256:new", Payload: []byte(`{"digest":"sha256:new"}`)}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/impact-preview/recompute", "secret", `{"actor":"alice"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("without key = %d, want 400", rec.Code)
	}
	req := operatorRequest("POST", "/api/v1/incidents/inc-1/impact-preview/recompute", "secret", `{"actor":"alice"}`)
	req.Header.Set("Idempotency-Key", "key-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sha256:new") {
		t.Fatalf("recompute = %d %s", rec.Code, rec.Body.String())
	}
	if len(op.recomputed) != 1 || op.recomputed[0] != "inc-1:token:alice:key-1" {
		t.Fatalf("recomputed = %v", op.recomputed)
	}
	req = operatorRequest("POST", "/api/v1/incidents/inc-1/impact-preview/recompute", "secret", `{"actor":"alice"}`)
	req.Header.Set("Idempotency-Key", "key-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay header missing: %v", rec.Header())
	}
}

func TestDecisionForwardsTheImpactDigest(t *testing.T) {
	op, handler := impactServer(nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest("POST", "/api/v1/incidents/inc-1/approve", "secret", `{"actor":"alice","park_epoch":2,"impact_digest":"sha256:seen"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if last := op.decisions[len(op.decisions)-1]; last != "inc-1:approved:token:alice:2:sha256:seen" {
		t.Fatalf("decision = %q, want the digest forwarded", last)
	}
}
