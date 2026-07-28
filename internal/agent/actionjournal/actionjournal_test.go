package actionjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func testAction(id string) types.Action {
	return types.Action{
		ID:      id,
		Type:    types.ActionGPUReset,
		Params:  map[string]string{"gpu_index": "0", "boot_id": "boot-a"},
		Timeout: time.Minute,
	}
}

func testResult(id string) types.ActionResult {
	started := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	return types.ActionResult{
		ActionID:   id,
		OK:         true,
		Output:     "reset completed",
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
	}
}

func openTestJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "actions.jsonl")
	journal, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return journal, path
}

func TestJournalWriteAndReopen(t *testing.T) {
	journal, path := openTestJournal(t)
	action := testAction("act-1")
	if _, err := journal.RecordReceived(action); err != nil {
		t.Fatalf("RecordReceived() error = %v", err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	wantResult := testResult(action.ID)
	if _, err := journal.RecordOutcome(action.ID, wantResult); err != nil {
		t.Fatalf("RecordOutcome() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	got, ok := reopened.Get(action.ID)
	if !ok {
		t.Fatal("Get() did not retain action after reopen")
	}
	if got.State != StateOutcomeKnown {
		t.Fatalf("state after reopen = %q, want %q", got.State, StateOutcomeKnown)
	}
	if got.Result == nil || *got.Result != wantResult {
		t.Fatalf("result after reopen = %#v, want %#v", got.Result, wantResult)
	}
	if got.Action.ID != action.ID || got.Action.Params["boot_id"] != "boot-a" {
		t.Fatalf("action after reopen = %#v, want retained intent", got.Action)
	}
}

func TestJournalTransitionsAreIdempotentAndRejectConflicts(t *testing.T) {
	journal, _ := openTestJournal(t)
	action := testAction("act-idempotent")

	first, err := journal.RecordReceived(action)
	if err != nil {
		t.Fatalf("first RecordReceived() error = %v", err)
	}
	second, err := journal.RecordReceived(action)
	if err != nil {
		t.Fatalf("second RecordReceived() error = %v", err)
	}
	if second.State != StateReceived || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("idempotent received = %#v, want original %#v", second, first)
	}

	conflict := testAction(action.ID)
	conflict.Params["gpu_index"] = "1"
	if _, err := journal.RecordReceived(conflict); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("RecordReceived(conflict) error = %v, want ErrActionConflict", err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatalf("first MarkRunning() error = %v", err)
	}
	if state, err := journal.MarkRunning(action.ID); err != nil || state.State != StateRunning {
		t.Fatalf("second MarkRunning() = (%#v, %v), want idempotent running", state, err)
	}

	result := testResult(action.ID)
	if _, err := journal.RecordOutcome(action.ID, result); err != nil {
		t.Fatalf("first RecordOutcome() error = %v", err)
	}
	if state, err := journal.RecordOutcome(action.ID, result); err != nil || state.State != StateOutcomeKnown {
		t.Fatalf("second RecordOutcome() = (%#v, %v), want idempotent known outcome", state, err)
	}
	differentResult := result
	differentResult.Output = "different"
	if _, err := journal.RecordOutcome(action.ID, differentResult); !errors.Is(err, ErrOutcomeConflict) {
		t.Fatalf("RecordOutcome(conflict) error = %v, want ErrOutcomeConflict", err)
	}
	if _, err := journal.MarkReported(action.ID); err != nil {
		t.Fatalf("first MarkReported() error = %v", err)
	}
	if state, err := journal.MarkReported(action.ID); err != nil || state.State != StateReported {
		t.Fatalf("second MarkReported() = (%#v, %v), want idempotent reported", state, err)
	}
	if _, err := journal.MarkRunning(action.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkRunning(reported) error = %v, want ErrInvalidTransition", err)
	}
}

func TestOpenMarksInterruptedRunningActionOutcomeUnknown(t *testing.T) {
	journal, path := openTestJournal(t)
	action := testAction("act-interrupted")
	if _, err := journal.RecordReceived(action); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	entry, ok := reopened.Get(action.ID)
	if !ok || entry.State != StateOutcomeUnknown {
		t.Fatalf("recovered entry = %#v, found %t; want outcome-unknown", entry, ok)
	}
	if _, err := reopened.MarkRunning(action.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkRunning(recovered unknown) error = %v, want ErrInvalidTransition", err)
	}

	openedAgain, err := Open(path)
	if err != nil {
		t.Fatalf("second Open(reopen) error = %v", err)
	}
	entry, ok = openedAgain.Get(action.ID)
	if !ok || entry.State != StateOutcomeUnknown {
		t.Fatalf("second recovered entry = %#v, found %t; want durable outcome-unknown", entry, ok)
	}
}

func TestClaimSurvivesRecoveryAndIsClearedAfterReport(t *testing.T) {
	journal, path := openTestJournal(t)
	action := testAction("act-claimed-recovery")
	if _, err := journal.RecordReceived(action); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(time.Hour).UTC()
	if _, err := journal.SetClaim(action.ID, "lease-original", expiresAt); err != nil {
		t.Fatalf("SetClaim() error = %v", err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	recoverable := reopened.ListRecoverable()
	if len(recoverable) != 1 {
		t.Fatalf("ListRecoverable() = %#v, want one entry", recoverable)
	}
	entry := recoverable[0]
	if entry.State != StateOutcomeUnknown || entry.LeaseToken != "lease-original" || !entry.LeaseExpiresAt.Equal(expiresAt) {
		t.Fatalf("recovered entry = %#v, want unknown state and original claim", entry)
	}
	if _, err := reopened.MarkReported(action.ID); err != nil {
		t.Fatalf("MarkReported() error = %v", err)
	}
	entry, ok := reopened.Get(action.ID)
	if !ok || entry.LeaseToken != "" || !entry.LeaseExpiresAt.IsZero() {
		t.Fatalf("reported entry = %#v, found %t; want cleared claim", entry, ok)
	}
	if entries := reopened.ListRecoverable(); len(entries) != 0 {
		t.Fatalf("ListRecoverable() after report = %#v, want none", entries)
	}
}

func TestSetClaimReplacesExpiredClaimWithoutChangingOutcome(t *testing.T) {
	journal, _ := openTestJournal(t)
	action := testAction("act-reclaimed")
	if _, err := journal.RecordReceived(action); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.SetClaim(action.ID, "expired-lease", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("SetClaim(expired) error = %v", err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatal(err)
	}
	result := testResult(action.ID)
	if _, err := journal.RecordOutcome(action.ID, result); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UTC()
	entry, err := journal.SetClaim(action.ID, "reclaimed-lease", future)
	if err != nil {
		t.Fatalf("SetClaim(reclaimed) error = %v", err)
	}
	if entry.State != StateOutcomeKnown || entry.Result == nil || *entry.Result != result || entry.LeaseToken != "reclaimed-lease" || !entry.LeaseExpiresAt.Equal(future) {
		t.Fatalf("reclaimed entry = %#v, want known outcome with replacement claim", entry)
	}
}

func TestOpenRecoversCorruptFinalTail(t *testing.T) {
	journal, path := openTestJournal(t)
	action := testAction("act-tail")
	if _, err := journal.RecordReceived(action); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.MarkRunning(action.ID); err != nil {
		t.Fatal(err)
	}
	result := testResult(action.ID)
	if _, err := journal.RecordOutcome(action.ID, result); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"version":1,"entry":"truncated-tail"`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(corrupt tail) error = %v", err)
	}
	entry, ok := reopened.Get(action.ID)
	if !ok || entry.State != StateOutcomeKnown || entry.Result == nil || *entry.Result != result {
		t.Fatalf("recovered entry = %#v, found %t; want retained known outcome", entry, ok)
	}
	if _, err := reopened.MarkReported(action.ID); err != nil {
		t.Fatalf("MarkReported() after tail recovery error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "truncated-tail") {
		t.Fatalf("corrupt tail survived rewrite: %q", data)
	}

	// The rewritten file must itself be durable and parseable on the next open.
	if _, err := Open(path); err != nil {
		t.Fatalf("Open(after tail rewrite) error = %v", err)
	}
}

func TestJournalRejectsUnboundedInput(t *testing.T) {
	journal, _ := openTestJournal(t)
	if _, err := journal.RecordReceived(types.Action{ID: "", Type: types.ActionGPUReset}); err == nil {
		t.Fatal("RecordReceived(empty ID) error = nil, want validation error")
	}
	tooLarge := testAction("act-large")
	tooLarge.Params["payload"] = strings.Repeat("x", maxParamBytes+1)
	if _, err := journal.RecordReceived(tooLarge); err == nil {
		t.Fatal("RecordReceived(oversized param) error = nil, want validation error")
	}
}

// reportEntry drives an action through its full lifecycle to reported.
func reportEntry(t *testing.T, j *Journal, id string, known bool) {
	t.Helper()
	if _, err := j.RecordReceived(testAction(id)); err != nil {
		t.Fatalf("RecordReceived(%s): %v", id, err)
	}
	if _, err := j.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning(%s): %v", id, err)
	}
	if known {
		if _, err := j.RecordOutcome(id, testResult(id)); err != nil {
			t.Fatalf("RecordOutcome(%s): %v", id, err)
		}
	} else {
		if _, err := j.MarkOutcomeUnknown(id); err != nil {
			t.Fatalf("MarkOutcomeUnknown(%s): %v", id, err)
		}
	}
	if _, err := j.MarkReported(id); err != nil {
		t.Fatalf("MarkReported(%s): %v", id, err)
	}
}

func TestCompactReportedDropsOnlyOldAcknowledgedEntries(t *testing.T) {
	journal, path := openTestJournal(t)

	reportEntry(t, journal, "old-known", true)
	reportEntry(t, journal, "old-unknown", false)
	if _, err := journal.RecordReceived(testAction("live")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.MarkRunning("live"); err != nil {
		t.Fatal(err)
	}

	// Retention 0: every reported entry is older than the cutoff.
	removed, err := journal.CompactReported(0)
	if err != nil {
		t.Fatalf("CompactReported: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, ok := journal.Get("old-known"); ok {
		t.Fatal("old reported entry must be gone")
	}
	if entry, ok := journal.Get("live"); !ok || entry.State != StateRunning {
		t.Fatalf("live entry must survive compaction untouched: %+v ok=%v", entry, ok)
	}

	// A generous retention keeps fresh reported entries.
	reportEntry(t, journal, "fresh", true)
	if removed, err = journal.CompactReported(time.Hour); err != nil || removed != 0 {
		t.Fatalf("fresh reported entry must not be dropped: removed=%d err=%v", removed, err)
	}

	// The compacted file must reload with identical surviving state.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen compacted journal: %v", err)
	}
	// Open converts the interrupted running action to outcome-unknown.
	if entry, ok := reopened.Get("live"); !ok || entry.State != StateOutcomeUnknown {
		t.Fatalf("live entry after reopen = %+v ok=%v, want outcome-unknown", entry, ok)
	}
	if entry, ok := reopened.Get("fresh"); !ok || entry.State != StateReported || entry.Result == nil {
		t.Fatalf("fresh reported entry lost by reload: %+v ok=%v", entry, ok)
	}
}

// At the action limit the journal compacts acknowledged history instead of
// wedging permanently; a backlog of live actions still fails closed.
func TestJournalActionLimitCompactsInsteadOfWedging(t *testing.T) {
	oldMax, oldRetention := maxActions, reportedRetention
	maxActions, reportedRetention = 3, 0
	defer func() { maxActions, reportedRetention = oldMax, oldRetention }()

	journal, _ := openTestJournal(t)
	reportEntry(t, journal, "done-1", true)
	reportEntry(t, journal, "done-2", false)
	if _, err := journal.RecordReceived(testAction("live-1")); err != nil {
		t.Fatal(err)
	}

	// Limit reached (3 entries), but two are acknowledged: the next intent
	// must trigger compaction and be accepted.
	if _, err := journal.RecordReceived(testAction("live-2")); err != nil {
		t.Fatalf("journal must compact reported entries at the limit: %v", err)
	}
	if _, ok := journal.Get("done-1"); ok {
		t.Fatal("acknowledged entry must have been compacted away")
	}

	// All-live journal at the limit keeps failing closed.
	if _, err := journal.RecordReceived(testAction("live-3")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.RecordReceived(testAction("live-4")); err == nil {
		t.Fatal("live backlog at the limit must still be rejected")
	}
}
