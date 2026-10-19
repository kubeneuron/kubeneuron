// Command alertsink is a deterministic webhook recorder for the kind GPU
// observability fixture (hack/kind-gpu-observability.sh).
//
// The fixture runs two copies: one stands in for whatever a human-facing
// receiver would deliver to, the other stands in for the KubeNeuron
// controller's Alertmanager webhook. Neither one acts on anything: every
// request is stored in memory and answered 200, and GET /requests returns
// the stored requests as JSON so the harness can assert which receiver
// Alertmanager actually delivered each alert to. It opens no incident,
// runs no remediation and talks to nothing else.
//
// With -post <url> it is instead a one-shot in-cluster HTTP client: it reads
// a JSON body from stdin, POSTs it with Content-Type application/json and
// prints the response body. The harness runs it with `kubectl exec -i` for
// the calls that must be POSTs (Alertmanager probe alerts, Grafana panel
// queries), because the API server's service proxy, which carries every
// other call, does not preserve the request Content-Type.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Record is one received request.
type Record struct {
	ReceivedAt    time.Time `json:"received_at"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	ContentType   string    `json:"content_type"`
	Authorization string    `json:"authorization"`
	UserAgent     string    `json:"user_agent"`
	// Body is the request body when it is valid JSON, else null; BodyText
	// then carries the raw text so nothing is lost.
	Body     json.RawMessage `json:"body"`
	BodyText string          `json:"body_text,omitempty"`
}

// Sink records requests. Any path other than /requests and /healthz is a
// recording endpoint, so a receiver URL with any path lands here.
type Sink struct {
	mu      sync.Mutex
	records []Record
	maxBody int64
}

// NewSink returns a sink that keeps bodies up to maxBody bytes.
func NewSink(maxBody int64) *Sink { return &Sink{maxBody: maxBody} }

// Records returns a copy of everything recorded so far, in arrival order.
func (s *Sink) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

func (s *Sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		_, _ = io.WriteString(w, "ok\n")
	case r.URL.Path == "/requests" && r.Method == http.MethodGet:
		records := s.Records()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(records), "requests": records})
	default:
		body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBody))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		rec := Record{
			ReceivedAt:    time.Now().UTC(),
			Method:        r.Method,
			Path:          r.URL.Path,
			ContentType:   r.Header.Get("Content-Type"),
			Authorization: r.Header.Get("Authorization"),
			UserAgent:     r.Header.Get("User-Agent"),
		}
		if json.Valid(body) {
			rec.Body = json.RawMessage(body)
		} else {
			rec.Body = json.RawMessage("null")
			rec.BodyText = string(body)
		}
		s.mu.Lock()
		s.records = append(s.records, rec)
		n := len(s.records)
		s.mu.Unlock()
		log.Printf("alertsink: recorded %s %s (%d bytes, %d total)", r.Method, r.URL.Path, len(body), n)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"recorded"}`+"\n")
	}
}

// PostJSON sends body to url as application/json and returns the status
// code and response body.
func PostJSON(client *http.Client, url string, body []byte) (int, []byte, error) {
	if !json.Valid(body) {
		return 0, nil, fmt.Errorf("body is not valid JSON")
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, out, err
}

func main() {
	listen := flag.String("listen", ":8080", "address to serve on")
	maxBody := flag.Int64("max-body-bytes", 4<<20, "largest request body kept")
	post := flag.String("post", "", "one-shot client: POST the JSON on stdin to this URL and print the response body; exits 1 on a non-2xx status")
	flag.Parse()
	if *post != "" {
		body, err := io.ReadAll(io.LimitReader(os.Stdin, *maxBody))
		if err != nil {
			log.Fatalf("alertsink: read stdin: %v", err)
		}
		status, out, err := PostJSON(&http.Client{Timeout: 30 * time.Second}, *post, body)
		if err != nil {
			log.Fatalf("alertsink: POST %s: %v", *post, err)
		}
		_, _ = os.Stdout.Write(out)
		if status < 200 || status >= 300 {
			fmt.Fprintf(os.Stderr, "alertsink: POST %s: HTTP %d\n", *post, status)
			os.Exit(1)
		}
		return
	}
	server := &http.Server{Addr: *listen, Handler: NewSink(*maxBody), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("alertsink: recording every request on %s (no action is ever taken)", *listen)
	log.Fatal(server.ListenAndServe())
}
