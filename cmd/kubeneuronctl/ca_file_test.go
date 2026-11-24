// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// privateCAServer serves the status routes with a certificate from its own
// CA, as a controller with spec.tls.publicServerSecretRef does, and returns
// that CA as PEM.
func privateCAServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = fmt.Fprint(w, `{"version":"v1.0.0"}`)
		case "/api/v1/incidents":
			_, _ = fmt.Fprint(w, `[]`)
		case "/api/v1/pause":
			_, _ = fmt.Fprint(w, `{"paused":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	return server, path
}

func TestCAFileTrustsPrivateControllerCertificate(t *testing.T) {
	server, caFile := privateCAServer(t)
	if _, err := runRoot(t, "--server", server.URL, "--token", "t", "status"); err == nil {
		t.Fatal("status trusted a private CA without --ca-file")
	}
	out, err := runRoot(t, "--server", server.URL, "--token", "t", "--ca-file", caFile, "status")
	if err != nil {
		t.Fatalf("status with --ca-file: %v", err)
	}
	if !strings.HasPrefix(out, "controller: v1.0.0\n") {
		t.Fatalf("unexpected status output:\n%s", out)
	}
	t.Setenv("KUBENEURONCTL_CA_FILE", caFile)
	if _, err := runRoot(t, "--server", server.URL, "--token", "t", "status"); err != nil {
		t.Fatalf("status with KUBENEURONCTL_CA_FILE: %v", err)
	}
}

func TestCAFileErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"missing": {filepath.Join(dir, "absent.pem"), "CA file: open " + filepath.Join(dir, "absent.pem")},
		"no PEM":  {empty, "CA file " + empty + " contains no PEM certificate"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runRoot(t, "--server", "https://127.0.0.1:1", "--ca-file", tc.path, "status")
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %v, want prefix %q", err, tc.want)
			}
		})
	}
}
