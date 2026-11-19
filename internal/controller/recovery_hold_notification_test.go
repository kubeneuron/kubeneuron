// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/recoverymode"
)

type interruptedHoldNotifier struct {
	*recordingNotifier
	afterDispatch bool
}

func (n interruptedHoldNotifier) Notify(ctx context.Context, event notify.NotifyEvent) error {
	if n.afterDispatch {
		_ = n.recordingNotifier.Notify(ctx, event)
	}
	panic(n)
}

func TestPendingHoldNotificationAfterControllerRestart(t *testing.T) {
	for _, tc := range []struct {
		name          string
		afterDispatch bool
		calls         int
	}{
		{"before dispatch", false, 1}, {"after dispatch before acknowledgement", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st, n := successorFixture(t, recoverymode.Automatic, newSuccessorPlatform(), "")
			inc := mustIncident(t, st, openOn(t, c, "n1"))
			interrupted := interruptedHoldNotifier{n, tc.afterDispatch}
			c.notifier = interrupted
			const reason = "node group spans several zones"
			func() {
				defer func() {
					if got := recover(); got != interrupted {
						t.Fatalf("interruption = %v", got)
					}
				}()
				_ = c.notifyAddFirstHold(t.Context(), inc, "cordon", reason)
			}()
			entries := trail(t, st, inc.ID)
			if !auditHas(entries, "recovery-unavailable", reason) || !auditHas(entries, "notify-pending", reason) || auditHas(entries, "notified", reason) {
				t.Fatal("interruption did not leave an unacknowledged intent")
			}
			restarted := &Controller{store: st, notifier: n}
			for range 3 {
				if err := restarted.notifyAddFirstHold(t.Context(), inc, "cordon", reason); err != nil {
					t.Fatal(err)
				}
			}
			count := 0
			for _, ev := range n.events {
				if ev.Kind == notify.EventNeedsHuman {
					count++
				}
			}
			if count != tc.calls || !auditHas(trail(t, st, inc.ID), "notified", reason) {
				t.Fatalf("got %d dispatches, want %d and a persisted acknowledgement", count, tc.calls)
			}
		})
	}
}
