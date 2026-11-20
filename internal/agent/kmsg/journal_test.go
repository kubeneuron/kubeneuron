// SPDX-License-Identifier: Apache-2.0

package kmsg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/journal"
)

// fakeJournal is a kernel log whose cursor is the index of the next entry.
// The zero cursor starts at the tail, as the reader contract says.
type fakeJournal struct {
	mu      sync.Mutex
	entries []string
	err     error
	reads   []journal.Cursor
	// unreadable marks entries the reader cannot decode: counted in the
	// batch, never returned.
	unreadable map[int]bool
}

func (f *fakeJournal) append(lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, lines...)
}

func (f *fakeJournal) Next(_ context.Context, after journal.Cursor) (journal.Batch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, after)
	if f.err != nil {
		return journal.Batch{}, f.err
	}
	if after == "" {
		return journal.Batch{Cursor: journal.Cursor(strconv.Itoa(len(f.entries)))}, nil
	}
	from, err := strconv.Atoi(string(after))
	if err != nil {
		return journal.Batch{}, journal.ErrInvalidJournal
	}
	var batch journal.Batch
	for i, line := range f.entries[from:] {
		if f.unreadable[from+i] {
			batch.UnreadableEntries++
			continue
		}
		batch.Records = append(batch.Records, journal.Record{Message: line})
	}
	batch.Cursor = journal.Cursor(strconv.Itoa(len(f.entries)))
	return batch, nil
}

const (
	xidLine   = "NVRM: Xid (PCI:0000:3b:00): 79, pid=1234, GPU has fallen off the bus."
	otherLine = "EXT4-fs (nvme0n1p1): mounted filesystem private-detail-42"
)

func journalSource(t *testing.T, f *fakeJournal, dir, cursorPath string, logs *bytes.Buffer) *JournalSource {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &JournalSource{
		Dir: dir, CursorPath: cursorPath, Idle: 5 * time.Millisecond, BootID: "boot-1",
		Open: func(directory string) (journal.Reader, error) {
			if directory != dir {
				t.Errorf("opened %q, want %q", directory, dir)
			}
			return f, nil
		},
	}
	if logs != nil {
		s.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return s
}

// watch starts the source and stops it, waiting for its goroutine, before
// the test's directories are removed.
func watch(t *testing.T, s *JournalSource) <-chan Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Watch(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		for range ch {
		}
	})
	return ch
}

func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("journal source stopped")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no journal event")
	}
	return Event{}
}

func noEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func savedCursor(t *testing.T, s *JournalSource) journal.Cursor {
	t.Helper()
	c, err := s.loadCursor()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestJournalSourceStartsAtTheTailAndSendsOnlyClassifiedFields(t *testing.T) {
	root := t.TempDir()
	f := &fakeJournal{entries: []string{xidLine, otherLine}}
	var logs bytes.Buffer
	s := journalSource(t, f, filepath.Join(root, "var"), filepath.Join(root, "state", "journal-cursor.json"), &logs)
	ch := watch(t, s)
	// The old XID is before the tail: it must not open an incident again.
	noEvent(t, ch)
	if got := savedCursor(t, s); got != "2" {
		t.Fatalf("cursor after the first empty read = %q, want the tail", got)
	}
	f.append(otherLine, xidLine)
	ev := nextEvent(t, ch)
	if ev.XID != 79 || ev.PCIAddr != "0000:3b:00" || ev.Raw != "" {
		t.Fatalf("event = %+v, want XID 79 on 0000:3b:00 with no raw text", ev)
	}
	if got := savedCursor(t, s); got != "2" {
		t.Fatalf("cursor moved to %q before the event was acknowledged", got)
	}
	if err := ev.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for savedCursor(t, s) != "4" {
		if time.Now().After(deadline) {
			t.Fatalf("cursor = %q after the acknowledgement, want 4", savedCursor(t, s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	noEvent(t, ch)
	if strings.Contains(logs.String(), "private-detail") || strings.Contains(logs.String(), "fallen off") {
		t.Fatalf("journal content reached the log: %s", logs.String())
	}
}

func TestJournalSourceReadsAnAbandonedBatchAgain(t *testing.T) {
	root := t.TempDir()
	f := &fakeJournal{}
	s := journalSource(t, f, filepath.Join(root, "var"), filepath.Join(root, "journal-cursor.json"), nil)
	ch := watch(t, s)
	noEvent(t, ch)
	f.append(xidLine)
	nextEvent(t, ch).Abandon()
	again := nextEvent(t, ch)
	if again.XID != 79 {
		t.Fatalf("redelivered %+v", again)
	}
	if got := savedCursor(t, s); got != "0" {
		t.Fatalf("an abandoned batch moved the cursor to %q", got)
	}
	_ = again.Acknowledge()
}

// An entry the reader skipped is reported once, when its batch is done, even
// if the batch is read again; the valid entry beside it is delivered and the
// cursor moves past both.
func TestJournalSourceReportsSkippedEntriesOnce(t *testing.T) {
	root := t.TempDir()
	f := &fakeJournal{unreadable: map[int]bool{0: true}}
	var logs bytes.Buffer
	s := journalSource(t, f, filepath.Join(root, "var"), filepath.Join(root, "journal-cursor.json"), &logs)
	var mu sync.Mutex
	var reported []int
	s.Unreadable = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, n)
	}
	ch := watch(t, s)
	noEvent(t, ch)
	f.append("private-detail-42 that does not decode", xidLine)
	nextEvent(t, ch).Abandon()
	ev := nextEvent(t, ch)
	if ev.XID != 79 {
		t.Fatalf("event = %+v, want the valid entry", ev)
	}
	_ = ev.Acknowledge()
	deadline := time.Now().Add(2 * time.Second)
	for savedCursor(t, s) != "2" {
		if time.Now().After(deadline) {
			t.Fatalf("cursor = %q, want past both entries", savedCursor(t, s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	noEvent(t, ch)
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || reported[0] != 1 {
		t.Fatalf("reported skips = %v, want one report of 1", reported)
	}
	if strings.Contains(logs.String(), "private-detail") {
		t.Fatalf("journal content reached the log: %s", logs.String())
	}
}

// A variant switch moves the node between the two directories: each keeps its
// own place in the shared state file, and returning to one neither replays
// what it delivered nor applies the other's cursor.
func TestJournalCursorSurvivesAVariantSwitch(t *testing.T) {
	root := t.TempDir()
	cursorPath := filepath.Join(root, "state", "journal-cursor.json")
	varLog := &fakeJournal{}
	runLog := &fakeJournal{entries: []string{xidLine, xidLine, xidLine}}

	run := func(f *fakeJournal, dir string, appended []string, want int) {
		t.Helper()
		s := journalSource(t, f, filepath.Join(root, dir), cursorPath, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := s.Watch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(appended) != 0 {
			// Reach the tail first, then write.
			noEvent(t, ch)
			f.append(appended...)
		}
		for i := 0; i < want; i++ {
			_ = nextEvent(t, ch).Acknowledge()
		}
		noEvent(t, ch)
		cancel()
		for range ch {
		}
	}
	run(varLog, "var", []string{xidLine}, 1)
	// The run journal's old entries are before its tail: the var cursor is
	// not applied to it.
	run(runLog, "run", nil, 0)
	// Back on var: only what was written while the agent was away.
	varLog.append(xidLine, otherLine)
	run(varLog, "var", nil, 1)
	run(varLog, "var", nil, 0)
	if varLog.reads[0] != "" {
		t.Fatalf("first var read used cursor %q", varLog.reads[0])
	}
	if runLog.reads[0] != "" {
		t.Fatalf("first run read used cursor %q, want the tail", runLog.reads[0])
	}
}

func TestJournalSourceUnavailable(t *testing.T) {
	if _, err := (&JournalSource{}).Watch(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no mount: %v", err)
	}
	if _, err := (&JournalSource{Dir: filepath.Join(t.TempDir(), "absent"), Open: func(string) (journal.Reader, error) {
		return &fakeJournal{}, nil
	}}).Watch(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent directory: %v", err)
	}
	root := t.TempDir()
	f := &fakeJournal{err: journal.ErrInvalidJournal}
	ch, err := journalSource(t, f, filepath.Join(root, "var"), "", nil).Watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an invalid journal produced an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an invalid journal did not stop the source")
	}
}

// A reboot is often the recovery itself: faults written before it must not
// open incidents again when the agent comes back on the new boot.
func TestJournalCursorFromAnotherBootStartsAtTheTail(t *testing.T) {
	root := t.TempDir()
	cursorPath := filepath.Join(root, "journal-cursor.json")
	f := &fakeJournal{}
	s := journalSource(t, f, filepath.Join(root, "var"), cursorPath, nil)
	if err := s.saveCursor("0"); err != nil {
		t.Fatal(err)
	}
	f.append(xidLine)
	s.BootID = "boot-2"
	ch := watch(t, s)
	noEvent(t, ch)
	f.mu.Lock()
	first := f.reads[0]
	f.mu.Unlock()
	if first != "" {
		t.Fatalf("the first read on a new boot used cursor %q, want the tail", first)
	}
	f.append(xidLine)
	if ev := nextEvent(t, ch); ev.XID != 79 {
		t.Fatalf("event = %+v", ev)
	}
}

// An event the agent cannot take holds its batch back, but the events of the
// batch already taken are not sent again, and the batch is read more slowly
// each time.
func TestJournalSourceRereadsOnlyWhatWasNotTaken(t *testing.T) {
	root := t.TempDir()
	f := &fakeJournal{}
	s := journalSource(t, f, filepath.Join(root, "var"), filepath.Join(root, "journal-cursor.json"), nil)
	s.RedeliverMax = 80 * time.Millisecond
	ch := watch(t, s)
	noEvent(t, ch)
	const dbeLine = "NVRM: Xid (PCI:0000:5e:00): 48, pid=1234, An uncorrectable double bit error"
	f.append(xidLine, dbeLine)
	if ev := nextEvent(t, ch); ev.XID != 79 {
		t.Fatalf("first event = %+v", ev)
	} else if err := ev.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	var at []time.Time
	for i := 0; i < 5; i++ {
		ev := nextEvent(t, ch)
		if ev.XID != 48 {
			t.Fatalf("redelivery %d sent XID %d again", i, ev.XID)
		}
		at = append(at, time.Now())
		ev.Abandon()
	}
	if first, last := at[1].Sub(at[0]), at[4].Sub(at[3]); last < 4*first || last < 30*time.Millisecond {
		t.Fatalf("redelivery gaps %v then %v, want a growing pause", first, last)
	}
	ev := nextEvent(t, ch)
	if ev.XID != 48 {
		t.Fatalf("event = %+v", ev)
	}
	_ = ev.Acknowledge()
	deadline := time.Now().Add(2 * time.Second)
	for savedCursor(t, s) != "2" {
		if time.Now().After(deadline) {
			t.Fatalf("cursor = %q, want 2 once the whole batch was taken", savedCursor(t, s))
		}
		time.Sleep(5 * time.Millisecond)
	}
	noEvent(t, ch)
}

// stalledJournal always asks for another tick without moving its cursor.
type stalledJournal struct {
	mu    sync.Mutex
	reads int
}

func (s *stalledJournal) Next(context.Context, journal.Cursor) (journal.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return journal.Batch{Cursor: "same", More: true}, nil
}

// TestJournalSourceWaitsOnABatchThatDoesNotMove: More without progress is
// read again after the idle wait, not at once.
func TestJournalSourceWaitsOnABatchThatDoesNotMove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &stalledJournal{}
	s := &JournalSource{Dir: dir, Idle: 20 * time.Millisecond, BootID: "boot-1",
		Open: func(string) (journal.Reader, error) { return r, nil }}
	watch(t, s)
	time.Sleep(200 * time.Millisecond)
	r.mu.Lock()
	reads := r.reads
	r.mu.Unlock()
	if reads < 2 || reads > 15 {
		t.Fatalf("%d reads in 200ms with a 20ms idle wait, want about 10", reads)
	}
}

// busyJournal ignores ctx and always has more, with a moving cursor.
type busyJournal struct{ n atomic.Int64 }

func (b *busyJournal) Next(context.Context, journal.Cursor) (journal.Batch, error) {
	return journal.Batch{Cursor: journal.Cursor(strconv.FormatInt(b.n.Add(1), 10)), More: true}, nil
}

// TestJournalSourceStopsWithAReaderThatIgnoresContext: cancelling ends the
// watch even when the reader never looks at ctx and never runs out.
func TestJournalSourceStopsWithAReaderThatIgnoresContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &JournalSource{Dir: dir, Idle: time.Millisecond, BootID: "boot-1",
		Open: func(string) (journal.Reader, error) { return &busyJournal{}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("event from a reader that returns no records")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop after cancel")
	}
}
