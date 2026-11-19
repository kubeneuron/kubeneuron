// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/journal"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type tailJournal struct{}

func (tailJournal) Next(context.Context, journal.Cursor) (journal.Batch, error) {
	return journal.Batch{Cursor: "tail"}, nil
}

func journalAgent(t *testing.T, dir string, open journal.OpenReader) *Agent {
	t.Helper()
	state := t.TempDir()
	a, err := New(Config{
		NodeName: "gpu-node-1", ControllerURL: "http://127.0.0.1:1", AllowInsecureHTTP: true,
		SpoolPath: state + "/spool.jsonl", ActionJournalPath: state + "/actions.jsonl",
		HealthListenAddress: "127.0.0.1:0", RegistrationInterval: 10 * time.Second, RegistrationStaleAfter: 30 * time.Second,
		JournalDir: dir, JournalOpen: open,
	}, &nvml.Fake{GPUs: []types.GPUInfo{{Index: 0, UUID: "GPU-test", Model: "test"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.journalSource.CursorPath; got != filepath.Join(state, "journal-cursor.json") {
		t.Fatalf("journal cursor path = %q, want beside the spool", got)
	}
	return a
}

func kernelSourceReason(t *testing.T) (bool, string) {
	t.Helper()
	for _, r := range metrics.AgentSignalSources("gpu-node-1") {
		if r.Source == "nvidia-kmsg" && r.Class == types.ClassXIDApp {
			return r.Available, r.Reason
		}
	}
	t.Fatal("no nvidia-kmsg row")
	return false, ""
}

func TestAgentReportsAnUnreadableJournal(t *testing.T) {
	a := journalAgent(t, filepath.Join(t.TempDir(), "absent"), func(string) (journal.Reader, error) { return tailJournal{}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ch := a.openJournal(ctx); ch != nil {
		t.Fatal("an absent journal directory opened")
	}
	a.reportKernelLog()
	if available, reason := kernelSourceReason(t); available || reason != "journal-unreadable" {
		t.Fatalf("kernel log = %t %q, want unavailable journal-unreadable", available, reason)
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	a = journalAgent(t, dir, func(string) (journal.Reader, error) { return tailJournal{}, nil })
	ch := a.openJournal(ctx)
	if ch == nil {
		t.Fatal("a readable journal did not open")
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()
	a.reportKernelLog()
	if available, _ := kernelSourceReason(t); !available {
		t.Fatal("a readable journal is reported unavailable")
	}
}
