// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type noNodesOperator struct{ *fakeOperator }

func (noNodesOperator) Nodes(context.Context) ([]*types.Node, error) { return nil, nil }

// An empty list is a JSON array, never null, so a client reads "no items"
// the same way it reads items.
func TestListEndpointsAnswerAnEmptyArray(t *testing.T) {
	plain := operatorServer(noNodesOperator{&fakeOperator{}}, "secret")
	for _, path := range []string{"/api/v1/incidents", "/api/v1/incidents?node=gpu-a", "/api/v1/nodes"} {
		rec := httptest.NewRecorder()
		plain.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("GET %s = %d %q, want 200 []", path, rec.Code, rec.Body.String())
		}
	}

	handler, closeStore := newOperationalHTTPServer(t)
	defer closeStore()
	for _, path := range []string{
		"/api/v1/candidates", "/api/v1/previews", "/api/v1/health-checks", "/api/v1/nodes/gpu-a/health-checks",
		"/api/v1/simulations", "/api/v1/autonomy/plans", "/api/v1/audit-events", "/api/v1/runtime-contract-qualifications",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, path, "secret", ""))
		var page struct {
			Items json.RawMessage `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || string(page.Items) != "[]" {
			t.Fatalf("GET %s = %d %q, want 200 with items []", path, rec.Code, rec.Body.String())
		}
	}
}
