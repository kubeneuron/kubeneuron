// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/agent/nvml"
	"github.com/kubeneuron/kubeneuron/internal/journal"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/pkg/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	r := kernelSourceRow(t, "nvidia-kmsg", types.ClassXIDApp)
	return r.Available, r.Reason
}

func kernelSourceRow(t *testing.T, source string, class types.ProblemClass) types.AgentSignalSource {
	t.Helper()
	for _, r := range metrics.AgentSignalSources("gpu-node-1") {
		if r.Source == source && r.Class == class {
			return r
		}
	}
	t.Fatalf("no %s %s row", source, class)
	return types.AgentSignalSource{}
}

func TestAgentReportsAnUnreadableJournal(t *testing.T) {
	a := journalAgent(t, filepath.Join(t.TempDir(), "absent"), func(string) (journal.Reader, error) { return tailJournal{}, nil })
	var logs strings.Builder
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ch := a.openJournal(ctx); ch != nil {
		t.Fatal("an absent journal directory opened")
	}
	if strings.Contains(logs.String(), "source open") || !strings.Contains(logs.String(), "will retry") {
		t.Fatalf("absent journal logs %q, want only the retry warning", logs.String())
	}
	a.reportKernelLog()
	if available, reason := kernelSourceReason(t); available || reason != "journal-unreadable" {
		t.Fatalf("kernel log = %t %q, want unavailable journal-unreadable", available, reason)
	}
	if r := kernelSourceRow(t, "nvidia-kmsg", types.ClassXIDApp); r.Via != "" {
		t.Fatalf("an unavailable source says via %q", r.Via)
	}
	// /dev/kmsg readable beside an unreadable journal: the source reads kmsg.
	a.kmsgReadable = true
	a.reportKernelLog()
	for _, r := range []types.AgentSignalSource{kernelSourceRow(t, "nvidia-kmsg", types.ClassXIDApp), kernelSourceRow(t, "amd-kmsg", types.ClassPCIe)} {
		if !r.Available || r.Via != "kmsg" || r.Reason != "" {
			t.Fatalf("kmsg beside an unreadable journal %+v, want available via kmsg", r)
		}
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	a = journalAgent(t, dir, func(string) (journal.Reader, error) { return tailJournal{}, nil })
	// The agent binary logs JSON, and hack/kind-journal.sh waits for this
	// line's msg and dir in that form.
	logs.Reset()
	a.log = slog.New(slog.NewJSONHandler(&logs, nil))
	ch := a.openJournal(ctx)
	if ch == nil {
		t.Fatal("a readable journal did not open")
	}
	var line struct{ Msg, Dir string }
	if err := json.Unmarshal([]byte(logs.String()), &line); err != nil || line.Msg != "journal kernel log source open" || line.Dir != dir {
		t.Fatalf("readable journal logs %q (%v), want the open line with dir %q", logs.String(), err, dir)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()
	a.reportKernelLog()
	for _, r := range []types.AgentSignalSource{kernelSourceRow(t, "nvidia-kmsg", types.ClassXIDApp), kernelSourceRow(t, "amd-kmsg", types.ClassDriverHang)} {
		if !r.Available || r.Via != "journal" {
			t.Fatalf("a readable journal %+v, want available via journal", r)
		}
	}
}

// A skipped journal entry is a lost kernel fault: the source reads
// journal-unreadable from the skip until journalSkipHold after the last one,
// a new skip extends that, and the counter counts the entries.
func TestAgentHoldsJournalUnreadableAfterASkippedEntry(t *testing.T) {
	a := journalAgent(t, t.TempDir(), func(string) (journal.Reader, error) { return tailJournal{}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	ch := a.openJournal(ctx)
	if ch == nil {
		cancel()
		t.Fatal("a readable journal did not open")
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	var logs strings.Builder
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	before := testutil.ToFloat64(metrics.AgentJournalUnreadableEntries)
	want := func(available bool) {
		t.Helper()
		a.reportKernelLog()
		got, reason := kernelSourceReason(t)
		if got != available || (!available && reason != "journal-unreadable") {
			t.Fatalf("at %s kernel log = %t %q, want available=%t", now.Format(time.TimeOnly), got, reason, available)
		}
	}

	want(true)
	a.journalSource.Unreadable(1)
	select {
	case <-a.journalSkipped:
	default:
		t.Fatal("a skip did not wake the Run loop")
	}
	want(false)
	now = now.Add(9 * time.Minute)
	want(false)
	a.journalSource.Unreadable(2)
	now = now.Add(journalSkipHold - time.Second)
	want(false)
	now = now.Add(time.Second)
	if !a.journalSkipShown || a.journalSkipHeld() {
		t.Fatal("the end of the hold is not noticed on the next tick")
	}
	want(true)
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for two skips in one hold, want one on entering it:\n%s", n, logs.String())
	}
	if got := testutil.ToFloat64(metrics.AgentJournalUnreadableEntries) - before; got != 3 {
		t.Fatalf("unreadable entries counted %v, want 3", got)
	}
}
