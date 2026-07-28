package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type acceleratorProfileBackend struct {
	registrationBackend
	profile *types.AgentAcceleratorObservationProfile
	err     error
	calls   int
	node    string
	vendor  types.AcceleratorVendor
}

func (b *acceleratorProfileBackend) AcceleratorObservationProfile(_ context.Context, node string, vendor types.AcceleratorVendor) (*types.AgentAcceleratorObservationProfile, error) {
	b.calls++
	b.node = node
	b.vendor = vendor
	return b.profile, b.err
}

func TestAcceleratorObservationProfileRouteReturnsOnlyAuthenticatedNodeSelection(t *testing.T) {
	backend := &acceleratorProfileBackend{profile: &types.AgentAcceleratorObservationProfile{
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:" + strings.Repeat("a", 64),
		ProfileUID:        "nvidia-profile-uid",
		ProfileGeneration: 2,
		RuntimeVersion:    "dcgm-4.1",
	}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, types.AgentAcceleratorProfilePath+"?vendor=nvidia", nil)
	authenticatedAgentRoutes(backend).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", response.Code, response.Body.String())
	}
	if backend.calls != 1 || backend.node != "node-a" || backend.vendor != types.AcceleratorVendorNVIDIA {
		t.Fatalf("profile lookup = calls=%d node=%q vendor=%q, want authenticated node-a NVIDIA", backend.calls, backend.node, backend.vendor)
	}
	var got types.AgentAcceleratorObservationProfile
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode profile response: %v", err)
	}
	if got != *backend.profile {
		t.Fatalf("profile = %+v, want %+v", got, *backend.profile)
	}

	// The public listener must never expose the agent-only configuration route.
	response = httptest.NewRecorder()
	New(backend).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("public route status = %d, want 404", response.Code)
	}
}

func TestAcceleratorObservationProfileRouteFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		backend    Backend
		path       string
		wantStatus int
	}{
		{
			name:       "no matching profile is observation-only",
			backend:    &acceleratorProfileBackend{},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "backend unavailable",
			backend:    &registrationBackend{},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "ambiguous or unavailable selection",
			backend:    &acceleratorProfileBackend{err: errors.New("ambiguous selector")},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "wrong vendor response",
			backend: &acceleratorProfileBackend{profile: &types.AgentAcceleratorObservationProfile{
				Vendor:            types.AcceleratorVendorAMD,
				ProfileDigest:     "sha256:" + strings.Repeat("c", 64),
				ProfileUID:        "amd-profile-uid",
				ProfileGeneration: 1,
				RuntimeVersion:    "dcgm-4.1",
			}},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "malformed digest response",
			backend: &acceleratorProfileBackend{profile: &types.AgentAcceleratorObservationProfile{
				Vendor:            types.AcceleratorVendorNVIDIA,
				ProfileDigest:     "sha256:not-valid",
				ProfileUID:        "nvidia-profile-uid",
				ProfileGeneration: 1,
				RuntimeVersion:    "dcgm-4.1",
			}},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia",
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "missing vendor",
			backend:    &acceleratorProfileBackend{},
			path:       types.AgentAcceleratorProfilePath,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown vendor",
			backend:    &acceleratorProfileBackend{},
			path:       types.AgentAcceleratorProfilePath + "?vendor=unknown",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "extra query rejected",
			backend:    &acceleratorProfileBackend{},
			path:       types.AgentAcceleratorProfilePath + "?vendor=nvidia&node=node-b",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			authenticatedAgentRoutes(tt.backend).ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
}
