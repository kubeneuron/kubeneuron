// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const secretPath = "/services/T000/B000/XXXXsecretXXXX"

func TestRedactURLKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://hooks.slack.com" + secretPath:            "https://hooks.slack.com/REDACTED",
		"https://user:pw@example.com:8443/hook?token=abc": "https://example.com:8443/REDACTED",
		"http://10.0.0.1/x#frag":                          "http://10.0.0.1/REDACTED",
		"not a url" + secretPath:                          "REDACTED",
		"":                                                "REDACTED",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactTextScrubsEveryURL(t *testing.T) {
	in := `Post "https://u:p@hooks.slack.com/services/a/b?x=1": dial; redirect to http://other.test/path/key`
	got := RedactText(in)
	for _, leak := range []string{"/services", "x=1", "u:p", "/path/key"} {
		if strings.Contains(got, leak) {
			t.Fatalf("RedactText left %q in %q", leak, got)
		}
	}
	if !strings.Contains(got, "https://hooks.slack.com/REDACTED") || !strings.Contains(got, "http://other.test/REDACTED") {
		t.Fatalf("RedactText dropped the hosts: %q", got)
	}
}

// unreachableURL returns a URL whose server is already closed, so a POST
// fails in the transport with a *url.Error that repeats the full URL.
func unreachableURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u + secretPath + "?token=querysecret"
}

func postErr(t *testing.T, ctx context.Context, target string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("post to a closed server succeeded")
	}
	return err
}

func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	for _, leak := range []string{"/services/", "XXXXsecretXXXX", "querysecret"} {
		if strings.Contains(s, leak) {
			t.Fatalf("%s leaks %q: %s", where, leak, s)
		}
	}
}

func TestRedactErrorDropsURLAndKeepsIs(t *testing.T) {
	raw := postErr(t, context.Background(), unreachableURL(t))
	if !strings.Contains(raw.Error(), "XXXXsecretXXXX") {
		t.Fatalf("precondition: net/http no longer repeats the URL: %v", raw)
	}
	assertNoSecret(t, "RedactError", RedactError(raw).Error())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := RedactError(postErr(t, ctx, unreachableURL(t)))
	if !errors.Is(canceled, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) lost through redaction: %v", canceled)
	}
	assertNoSecret(t, "canceled error", canceled.Error())

	if got := RedactError(errors.New("token tok-123 rejected"), "tok-123").Error(); strings.Contains(got, "tok-123") {
		t.Fatalf("explicit secret not removed: %s", got)
	}
	if RedactError(nil) != nil {
		t.Fatal("RedactError(nil) != nil")
	}
}

// leakyNotifier returns the raw transport error, URL and all — a notifier
// that does not redact on its own.
type leakyNotifier struct{ target string }

func (l leakyNotifier) Notify(ctx context.Context, _ NotifyEvent) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, l.target, nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func (l leakyNotifier) RequestApproval(ctx context.Context, _ *types.Incident, _ string) error {
	return l.Notify(ctx, NotifyEvent{})
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAsyncRetryAndDeadLetterLogsAreRedacted(t *testing.T) {
	var logs syncBuffer
	a := NewAsync(leakyNotifier{target: unreachableURL(t)}, slog.New(slog.NewJSONHandler(&logs, nil)))
	a.sleep = func(context.Context, time.Duration) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)

	_ = a.Notify(context.Background(), testEvent())
	_ = a.RequestApproval(context.Background(), &types.Incident{ID: "inc-2"}, "reboot")
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(logs.String(), "dead-lettered") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	out := logs.String()
	if strings.Count(out, "dead-lettered") != 2 || !strings.Contains(out, "retrying") {
		t.Fatalf("want retry lines and two dead letters, got:\n%s", out)
	}
	assertNoSecret(t, "async log", out)
}
