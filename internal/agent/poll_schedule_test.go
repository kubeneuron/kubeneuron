// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func TestJitteredDelay(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		base, spread, ceiling, lower, upper time.Duration
	}{
		{"first idle poll", 5, 1, 15, 4, 6},
		{"second idle poll", 10, 2, 15, 8, 12},
		{"capped idle poll", 15, 3, 15, 12, 15},
		{"heartbeat", 30, 3, 33, 27, 33},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lower, upper := tc.lower*time.Second, tc.upper*time.Second
			for _, draw := range []float64{0, 0.5, 1} {
				want := lower + time.Duration(float64(upper-lower)*draw)
				got := jitteredDelay(tc.base*time.Second, tc.spread*time.Second, tc.ceiling*time.Second, draw)
				if got != want {
					t.Errorf("draw %g: got %v, want %v", draw, got, want)
				}
			}
		})
	}
}

type pollTransport func(*http.Request) (*http.Response, error)

func (fn pollTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestActionLoopPollSequence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newTestAgent(t, "http://controller.test", &testClock{now: time.Now()}, slog.Default())
		a.cfg.NodeLevelActions = true
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var polls []time.Time
		results := 0
		a.client.Transport = pollTransport(func(req *http.Request) (*http.Response, error) {
			response := &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}
			if req.Method == http.MethodPost {
				results++
				return response, nil
			}
			polls = append(polls, time.Now())
			switch len(polls) {
			case 2:
				response.StatusCode = http.StatusServiceUnavailable
			case 3:
				return nil, context.DeadlineExceeded
			case 4:
				response.StatusCode = http.StatusOK
				response.Header.Set(types.AgentActionLeaseHeader, "lease")
				response.Header.Set(types.AgentActionLeaseExpiresHeader, time.Now().Add(time.Minute).Format(time.RFC3339Nano))
				response.Body = io.NopCloser(strings.NewReader(`{"id":"schedule-test","type":"idle_check","params":{"gpu_index":"0"}}`))
			case 6:
				cancel()
			}
			return response, nil
		})
		start := time.Now()
		a.actionLoop(ctx)
		if results != 1 || len(polls) != 6 {
			t.Fatalf("results=%d polls=%d", results, len(polls))
		}
		for i, bounds := range [][2]time.Duration{{0, 0}, {4, 6}, {8, 12}, {12, 15}, {0, 0}, {4, 6}} {
			gap := polls[i].Sub(start)
			if gap < bounds[0]*time.Second || gap > bounds[1]*time.Second {
				t.Errorf("poll %d gap %v, expected %v seconds", i, gap, bounds)
			}
			start = polls[i]
		}
	})
}
