// Package actionjournal provides a small, crash-safe local write-ahead log
// for agent actions. It records the intent before an action may start and the
// outcome before that outcome is sent to the controller.
//
// The journal is deliberately conservative during recovery: an action that
// was durably marked running is changed to outcome-unknown when the agent
// reopens the journal. A caller must not execute that action again
// automatically, because the previous process may have completed a
// destructive host operation immediately before it crashed.
package actionjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const (
	journalVersion = 1

	// The journal is for the small set of in-flight and recently completed
	// remediation actions on one node. These limits make a corrupt or
	// hostile local file unable to consume unbounded memory on agent startup.
	maxRecordBytes     = 128 << 10
	maxActionIDBytes   = 256
	maxActionTypeBytes = 128
	maxParams          = 128
	maxParamBytes      = 4 << 10
	maxResultTextBytes = 64 << 10
	maxLeaseTokenBytes = 4 << 10
)

// Size limits and the reported-entry retention are variables only so tests
// can exercise the exhaustion and compaction paths without 10k fsyncs; the
// production values are fixed at init.
var (
	maxJournalBytes int64 = 64 << 20
	maxActions            = 10_000

	// reportedRetention bounds how long acknowledged (reported) entries are
	// kept for controller-replay dedup before compaction may drop them. It
	// must comfortably exceed any redelivery/lease window; a dropped reported
	// entry makes a very late duplicate delivery look like a fresh action.
	reportedRetention = 24 * time.Hour
)

var (
	// ErrNotFound means the journal has no intent record for an action ID.
	ErrNotFound = errors.New("action journal entry not found")
	// ErrActionConflict means an action ID was reused for a different intent.
	ErrActionConflict = errors.New("action journal action ID conflicts with existing intent")
	// ErrOutcomeConflict means a completed action was given a different result.
	ErrOutcomeConflict = errors.New("action journal outcome conflicts with existing outcome")
	// ErrInvalidTransition means the requested state cannot follow the entry's
	// durable state.
	ErrInvalidTransition = errors.New("action journal invalid state transition")
)

// State is an action's durable execution state.
type State string

const (
	// StateReceived records a controller action before the agent starts it.
	StateReceived State = "received"
	// StateRunning records that execution may have begun. It is the point at
	// which recovery must assume the outcome is unknown after a crash.
	StateRunning State = "running"
	// StateOutcomeKnown records a local executor result.
	StateOutcomeKnown State = "outcome-known"
	// StateOutcomeUnknown records that an interrupted action must not be
	// retried automatically.
	StateOutcomeUnknown State = "outcome-unknown"
	// StateReported records that either a known or unknown outcome was
	// acknowledged by the controller.
	StateReported State = "reported"
)

