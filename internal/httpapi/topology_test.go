// SPDX-License-Identifier: LicenseRef-KubeNeuron

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/decision"
	"github.com/kubeneuron/kubeneuron/internal/operations"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/topology"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

var topologyNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// topologyHTTPBuilder is a deterministic stand-in for the controller adapter:
// gpu-a is a time-sliced node, gpu-b has no report, ghost does not exist.
func topologyHTTPBuilder(_ context.Context, node string) (topology.Classification, error) {
	switch node {
	case "ghost":
		return topology.Classification{}, store.ErrNotFound
	case "broken":
		return topology.Classification{}, errors.Join(operations.ErrUnavailable, errors.New("accelerator report store is not configured"))
	}
	snapshot := topology.Snapshot{
		Node: node, NodeUID: "node-uid", EvaluatedAt: topologyNow,
		Resources: topology.Resources{Source: topology.ResourceSourceNode, Capacity: map[string]int64{"nvidia.com/gpu": 4}},
		Labels:    map[string]string{topology.LabelGPUReplicas: "4", "kubeneuron.io/secret-looking": "token"},
		DRA:       topology.DRAObservation{State: topology.DRADisabled},
	}
	if node == "gpu-a" {
		snapshot.Report = &topology.Report{ObservedAt: topologyNow.Add(-time.Minute), TopologySafety: types.AcceleratorTopologyVerifiedUnpartitioned, Readiness: types.AcceleratorReadinessReady, PhysicalDevices: 1}
	}
	return topology.Classify(snapshot), nil
}

func newTopologyHTTPServer(t *testing.T, builder operations.TopologyBuilder) http.Handler {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	manager := operations.New(operations.Options{
		BuildTopology: builder,
		BuildSnapshot: func(_ context.Context, node string, request decision.Request) (decision.Snapshot, error) {
			if node == "ghost" {
				return decision.Snapshot{}, store.ErrNotFound
			}
			return decision.Snapshot{Version: decision.EvaluatorVersion, EvaluatedAt: topologyNow, Node: types.Node{Name: node, UID: "node-uid"}, Request: request}, nil
		},
		Resources: st,
		Workflow:  st,
	})
	op := &operationalOperator{fakeOperator: &fakeOperator{}, manager: manager}
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(op, "secret")
	return server.Routes()
}

func TestNodeTopologyRouteServesClassification(t *testing.T) {
	handler := newTopologyHTTPServer(t, topologyHTTPBuilder)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/topology", "secret", ""))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("topology = %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"version":"topology-classification/v1"`, `"node":"gpu-a"`, `"node_uid":"node-uid"`, `"topology":"time-sliced"`,
		`"device_actions":"refused"`, `"reasons":["TimeSliced"]`, `"attribution":"device-wide"`,
		`"dra":{"state":"disabled","resource_slices":0,"devices":0,"pods_inspected":false,"claim_pods":0,"direct_claims":0,"template_claims":0}`, `"observed_at":"2026-10-21T12:00:00Z"`, `"inputs_digest":"sha256:`,
		`"gpu_replicas_label":"4"`, `"whole_devices":4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-looking") || strings.Contains(body, "token") {
		t.Fatalf("unrelated node labels leaked into the topology response:\n%s", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-b/topology", "secret", ""))
	var decoded topology.Classification
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("gpu-b = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if decoded.Topology != topology.TopologyTimeSliced || !decoded.Has(topology.ReasonReportMissing) || !decoded.Has(topology.ReasonTimeSliced) {
		t.Fatalf("missing report classification = %s", decoded.Summary)
	}
}

func TestNodeTopologyRouteErrorMapping(t *testing.T) {
	handler := newTopologyHTTPServer(t, topologyHTTPBuilder)
	for path, want := range map[string]int{
		"/api/v1/nodes/ghost/topology":  http.StatusNotFound,
		"/api/v1/nodes/broken/topology": http.StatusServiceUnavailable,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		if rec.Code != want {
			t.Errorf("%s = %d %s, want %d", path, rec.Code, rec.Body.String(), want)
		}
	}
	// No operations provider at all, and a manager without the builder, are
	// both unavailable rather than an empty but healthy-looking answer.
	rec := httptest.NewRecorder()
	operatorServer(&fakeOperator{}, "secret").ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/topology", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without operations provider = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	newTopologyHTTPServer(t, nil).ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/topology", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "topology classifier is unavailable") {
		t.Fatalf("without builder = %d %s", rec.Code, rec.Body.String())
	}
}

// TestNodeTopologyRouteIsOperatorOnlyAndReadOnly pins the trust surface: the
// route lives on the existing operator listener, needs operator
// authorization, and accepts no mutating verb.
func TestNodeTopologyRouteIsOperatorOnlyAndReadOnly(t *testing.T) {
	handler := newTopologyHTTPServer(t, topologyHTTPBuilder)
	for _, token := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/topology", token, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", token, rec.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(method, "/api/v1/nodes/gpu-a/topology", "secret", `{"topology":"whole-device"}`))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405: the topology surface is read-only", method, rec.Code)
		}
	}
	auth := &fakeOperatorAuthenticator{identity: OperatorIdentity{Actor: "viewer", Method: "kubernetes"}}
	server := New(&registrationBackend{})
	server.EnableOperatorAPI(&operationalOperator{fakeOperator: &fakeOperator{}, manager: operations.New(operations.Options{BuildTopology: topologyHTTPBuilder})}, "secret")
	server.SetOperatorAuthenticator(auth)
	rec := httptest.NewRecorder()
	server.Routes().ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/topology", "per-caller", ""))
	if rec.Code != http.StatusOK || len(auth.verbs) != 1 || auth.verbs[0] != "get" {
		t.Fatalf("per-caller read = %d verbs=%v", rec.Code, auth.verbs)
	}
}

// TestNodeReadinessEmbedsTopology pins the readiness contract extension: the
// single-node answer carries the classification under `topology`, and the
// decision beside it is unchanged by it.
func TestNodeReadinessEmbedsTopology(t *testing.T) {
	handler := newTopologyHTTPServer(t, topologyHTTPBuilder)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/readiness", "secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Node     string                   `json:"node"`
		Decision decision.Result          `json:"decision"`
		Topology *topology.Classification `json:"topology"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Topology == nil || body.Topology.Topology != topology.TopologyTimeSliced || body.Topology.DeviceActions != topology.DeviceActionsRefused {
		t.Fatalf("readiness topology = %+v", body.Topology)
	}
	if body.Decision.State != decision.StateUnknown {
		t.Fatalf("decision = %+v: topology must not alter the readiness decision", body.Decision)
	}
	// A controller without the builder serves readiness without the field.
	rec = httptest.NewRecorder()
	newTopologyHTTPServer(t, nil).ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/nodes/gpu-a/readiness", "secret", ""))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"topology"`) {
		t.Fatalf("readiness without builder = %d %s", rec.Code, rec.Body.String())
	}
}
