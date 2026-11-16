// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// The endpoint URL (and any token) is a credential: no delivery error may
// repeat its path, query or userinfo.
func TestDeliveryErrorsNeverCarryTheEndpointSecret(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer failing.Close()

	for name, base := range map[string]string{"unreachable": closed.URL, "non-2xx": failing.URL} {
		target := strings.Replace(base, "://", "://user:pw@", 1) + "/services/T0/B0/pathsecret?token=querysecret"
		n := &Notifier{WebhookURL: target}
		err := n.Notify(context.Background(), notify.NotifyEvent{Kind: notify.EventOpened, Incident: &types.Incident{ID: "inc-1"}})
		if err == nil {
			t.Fatalf("%s: want a delivery error", name)
		}
		for _, leak := range []string{"pathsecret", "querysecret", "user:pw", "/services/", "bearer-secret", "routing-secret"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: error leaks %q: %v", name, leak, err)
			}
		}
	}
}
