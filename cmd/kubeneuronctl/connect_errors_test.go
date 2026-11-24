// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

// goodToken is the token connectAPI accepts; no error may show it.
const goodToken = "operator-secret-good"

// connectAPI answers like the controller's operator API: auth names the
// failure it returns before any route, standby and oldRelease the leader
// and route behaviour.
func connectAPI(auth string, standby, oldRelease bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch auth {
		case "disabled":
			w.Header().Set(httpapi.UnavailableHeader, httpapi.UnavailableOperatorAPIDisabled)
			http.Error(w, "operator API disabled: no API token configured", http.StatusServiceUnavailable)
			return
		case "forbidden":
			http.Error(w, "operator authentication failed", http.StatusForbidden)
			return
		case "throttled":
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kubeneuron-operator"`)
			http.Error(w, "operator authentication failed", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/version":
			switch {
			case oldRelease:
				http.NotFound(w, r)
			case standby:
				w.Header().Set(httpapi.UnavailableHeader, httpapi.UnavailableStandby)
				http.Error(w, "standby: not the elected leader", http.StatusServiceUnavailable)
			default:
				_, _ = fmt.Fprint(w, `{"version":"v1.0.0"}`)
			}
		case "/api/v1/incidents":
			_, _ = fmt.Fprint(w, `[]`)
		case "/api/v1/pause":
			_, _ = fmt.Fprint(w, `{"paused":false}`)
		default:
			http.NotFound(w, r)
		}
	})
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-72 * time.Hour), NotAfter: time.Now().Add(72 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return testCA{cert, key, file}
}

func (ca testCA) serve(t *testing.T, ips []net.IP, names []string, notAfter time.Time) *httptest.Server {
	t.Helper()
	return ca.serveFor(t, ips, names, notAfter, x509.ExtKeyUsageServerAuth)
}

func (ca testCA) serveFor(t *testing.T, ips []net.IP, names []string, notAfter time.Time, usage x509.ExtKeyUsage) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kubeneuron-controller"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter, IPAddresses: ips, DNSNames: names,
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(connectAPI("", false, false))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

var (
	serverAddress = regexp.MustCompile(`127\.0\.0\.1:\d+`)
	certTimes     = regexp.MustCompile(`current time \S+ is after \S+`)
	// The untrusted-CA cause is the platform verifier's own text.
	platformTrust = regexp.MustCompile(`(?m)^(cause: .*tls: failed to verify certificate: ).*(not trusted|unknown authority).*$`)
)

func TestConnectionErrorsSayWhatToDoNext(t *testing.T) {
	ca := newTestCA(t)
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	plain := func(h http.Handler) string {
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		return server.URL
	}
	tlsServer := httptest.NewTLSServer(connectAPI("", false, false))
	t.Cleanup(tlsServer.Close)

	cases := []struct {
		name string
		args []string
	}{
		{"no port-forward", []string{"--server", "http://" + closed, "--token", goodToken, "status"}},
		{"no token", []string{"--server", plain(connectAPI("", false, false)), "status"}},
		{"wrong token", []string{"--server", plain(connectAPI("", false, false)), "--token", "operator-secret-bad", "incidents"}},
		{"operator API disabled", []string{"--server", plain(connectAPI("disabled", false, false)), "--token", goodToken, "status"}},
		{"token without permission", []string{"--server", plain(connectAPI("forbidden", false, false)), "--token", goodToken, "incidents"}},
		{"too many failed sign-ins", []string{"--server", plain(connectAPI("throttled", false, false)), "--token", "operator-secret-bad", "status"}},
		{"standby", []string{"--server", plain(connectAPI("", true, false)), "--token", goodToken, "status"}},
		{"older controller", []string{"--server", plain(connectAPI("", false, true)), "--token", goodToken, "pools"}},
		{"expired certificate", []string{"--server", ca.serve(t, loopback, nil, time.Now().Add(-time.Hour)).URL, "--ca-file", ca.file, "--token", goodToken, "status"}},
		{"certificate for another name", []string{"--server", ca.serve(t, nil, []string{"kubeneuron-controller.kube-neuron.svc"}, time.Now().Add(time.Hour)).URL, "--ca-file", ca.file, "--token", goodToken, "status"}},
		{"certificate not for server use", []string{"--server", ca.serveFor(t, loopback, nil, time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth).URL, "--ca-file", ca.file, "--token", goodToken, "status"}},
		{"untrusted CA", []string{"--server", ca.serve(t, loopback, nil, time.Now().Add(time.Hour)).URL, "--token", goodToken, "status"}},
		{"https to a plain-HTTP controller", []string{"--server", "https://" + strings.TrimPrefix(plain(connectAPI("", false, false)), "http://"), "--token", goodToken, "status"}},
		{"http to an HTTPS controller", []string{"--server", "http://" + tlsServer.Listener.Addr().String(), "--token", goodToken, "status"}},
	}
	var report strings.Builder
	for _, tc := range cases {
		_, err := runRoot(t, tc.args...)
		if err == nil {
			t.Fatalf("%s: command succeeded", tc.name)
		}
		fmt.Fprintf(&report, "## %s\nerror: %s\n\n", tc.name, err)
	}
	if strings.Contains(report.String(), "operator-secret") {
		t.Fatalf("an error shows the API token:\n%s", report.String())
	}
	got := serverAddress.ReplaceAllString(report.String(), "<server>")
	got = certTimes.ReplaceAllString(got, "current time <now> is after <not-after>")
	got = platformTrust.ReplaceAllString(got, "${1}<platform verifier text>")
	checkGolden(t, "connect-errors.golden", []byte(strings.TrimRight(got, "\n")+"\n"))
}

func TestConnectionErrorsOmitURLCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	_, err = runRoot(t, "--server", "http://someone:secret-pass@"+closed, "--token", "secret-token", "status")
	if err == nil {
		t.Fatal("status reached a closed port")
	}
	for _, leaked := range []string{"someone", "secret-pass", "secret-token"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error shows %q:\n%s", leaked, err)
		}
	}
	if !strings.Contains(err.Error(), "Cannot reach the controller at http://"+closed+".") {
		t.Fatalf("unexpected error:\n%s", err)
	}
}

func TestOnlyTheFirstLineOfAnErrorBodyIsShown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "operator authentication failed\nsecond line\n"+strings.Repeat("x", 300), http.StatusForbidden)
	}))
	defer server.Close()
	_, err := runRoot(t, "--server", server.URL, "--token", goodToken, "status")
	if err == nil || !strings.HasSuffix(err.Error(), "\ncause: 403 Forbidden: operator authentication failed") {
		t.Fatalf("unexpected error:\n%v", err)
	}
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("y", 300), http.StatusForbidden)
	}))
	defer long.Close()
	_, err = runRoot(t, "--server", long.URL, "--token", goodToken, "status")
	if err == nil || !strings.HasSuffix(err.Error(), "\ncause: 403 Forbidden: "+strings.Repeat("y", 200)+"…") {
		t.Fatalf("unexpected error:\n%v", err)
	}
}

func TestErrorBodyIsCutOnACharacterBoundary(t *testing.T) {
	line := firstLine(strings.Repeat("я", 250))
	if want := strings.Repeat("я", 200) + "…"; line != want {
		t.Fatalf("firstLine = %q, want %q", line, want)
	}
}