// Entry is the most recent durable record for one action. Result is present
// for a known outcome and remains present after that outcome is reported.
// A reported entry with a nil Result represents a reported unknown outcome.
type Entry struct {
	Action         types.Action        `json:"action"`
	State          State               `json:"state"`
	Result         *types.ActionResult `json:"result,omitempty"`
	LeaseToken     string              `json:"lease_token,omitempty"`
	LeaseExpiresAt time.Time           `json:"lease_expires_at,omitempty"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

// Journal stores a history of complete Entry snapshots. Keeping every record
// self-contained means a torn final append can be discarded without losing an
// earlier action intent or outcome.
type Journal struct {
	mu      sync.Mutex
	path    string
	entries map[string]Entry
	bytes   int64
}

type diskRecord struct {
	Version int   `json:"version"`
	Entry   Entry `json:"entry"`
}

// Open creates or opens a local journal. A malformed final record is treated
// as a torn write and is removed through an fsynced atomic replacement. A
// malformed record before the final record is a corruption error: silently
// skipping it could hide an action that did run.
func Open(path string) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("action journal path is required")
	}
	if err := ensureFile(path); err != nil {
		return nil, fmt.Errorf("create action journal: %w", err)
	}

	records, bytesOnDisk, tailCorrupt, err := readRecords(path)
	if err != nil {
		return nil, err
	}
	j := &Journal{
		path:    path,
		entries: make(map[string]Entry),
		bytes:   bytesOnDisk,
	}
	for index, record := range records {
		if err := j.applyRecordLocked(record); err != nil {
			// A final snapshot which is syntactically complete but semantically
			// invalid can still be a torn/corrupted append (for example, a
			// storage tear that happens to leave valid JSON). The preceding
			// snapshot is authoritative, so recover that tail just as we do a
			// malformed final line. Corruption before the tail remains fatal.
			if index == len(records)-1 {
				records = records[:index]
				tailCorrupt = true
				break
			}
			return nil, fmt.Errorf("recover action journal: %w", err)
		}
	}
	if tailCorrupt {
		if err := j.rewriteRecordsLocked(records); err != nil {
			return nil, fmt.Errorf("truncate corrupt action journal tail: %w", err)
		}
	}

	// A durable running marker is intentionally converted to unknown before
	// returning the journal. If this write cannot be synced, callers receive
	// no journal and therefore cannot accidentally retry the action.
	ids := make([]string, 0)
	for id, entry := range j.entries {
		if entry.State == StateRunning {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := j.entries[id]
		entry.State = StateOutcomeUnknown
		entry.Result = nil
		entry.UpdatedAt = time.Now().UTC()
		if err := j.appendLocked(entry); err != nil {
			return nil, fmt.Errorf("mark interrupted action %q outcome unknown: %w", id, err)
		}
	}
	return j, nil
}

// RecordReceived durably records an action intent. Repeating an identical
// intent is idempotent; reusing its ID for another action is rejected.
func (j *Journal) RecordReceived(action types.Action) (Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := validateAction(action); err != nil {
		return Entry{}, err
	}
	if existing, ok := j.entries[action.ID]; ok {
		if !sameAction(existing.Action, action) {
			return Entry{}, fmt.Errorf("%w: %q", ErrActionConflict, action.ID)
		}
		return cloneEntry(existing), nil
	}
	if len(j.entries) >= maxActions {
		// Old acknowledged entries are the only safely removable records.
		// A backlog of live (unreported) actions must keep failing closed.
		if _, err := j.compactReportedLocked(reportedRetention); err != nil {
			return Entry{}, fmt.Errorf("compact action journal at action limit: %w", err)
		}
	}
	if len(j.entries) >= maxActions {
		return Entry{}, fmt.Errorf("action journal has reached its %d action limit", maxActions)
	}
	entry := Entry{Action: cloneAction(action), State: StateReceived, UpdatedAt: time.Now().UTC()}
	if err := j.appendLocked(entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry), nil
}

// SetClaim durably associates an action with the exact controller delivery
// lease that authorized it. It is intentionally separate from RecordReceived:
// callers must fsync the controller intent before they retain its bearer-like
// lease token, and must fsync this claim before they may start execution.
//
// A newly reclaimed controller action replaces an expired local claim. A
// reported entry cannot be claimed again because its controller acknowledgement
// has already been durably recorded.
func (j *Journal) SetClaim(actionID, token string, expiresAt time.Time) (Entry, error) {
	if err := validateActionID(actionID); err != nil {
		return Entry{}, err
	}
	if err := validateClaim(token, expiresAt); err != nil {
		return Entry{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	current, ok := j.entries[actionID]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, actionID)
	}
	if current.State == StateReported {
		return Entry{}, fmt.Errorf("%w: cannot claim reported action %q", ErrInvalidTransition, actionID)
	}
	if current.LeaseToken == token && current.LeaseExpiresAt.Equal(expiresAt) {
		return cloneEntry(current), nil
	}
	entry := cloneEntry(current)
	entry.LeaseToken = token
	entry.LeaseExpiresAt = expiresAt.UTC()
	entry.UpdatedAt = time.Now().UTC()
	if err := j.appendLocked(entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry), nil
}

// MarkRunning durably records that an action may start executing. It is
// idempotent only while the entry remains in StateRunning.
func (j *Journal) MarkRunning(actionID string) (Entry, error) {
	return j.transition(actionID, StateRunning, nil)
}

// RecordOutcome durably records a completed local executor result. The result
// must belong to the action ID. Repeating the identical result is idempotent.
func (j *Journal) RecordOutcome(actionID string, result types.ActionResult) (Entry, error) {
	if result.ActionID != actionID {
		return Entry{}, fmt.Errorf("action result ID %q does not match journal action ID %q", result.ActionID, actionID)
	}
	if err := validateActionID(actionID); err != nil {
		return Entry{}, err
	}
	if err := validateResult(result); err != nil {
		return Entry{}, err
	}
	return j.transition(actionID, StateOutcomeKnown, &result)
}

// MarkOutcomeUnknown durably blocks automatic retry for an action whose
// outcome cannot be established. It is allowed after received or running so
// callers can conservatively stop before a side effect is dispatched.
func (j *Journal) MarkOutcomeUnknown(actionID string) (Entry, error) {
	return j.transition(actionID, StateOutcomeUnknown, nil)
}

// MarkReported durably records that the controller acknowledged the current
// known or unknown outcome. It does not discard the entry, which preserves
// idempotence across an agent restart within the journal retention window.
func (j *Journal) MarkReported(actionID string) (Entry, error) {
	return j.transition(actionID, StateReported, nil)
}

// Get returns a copy of the current entry for actionID.
func (j *Journal) Get(actionID string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry, ok := j.entries[actionID]
	return cloneEntry(entry), ok
}

// Len reports the number of action IDs retained in the journal.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}

// ListRecoverable returns a deterministic snapshot of every action that has
// not yet received a durable controller acknowledgement. The caller decides
// whether an entry's persisted lease is still current before resuming it.
func (j *Journal) ListRecoverable() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()

	ids := make([]string, 0, len(j.entries))
	for id, entry := range j.entries {
		if entry.State != StateReported {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, cloneEntry(j.entries[id]))
	}
	return entries
}

// CompactReported drops reported (controller-acknowledged) entries whose last
// update is older than retention and rewrites the journal as one minimal
// valid history per surviving action. It returns how many actions were
// dropped. Without compaction a long-lived agent eventually hits the action
// or byte limit and refuses all further work.
func (j *Journal) CompactReported(retention time.Duration) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.compactReportedLocked(retention)
}

// compactReportedLocked implements CompactReported with j.mu held.
func (j *Journal) compactReportedLocked(retention time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-retention)
	survivors := make(map[string]Entry, len(j.entries))
	removed := 0
	for id, entry := range j.entries {
		if entry.State == StateReported && entry.UpdatedAt.Before(cutoff) {
			removed++
			continue
		}
		survivors[id] = entry
	}
	if removed == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(survivors))
	for id := range survivors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := make([]diskRecord, 0, len(survivors)*4)
	for _, id := range ids {
		records = append(records, entryHistory(survivors[id])...)
	}
	if err := j.rewriteRecordsLocked(records); err != nil {
		return 0, fmt.Errorf("rewrite compacted action journal: %w", err)
	}
	j.entries = survivors
	return removed, nil
}

// entryHistory synthesizes the minimal record chain that reproduces entry
// when replayed through applyRecordLocked: the loader requires every action
// to begin at "received" and follow legal transitions, so a compacted file
// keeps exactly the same invariants as an organically grown one.
func entryHistory(entry Entry) []diskRecord {
	var states []State
	switch entry.State {
	case StateReceived:
		states = []State{StateReceived}
	case StateRunning:
		states = []State{StateReceived, StateRunning}
	case StateOutcomeKnown:
		states = []State{StateReceived, StateRunning, StateOutcomeKnown}
	case StateOutcomeUnknown:
		states = []State{StateReceived, StateOutcomeUnknown}
	case StateReported:
		if entry.Result != nil {
			states = []State{StateReceived, StateRunning, StateOutcomeKnown, StateReported}
		} else {
			states = []State{StateReceived, StateOutcomeUnknown, StateReported}
		}
	}
	records := make([]diskRecord, 0, len(states))
	for _, state := range states {
		e := cloneEntry(entry)
		e.State = state
		switch state {
		case StateReceived, StateRunning, StateOutcomeUnknown:
			e.Result = nil
		case StateOutcomeKnown, StateReported:
			e.Result = cloneResult(entry.Result)
		}
		records = append(records, diskRecord{Version: journalVersion, Entry: e})
	}
	return records
}

func (j *Journal) transition(actionID string, next State, result *types.ActionResult) (Entry, error) {
	if err := validateActionID(actionID); err != nil {
		return Entry{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	current, ok := j.entries[actionID]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, actionID)
	}
	if current.State == next {
		switch next {
		case StateOutcomeKnown:
			if result == nil || !sameResult(current.Result, result) {
				return Entry{}, fmt.Errorf("%w: %q", ErrOutcomeConflict, actionID)
			}
		case StateReported:
			// A reported outcome is immutable; there is no additional payload to
			// compare, so this is the natural replay of a successful POST.
		case StateRunning, StateOutcomeUnknown:
			// Repeating the same marker does not change an outcome.
		default:
			return Entry{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.State, next)
		}
		return cloneEntry(current), nil
	}
	if !allowedTransition(current.State, next) {
		return Entry{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.State, next)
	}

	entry := cloneEntry(current)
	entry.State = next
	entry.UpdatedAt = time.Now().UTC()
	switch next {
	case StateOutcomeKnown:
		entry.Result = cloneResult(result)
	case StateOutcomeUnknown:
		entry.Result = nil
	case StateReported:
		// Retain an existing known result. A nil result carries the fact that
		// the reported outcome was unknown. The lease has served its purpose
		// and must not be retained as a reusable credential.
		entry.LeaseToken = ""
		entry.LeaseExpiresAt = time.Time{}
	}
	if err := j.appendLocked(entry); err != nil {
		return Entry{}, err
	}
	return cloneEntry(entry), nil
}

func allowedTransition(current, next State) bool {
	switch current {
	case StateReceived:
		return next == StateRunning || next == StateOutcomeUnknown
	case StateRunning:
		return next == StateOutcomeKnown || next == StateOutcomeUnknown
	case StateOutcomeKnown, StateOutcomeUnknown:
		return next == StateReported
	default:
		return false
	}
}

func (j *Journal) appendLocked(entry Entry) error {
	if err := validateEntry(entry); err != nil {
		return err
	}
	record := diskRecord{Version: journalVersion, Entry: entry}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode action journal record: %w", err)
	}
	line = append(line, '\n')
	if len(line) > maxRecordBytes {
		return fmt.Errorf("action journal record exceeds %d bytes", maxRecordBytes)
	}
	if j.bytes+int64(len(line)) > maxJournalBytes {
		if _, err := j.compactReportedLocked(reportedRetention); err != nil {
			return fmt.Errorf("compact action journal at byte limit: %w", err)
		}
	}
	if j.bytes+int64(len(line)) > maxJournalBytes {
		return fmt.Errorf("action journal exceeds %d byte limit", maxJournalBytes)
	}

	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open action journal for append: %w", err)
	}
	if err := writeAll(f, line); err != nil {
		_ = f.Close()
		return fmt.Errorf("append action journal record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync action journal record: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close action journal: %w", err)
	}
	j.entries[entry.Action.ID] = cloneEntry(entry)
	j.bytes += int64(len(line))
	return nil
}

func (j *Journal) applyRecordLocked(record diskRecord) error {
	if record.Version != journalVersion {
		return fmt.Errorf("unsupported action journal record version %d", record.Version)
	}
	if err := validateEntry(record.Entry); err != nil {
		return err
	}
	id := record.Entry.Action.ID
	current, exists := j.entries[id]
	if !exists {
		if record.Entry.State != StateReceived {
			return fmt.Errorf("action %q begins at state %q", id, record.Entry.State)
		}
		if len(j.entries) >= maxActions {
			return fmt.Errorf("action journal has more than %d actions", maxActions)
		}
		j.entries[id] = cloneEntry(record.Entry)
		return nil
	}
	if !sameAction(current.Action, record.Entry.Action) {
		return fmt.Errorf("%w: %q", ErrActionConflict, id)
	}
	if current.State == record.Entry.State {
		if !sameStatePayload(current, record.Entry) {
			return fmt.Errorf("conflicting repeated action journal record for %q", id)
		}
		if !sameClaim(current, record.Entry) {
			// SetClaim is the only same-state mutation. It may add or replace a
			// non-empty claim before an action is reported; it may never erase a
			// claim or revive a reported entry.
			if current.State == StateReported || record.Entry.LeaseToken == "" {
				return fmt.Errorf("conflicting action journal claim for %q", id)
			}
		}
		j.entries[id] = cloneEntry(record.Entry)
		return nil
	}
	if !allowedTransition(current.State, record.Entry.State) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.State, record.Entry.State)
	}
	if record.Entry.State == StateOutcomeKnown && record.Entry.Result == nil {
		return fmt.Errorf("action %q known outcome has no result", id)
	}
	if record.Entry.State == StateOutcomeUnknown && record.Entry.Result != nil {
		return fmt.Errorf("action %q unknown outcome has a result", id)
	}
	if record.Entry.State == StateReported && !sameResult(current.Result, record.Entry.Result) {
		return fmt.Errorf("action %q reported outcome differs from prior outcome", id)
	}
	if record.Entry.State != StateReported && !sameClaim(current, record.Entry) {
		return fmt.Errorf("action %q changed claim during state transition", id)
	}
	j.entries[id] = cloneEntry(record.Entry)
	return nil
}

func ensureFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(dir)
}

// readRecords accepts a malformed final line as a recoverable torn tail. The
// returned records are complete JSON lines, preserving exactly what will be
// retained if recovery needs to rewrite the file.
func readRecords(path string) ([]diskRecord, int64, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read action journal: %w", err)
	}
	if int64(len(data)) > maxJournalBytes {
		return nil, 0, false, fmt.Errorf("action journal exceeds %d byte limit", maxJournalBytes)
	}
	if len(data) == 0 {
		return nil, 0, false, nil
	}

	lines := bytes.SplitAfter(data, []byte{'\n'})
	records := make([]diskRecord, 0, len(lines))
	var retained int64
	for index, raw := range lines {
		if len(raw) == 0 {
			continue
		}
		last := index == len(lines)-1
		complete := raw[len(raw)-1] == '\n'
		line := bytes.TrimSuffix(raw, []byte{'\n'})
		if len(line) == 0 || len(raw) > maxRecordBytes {
			if last {
				return records, retained, true, nil
			}
			return nil, 0, false, fmt.Errorf("invalid action journal record %d", index+1)
		}
		var record diskRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if last {
				return records, retained, true, nil
			}
			return nil, 0, false, fmt.Errorf("decode action journal record %d: %w", index+1, err)
		}
		if !complete {
			// A complete JSON object without its newline still came from a
			// partial append. Treat it as not committed; accepted appends always
			// fsync the newline in the same write before returning success.
			return records, retained, true, nil
		}
		records = append(records, record)
		retained += int64(len(raw))
	}
	return records, retained, false, nil
}

func (j *Journal) rewriteRecordsLocked(records []diskRecord) (retErr error) {
	dir := filepath.Dir(j.path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(j.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	var written int64
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			_ = tmp.Close()
			return err
		}
		line = append(line, '\n')
		if err := writeAll(tmp, line); err != nil {
			_ = tmp.Close()
			return err
		}
		written += int64(len(line))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, j.path); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	j.bytes = written
	return nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if err := validateAction(entry.Action); err != nil {
		return err
	}
	if entry.UpdatedAt.IsZero() {
		return fmt.Errorf("action journal updated time is required")
	}
	if entry.LeaseToken == "" {
		if !entry.LeaseExpiresAt.IsZero() {
			return fmt.Errorf("action journal lease expiry requires a token")
		}
	} else if err := validateClaim(entry.LeaseToken, entry.LeaseExpiresAt); err != nil {
		return err
	}
	switch entry.State {
	case StateReceived, StateRunning, StateOutcomeUnknown:
		if entry.Result != nil {
			return fmt.Errorf("action journal state %q cannot include a result", entry.State)
		}
	case StateOutcomeKnown:
		if entry.Result == nil {
			return fmt.Errorf("action journal known outcome requires a result")
		}
		if entry.Result.ActionID != entry.Action.ID {
			return fmt.Errorf("action journal result ID %q does not match action ID %q", entry.Result.ActionID, entry.Action.ID)
		}
		if err := validateResult(*entry.Result); err != nil {
			return err
		}
	case StateReported:
		if entry.LeaseToken != "" || !entry.LeaseExpiresAt.IsZero() {
			return fmt.Errorf("reported action journal entry cannot retain a lease")
		}
		if entry.Result != nil {
			if entry.Result.ActionID != entry.Action.ID {
				return fmt.Errorf("action journal result ID %q does not match action ID %q", entry.Result.ActionID, entry.Action.ID)
			}
			if err := validateResult(*entry.Result); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown action journal state %q", entry.State)
	}
	return nil
}

func validateAction(action types.Action) error {
	if err := validateActionID(action.ID); err != nil {
		return err
	}
	if len(action.Type) == 0 || len(action.Type) > maxActionTypeBytes {
		return fmt.Errorf("action type must be between 1 and %d bytes", maxActionTypeBytes)
	}
	if len(action.Params) > maxParams {
		return fmt.Errorf("action has more than %d params", maxParams)
	}
	for key, value := range action.Params {
		if key == "" || len(key) > maxParamBytes || len(value) > maxParamBytes {
			return fmt.Errorf("action parameter %q exceeds journal bounds", key)
		}
	}
	if action.Timeout < 0 {
		return fmt.Errorf("action timeout must not be negative")
	}
	return nil
}

func validateActionID(actionID string) error {
	if strings.TrimSpace(actionID) == "" || len(actionID) > maxActionIDBytes {
		return fmt.Errorf("action ID must be between 1 and %d bytes", maxActionIDBytes)
	}
	return nil
}

func validateClaim(token string, expiresAt time.Time) error {
	if strings.TrimSpace(token) != token || token == "" || len(token) > maxLeaseTokenBytes {
		return fmt.Errorf("action lease token must be between 1 and %d non-whitespace bytes", maxLeaseTokenBytes)
	}
	if expiresAt.IsZero() {
		return fmt.Errorf("action lease expiry is required")
	}
	return nil
}

func validateResult(result types.ActionResult) error {
	if err := validateActionID(result.ActionID); err != nil {
		return err
	}
	if len(result.Output) > maxResultTextBytes || len(result.Error) > maxResultTextBytes {
		return fmt.Errorf("action result text exceeds %d bytes", maxResultTextBytes)
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() {
		return fmt.Errorf("action result start and finish times are required")
	}
	if result.FinishedAt.Before(result.StartedAt) {
		return fmt.Errorf("action result finish time is before start time")
	}
	return nil
}

func sameStatePayload(a, b Entry) bool {
	return sameAction(a.Action, b.Action) && a.State == b.State && sameResult(a.Result, b.Result)
}

func sameClaim(a, b Entry) bool {
	return a.LeaseToken == b.LeaseToken && a.LeaseExpiresAt.Equal(b.LeaseExpiresAt)
}

func sameAction(a, b types.Action) bool {
	if a.ID != b.ID || a.Type != b.Type || a.Timeout != b.Timeout || len(a.Params) != len(b.Params) {
		return false
	}
	for key, value := range a.Params {
		if b.Params[key] != value {
			return false
		}
	}
	return true
}

func sameResult(a, b *types.ActionResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func cloneEntry(entry Entry) Entry {
	entry.Action = cloneAction(entry.Action)
	entry.Result = cloneResult(entry.Result)
	return entry
}

func cloneAction(action types.Action) types.Action {
	if action.Params == nil {
		return action
	}
	params := make(map[string]string, len(action.Params))
	for key, value := range action.Params {
		params[key] = value
	}
	action.Params = params
	return action
}

func cloneResult(result *types.ActionResult) *types.ActionResult {
	if result == nil {
		return nil
	}
	cloned := *result
	return &cloned
}
