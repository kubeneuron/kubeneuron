// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type poolOperator struct {
	*fakeOperator
	reads int
}

func (p *poolOperator) PoolAvailability() types.PoolAvailabilityView {
	p.reads++
	return types.PoolAvailabilityView{Pools: []types.PoolAvailability{{Pool: "workers", Cloud: "GKE", Status: "unknown", Reason: "not checked yet"}}}
}

func TestPoolAvailabilityEndpoint(t *testing.T) {
	op := &poolOperator{fakeOperator: &fakeOperator{}}
	s := New(&registrationBackend{})
	s.EnableOperatorAPI(op, "secret")
	h := s.Routes()
	for _, tc := range []struct {
		token  string
		leader bool
		code   int
	}{{"", true, http.StatusUnauthorized}, {"secret", false, http.StatusServiceUnavailable}, {"secret", true, http.StatusOK}} {
		s.SetReadyCheck(func() bool { return tc.leader })
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, operatorRequest("GET", types.PoolAvailabilityPath, tc.token, ""))
		if rec.Code != tc.code {
			t.Fatalf("got %d want %d", rec.Code, tc.code)
		}
		if rec.Code == http.StatusOK {
			var view types.PoolAvailabilityView
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || len(view.Pools) != 1 || view.Pools[0].Status != "unknown" {
				t.Fatalf("response: %s", rec.Body)
			}
		}
	}
	if op.reads != 1 {
		t.Fatalf("snapshot reads: %d", op.reads)
	}
}
