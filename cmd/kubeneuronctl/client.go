package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// client is a thin authenticated JSON client for the controller API.
type client struct {
	base  string
	token string
	http  *http.Client
}

// newClient resolves the server and token from flags/environment. Token
// sources, in order: --token-file, KUBENEURONCTL_TOKEN, --token (argv is
// visible to other local processes; prefer the first two).
func newClient(cmd *cobra.Command) (*client, error) {
	base, _ := cmd.Flags().GetString("server")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("--server must be an absolute URL")
	}
	token, _ := cmd.Flags().GetString("token")
	if path, _ := cmd.Flags().GetString("token-file"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("token file: %w", err)
		}
		token = string(data)
	} else if env := os.Getenv("KUBENEURONCTL_TOKEN"); token == "" && env != "" {
		token = env
	}
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: strings.TrimSpace(token),
		http:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// do performs a request; when out is non-nil the JSON response is decoded
// into it.
func (c *client) do(method, path string, in, out any) error {
	return c.doHeaders(method, path, in, out, nil)
}

// doHeaders is the operation-aware variant of do. v0.4.0 mutations carry an
// Idempotency-Key so a terminal retry never creates a second diagnostic,
// simulation, approval or autonomous rollout transition.
func (c *client) doHeaders(method, path string, in, out any, headers map[string]string) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(detail))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusServiceUnavailable {
			msg += " (set --token-file or KUBENEURONCTL_TOKEN; the controller needs -api-token-file)"
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (c *client) doBytes(method, path string, payload []byte, contentType string, out any, headers map[string]string) error {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func actorOrLocalUser(cmd *cobra.Command) string {
	if actor, _ := cmd.Flags().GetString("actor"); strings.TrimSpace(actor) != "" {
		return actor
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
