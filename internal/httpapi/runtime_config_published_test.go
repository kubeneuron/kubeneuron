// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type publishingOperator struct {
	*fakeOperator
	digest     string
	at         time.Time
	configured bool
	err        error
}

func (p publishingOperator) PublishedConfig(context.Context) (string, time.Time, bool, error) {
	return p.digest, p.at, p.configured, p.err
}

// /api/v1/runtime-config says what the operator published next to what the
// controller loaded, read when asked; without an operator it says nothing.
func TestRuntimeConfigNamesThePublishedConfiguration(t *testing.T) {
	at := time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		op   OperatorBackend
		want *PublishedConfigInfo
	}{
		"published":   {publishingOperator{&fakeOperator{}, "newer", at, true, nil}, &PublishedConfigInfo{Digest: "newer", At: at}},
		"unreadable":  {publishingOperator{&fakeOperator{}, "", time.Time{}, true, errors.New("forbidden")}, &PublishedConfigInfo{Error: "forbidden"}},
		"no operator": {publishingOperator{&fakeOperator{}, "", time.Time{}, false, nil}, nil},
		"no reader":   {&fakeOperator{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(&registrationBackend{})
			s.EnableOperatorAPI(tc.op, "secret")
			s.SetRuntimeConfigInfo(RuntimeConfigInfo{SourceDigest: "loaded", ExecutionMode: "enabled"})
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, operatorRequest(http.MethodGet, "/api/v1/runtime-config", "secret", ""))
			var got RuntimeConfigInfo
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
				t.Fatalf("GET = %d %q", rec.Code, rec.Body.String())
			}
			if got.SourceDigest != "loaded" {
				t.Fatalf("source_digest %q, want the loaded one", got.SourceDigest)
			}
			switch {
			case tc.want == nil && got.Published != nil:
				t.Fatalf("published %+v, want none", got.Published)
			case tc.want != nil && (got.Published == nil || *got.Published != *tc.want):
				t.Fatalf("published %+v, want %+v", got.Published, tc.want)
			}
		})
	}
}
