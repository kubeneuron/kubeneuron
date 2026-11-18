// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type acceleratorHealthOperator struct {
	*fakeOperator
	asked []string
}

func (f *acceleratorHealthOperator) AcceleratorHealth(_ context.Context, node string) types.AcceleratorHealthView {
	f.asked = append(f.asked, node)
	return types.AcceleratorHealthView{ProfileVersion: "v1", Rows: []types.AcceleratorHealthRow{{Node: "n1", Source: "dra", State: "no-signal"}}}
}

func TestAcceleratorHealthEndpoint(t *testing.T) {
	op := &acceleratorHealthOperator{fakeOperator: &fakeOperator{}}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, "secret")
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest("GET", types.AcceleratorHealthPath+"?node=n1", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest("GET", types.AcceleratorHealthPath+"?node=n1", "secret", ""))
	var view types.AcceleratorHealthView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil || len(view.Rows) != 1 || view.Rows[0].State != "no-signal" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if len(op.asked) != 1 || op.asked[0] != "n1" {
		t.Fatalf("asked %q", op.asked)
	}

	// The read is held by the leader: a standby has none to serve.
	s.SetReadyCheck(func() bool { return false })
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest("GET", types.AcceleratorHealthPath, "secret", ""))
	if rec.Code != http.StatusServiceUnavailable || len(op.asked) != 1 {
		t.Fatalf("standby: status %d, asked %q", rec.Code, op.asked)
	}

	// A backend without the view says it is unavailable.
	rec = httptest.NewRecorder()
	operatorServer(&fakeOperator{}, "secret").ServeHTTP(rec, operatorRequest("GET", types.AcceleratorHealthPath, "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no backend: %d", rec.Code)
	}
}
