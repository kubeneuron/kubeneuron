package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type recoveryReportOperator struct {
	*fakeOperator
	windows []time.Duration
	report  *types.RecoveryReport
	err     error
}

func (f *recoveryReportOperator) RecoveryReport(_ context.Context, window time.Duration) (*types.RecoveryReport, error) {
	f.windows = append(f.windows, window)
	if f.err != nil {
		return nil, f.err
	}
	return f.report, nil
}

func TestRecoveryReportIsAuthenticatedAndFailsClosed(t *testing.T) {
	// An older backend that cannot compute the report must say so: an empty
	// report reads as "nothing broke", which is a lie a capacity owner would
	// quote.
	plain := operatorServer(&fakeOperator{}, "secret")
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/report/recovery", "secret", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing report backend status = %d, want 503", rec.Code)
	}

	op := &recoveryReportOperator{fakeOperator: &fakeOperator{}, report: &types.RecoveryReport{}}
	handler := operatorServer(op, "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/report/recovery", "wrong", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	if len(op.windows) != 0 {
		t.Fatalf("unauthenticated request reached the backend: %v", op.windows)
	}
}

func TestRecoveryReportWindowParsing(t *testing.T) {
	op := &recoveryReportOperator{fakeOperator: &fakeOperator{}, report: &types.RecoveryReport{DegradedGPUHours: 3.5}}
	handler := operatorServer(op, "secret")

	tests := []struct {
		query      string
		wantStatus int
		wantWindow time.Duration
	}{
		{query: "", wantStatus: http.StatusOK, wantWindow: defaultReportWindow},
		{query: "?window=720h", wantStatus: http.StatusOK, wantWindow: 720 * time.Hour},
		{query: "?window=0s", wantStatus: http.StatusBadRequest},
		{query: "?window=-24h", wantStatus: http.StatusBadRequest},
		{query: "?window=30d", wantStatus: http.StatusBadRequest}, // days are the CLI's spelling, not the API's
		{query: "?window=9000h", wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		op.windows = nil
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/report/recovery"+tc.query, "secret", ""))
		if rec.Code != tc.wantStatus {
			t.Fatalf("%q status = %d, want %d", tc.query, rec.Code, tc.wantStatus)
		}
		if tc.wantStatus != http.StatusOK {
			if len(op.windows) != 0 {
				t.Fatalf("%q reached the backend with %v", tc.query, op.windows)
			}
			continue
		}
		if len(op.windows) != 1 || op.windows[0] != tc.wantWindow {
			t.Fatalf("%q backend windows = %v, want [%s]", tc.query, op.windows, tc.wantWindow)
		}
		var got types.RecoveryReport
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("%q decode: %v", tc.query, err)
		}
		if got.DegradedGPUHours != 3.5 {
			t.Fatalf("%q degraded GPU-hours = %v, want 3.5", tc.query, got.DegradedGPUHours)
		}
	}
}

func TestRecoveryReportBackendFailureIsAServerError(t *testing.T) {
	op := &recoveryReportOperator{fakeOperator: &fakeOperator{}, err: errors.New("store unavailable")}
	handler := operatorServer(op, "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/report/recovery", "secret", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("backend failure status = %d, want 500", rec.Code)
	}
}
