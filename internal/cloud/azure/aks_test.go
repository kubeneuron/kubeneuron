// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

func permissionsServer(t *testing.T, next func(base string) string) (*sdkAPI, *[]string) {
	t.Helper()
	var queries []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if len(queries) == 1 {
			_, _ = fmt.Fprintf(w, `{"value":[{"actions":["a/read"]}],"nextLink":%q}`, next(srv.URL))
			return
		}
		_, _ = fmt.Fprint(w, `{"value":[{"actions":["b/write"]}]}`)
	}))
	t.Cleanup(srv.Close)
	return &sdkAPI{pipeline: runtime.NewPipeline("test", "v1", runtime.PipelineOptions{}, nil), endpoint: srv.URL}, &queries
}

func TestPermissionsFollowsNextLinkAsGiven(t *testing.T) {
	api, queries := permissionsServer(t, func(base string) string {
		return base + "/scope/providers/Microsoft.Authorization/permissions?api-version=2022-04-01&$skiptoken=x"
	})
	perms, err := api.Permissions(context.Background(), "/scope")
	if err != nil {
		t.Fatalf("Permissions: %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("permissions = %v, want both pages", perms)
	}
	if (*queries)[0] != "api-version="+permissionsAPIVersion {
		t.Errorf("first query = %q", (*queries)[0])
	}
	if got := (*queries)[1]; strings.Count(got, "api-version") != 1 || !strings.Contains(got, "skiptoken=x") {
		t.Errorf("next page query = %q, want the link unchanged", got)
	}
}

func TestPermissionsRefusesNextLinkOffTheEndpoint(t *testing.T) {
	var stray int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		stray++
		_, _ = fmt.Fprint(w, `{"value":[]}`)
	}))
	t.Cleanup(other.Close)
	otherHost := strings.TrimPrefix(other.URL, "http://")
	for name, link := range map[string]func(string) string{
		"other host":       func(string) string { return other.URL + "/next" },
		"endpoint as user": func(base string) string { return base + "@" + otherHost + "/next" },
	} {
		t.Run(name, func(t *testing.T) {
			stray = 0
			api, queries := permissionsServer(t, link)
			if _, err := api.Permissions(context.Background(), "/scope"); err == nil {
				t.Fatal("Permissions followed a next page off the endpoint")
			}
			if len(*queries) != 1 || stray != 0 {
				t.Errorf("requests: endpoint %d, elsewhere %d; want only the first", len(*queries), stray)
			}
		})
	}
}
