// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	corev1 "k8s.io/api/core/v1"
)

type TrainingReceiver struct {
	Authenticate func(*http.Request) (*corev1.Pod, error)
	Monitored    func(context.Context, string) bool
	Owner        func(context.Context, *corev1.Pod) (types.TrainingOwner, bool)
	Record       func(context.Context, *corev1.Pod, types.TrainingOwner, TrainingReport, time.Time) error

	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func (h *TrainingReceiver) allow(uid string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil || now.Sub(h.window) >= time.Minute {
		h.window = now
		h.counts = make(map[string]int)
	}
	if h.counts[uid] >= 60 || (h.counts[uid] == 0 && len(h.counts) >= 16384) {
		return false
	}
	h.counts[uid]++
	return true
}

func (h *TrainingReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	pod, err := h.Authenticate(r)
	if err != nil || pod == nil || pod.UID == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !h.allow(string(pod.UID), time.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	report, status := ReadTrainingReport(w, r)
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	ctx := r.Context()
	if !h.Monitored(ctx, pod.Spec.NodeName) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	owner, ok := h.Owner(ctx, pod)
	if !ok || owner.UID == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if err := h.Record(ctx, pod, owner, report, time.Now().UTC()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
