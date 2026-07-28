package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func capture(t *testing.T, status int) (*Notifier, *[]string) {
	t.Helper()
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("bad payload: %v", err)
		}
		texts = append(texts, payload.Text)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return New(server.URL), &texts
}

func incident() *types.Incident {
	return &types.Incident{
		ID: "ecc-dbe-n1-abcd", Class: types.ClassECCDBE, State: types.StateExecuting,
		Target: types.Target{Node: "n1", GPUUUID: "GPU-1"}, DryRun: true,
	}
}

func TestNotifyPostsFormattedMessage(t *testing.T) {
	n, texts := capture(t, http.StatusOK)
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: incident(), Message: "incident opened",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := (*texts)[0]
	for _, want := range []string{"ecc-dbe-n1-abcd", "n1", "ecc-dbe", "dry-run"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message %q missing %q", got, want)
		}
	}
}

func TestRequestApprovalIncludesDecisionCommands(t *testing.T) {
	n, texts := capture(t, http.StatusOK)
	if err := n.RequestApproval(context.Background(), incident(), "reboot"); err != nil {
		t.Fatal(err)
	}
	got := (*texts)[0]
	if !strings.Contains(got, "kubeneuronctl approve ecc-dbe-n1-abcd") ||
		!strings.Contains(got, "kubeneuronctl reject ecc-dbe-n1-abcd") {
		t.Fatalf("approval message %q missing decision commands", got)
	}
}

func TestWebhookErrorSurfaces(t *testing.T) {
	n, _ := capture(t, http.StatusForbidden)
	err := n.Notify(context.Background(), notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: incident(),
	})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want webhook status error", err)
	}
}
