// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"github.com/kubeneuron/kubeneuron/internal/httpapi"
)

// connectError is a failed controller request explained for a person: what
// happened and what to do next, followed by the underlying cause on its own
// line. It never carries the token, request headers or URL credentials.
type connectError struct {
	message string
	cause   string
}

func (e *connectError) Error() string {
	return e.message + "\ncause: " + e.cause
}

const tokenCommand = `export KUBENEURONCTL_TOKEN=$(kubectl -n kube-neuron get secret kubeneuron-operator-api-token -o jsonpath='{.data.token}' | base64 -d)`

// explainTransport explains a request that got no HTTP response. Errors it
// does not recognise are returned unchanged.
func explainTransport(base, command string, err error) error {
	cause := redactedCause(err)
	var expired x509.CertificateInvalidError
	var hostname x509.HostnameError
	var unknownAuthority x509.UnknownAuthorityError
	var verification *tls.CertificateVerificationError
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &expired) && expired.Reason == x509.Expired:
		return &connectError{"The controller's certificate has expired or is not valid yet, or this computer's clock is wrong. Check the clock and the certificate's validity dates; if the certificate has expired, renew it where HTTPS terminates", cause}
	case errors.As(err, &hostname):
		return &connectError{fmt.Sprintf("The controller's certificate is not valid for %s. Use an address the certificate lists in --server, or add this name to the certificate", hostname.Host), cause}
	case errors.As(err, &unknownAuthority), runtime.GOOS == "darwin" && errors.As(err, &verification) && !errors.As(err, new(x509.CertificateInvalidError)):
		// The macOS verifier reports some untrusted chains only as a
		// verification error with the system's own text.
		return &connectError{`The controller's certificate is signed by a CA this computer does not trust. Save the CA from the controller's TLS Secret and pass it with --ca-file: kubectl -n <namespace> get secret <secret> -o jsonpath='{.data.ca\.crt}' | base64 -d > kubeneuron-ca.crt`, cause}
	case errors.As(err, &verification):
		return &connectError{"The controller's certificate could not be verified. Check the certificate where HTTPS terminates", cause}
	case strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		// net/http reports this with an unexported error value.
		return &connectError{"The controller answered over plain HTTP. Use an http:// address in --server only through a port-forward or another trusted connection: over plain HTTP the API token is sent unencrypted", cause}
	case errors.As(err, &dns), errors.As(err, &op) && op.Op == "dial":
		return &connectError{fmt.Sprintf("Cannot reach the controller at %s. If it runs in a cluster, start a port-forward in another terminal first; for a default install: kubectl -n kube-neuron port-forward service/kubeneuron-controller 8080:8080", redactURL(base)), cause}
	}
	return err
}

// explainResponse explains an error status the person can act on without
// reading the controller's code; it returns nil for every other status, which
// is reported as before.
func explainResponse(resp *http.Response, body, command string, sentToken bool) error {
	cause := resp.Status
	if line := firstLine(body); line != "" {
		cause += ": " + line
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		if !sentToken {
			return &connectError{"No API token was given. Read it from the installation's Secret; for a default install: " + tokenCommand, cause}
		}
		return &connectError{"The controller did not accept the API token. Read the current token from the installation's Secret; for a default install: " + tokenCommand, cause}
	case http.StatusForbidden:
		return &connectError{"The controller recognised the credentials, but they do not allow this request. Use a token whose role allows this command", cause}
	case http.StatusTooManyRequests:
		if firstLine(body) == httpapi.TooManyFailedAuthentications {
			return &connectError{"The controller is refusing requests from this address after repeated failed sign-ins. Wait one minute, check the token, then try again", cause}
		}
	case http.StatusServiceUnavailable:
		switch resp.Header.Get(httpapi.UnavailableHeader) {
		case httpapi.UnavailableStandby:
			return &connectError{"This controller Pod is not the elected leader. Connect through the controller Service", cause}
		case httpapi.UnavailableOperatorAPIDisabled:
			return &connectError{"The controller's operator API is off because the controller has no API token configured. Check the operator API token Secret of the installation and the controller's logs", cause}
		}
	case http.StatusNotFound:
		// The mux's own 404, not a handler's: the route itself is missing.
		if strings.TrimSpace(body) == "404 page not found" {
			return &connectError{fmt.Sprintf("This controller does not support %s; it is from an older release. Upgrade the controller, or use the kubeneuronctl of the controller's release", command), cause}
		}
	case http.StatusBadRequest:
		// A Go TLS server's reply to a plain-HTTP request.
		if strings.Contains(body, "Client sent an HTTP request to an HTTPS server") {
			return &connectError{"The controller expects HTTPS. Use an https:// address in --server", cause}
		}
	}
	return nil
}

func firstLine(body string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > 200 {
		line = string(runes[:200]) + "…"
	}
	return line
}

// redactedCause is err's text with any URL credentials removed.
func redactedCause(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		copied := *urlErr
		copied.URL = redactURL(urlErr.URL)
		return copied.Error()
	}
	return err.Error()
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}
