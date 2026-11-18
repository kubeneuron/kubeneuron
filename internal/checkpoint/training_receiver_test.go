// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

func TestTrainingReceiverLimitsEachPod(t *testing.T) {
	h := new(TrainingReceiver)
	now := time.Now()
	for range 60 {
		if !h.allow("first", now) {
			t.Fatal("initial budget exhausted early")
		}
	}
	if h.allow("first", now) || !h.allow("second", now) || !h.allow("first", now.Add(time.Minute)) {
		t.Fatal("incorrect pod budget or window rollover")
	}
}

func TestTrainingReceiverAuthorizesBeforeRecording(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "worker", Namespace: "training"}, Spec: corev1.PodSpec{NodeName: "node"}}
	owner := types.TrainingOwner{UID: "verified-job"}
	const progress = `{"kind":"progress","step":0}`
	for _, tc := range []struct {
		name, body                      string
		authenticated, monitored, owned bool
		status                          int
	}{
		{"invalid token", progress, false, true, true, 401},
		{"unregistered node", progress, true, false, true, 403},
		{"unverified owner", progress, true, true, false, 403},
		{"client supplied job", `{"kind":"progress","step":0,"job":"other"}`, true, true, true, 400},
		{"verified worker", progress, true, true, true, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			writes := 0
			h := TrainingReceiver{
				Authenticate: func(*http.Request) (*corev1.Pod, error) {
					if !tc.authenticated {
						return nil, errors.New("private token details")
					}
					return pod, nil
				},
				Monitored: func(_ context.Context, node string) bool { return node == pod.Spec.NodeName && tc.monitored },
				Owner: func(_ context.Context, sender *corev1.Pod) (types.TrainingOwner, bool) {
					return owner, sender == pod && tc.owned
				},
				Record: func(_ context.Context, sender *corev1.Pod, target types.TrainingOwner, report TrainingReport, at time.Time) error {
					writes++
					if sender != pod || target != owner || report.Step == nil || *report.Step != 0 || at.Before(before) || at.After(time.Now()) {
						t.Fatal("unexpected report identity, step or receive time")
					}
					return nil
				},
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if response.Code != tc.status || response.Body.Len() != 0 {
				t.Fatalf("status %d, body %q", response.Code, response.Body.String())
			}
			wantWrites := 0
			if tc.status == http.StatusNoContent {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("writes %d, want %d", writes, wantWrites)
			}
		})
	}
}
