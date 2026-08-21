package kmsg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// cursorVersion guards the on-disk cursor format. A future incompatible
// change bumps it, and an unrecognized version is treated as "no cursor" so
// the watcher fails safe to tail-seek rather than misinterpreting old state.
const cursorVersion = 1

// cursorFile is the durable record of how far the watcher has consumed the
// kernel log. Persisting the last-processed sequence number lets a restarted
// agent resume from where it left off instead of seeking to the tail and
// silently dropping every XID printed while it was down.
//
// BootID binds the sequence to the boot it was recorded under. /dev/kmsg
// sequence numbers are per-boot and restart near zero after a reboot, so a
// cursor carrying a pre-reboot (large) seq would suppress every XID printed by
// the new boot. A reboot is an ordinary escalation-ladder rung, so this is the
// normal post-remediation state; binding the boot ID lets a load detect the
// mismatch and fail safe to tail-seek instead.
type cursorFile struct {
	Version int    `json:"version"`
	BootID  string `json:"boot_id,omitempty"`
	Seq     uint64 `json:"seq"`
}

// loadCursor reads the persisted sequence number for the current boot. A
// missing cursor returns ok=false with no error; decode/version failures are
// surfaced so the caller can log them. In every non-usable case the caller
// must fail safe to tail-seek.
//
// The cursor is only usable when its recorded boot ID matches the live one:
// a mismatch (a normal reboot), an absent recorded boot ID (an old cursor
// written before boot binding), or an unknown live boot ID (bootID == "",
// e.g. a non-Linux dev host) all yield ok=false so the watcher tail-seeks
// rather than replaying a stale, cross-boot sequence. The zero seq is likewise
// treated as "no usable cursor": kmsg sequence numbers a restarted agent
// resumes from are always positive in practice, and 0 would replay the whole
// ring on every start.
func loadCursor(path, bootID string) (uint64, bool, error) {
	if path == "" {
		return 0, false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read kmsg cursor: %w", err)
	}
	var cf cursorFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return 0, false, fmt.Errorf("decode kmsg cursor: %w", err)
	}
	// `cf.Seq == 0` stays, and deliberately, unlike the two other zero-checks
	// in this package that were removed.
	//
	// The file has no "is there a cursor" bit, so a persisted 0 is genuinely
	// indistinguishable from an absent cursor — and resuming from 0 means
	// replaying the entire ring on every start, which is far worse than the
	// cost of not resuming: at most one re-delivered event at sequence 0, and
	// re-delivery is safe because the controller deduplicates by capture ID.
	//
	// The defect the other two had is absent here for that reason. Adding a
	// bit to the file would need a format version bump to buy back one event
	// that is the boot banner in practice.
	if cf.Version != cursorVersion || cf.Seq == 0 {
		return 0, false, fmt.Errorf("unsupported kmsg cursor")
	}
	if bootID == "" || cf.BootID == "" || cf.BootID != bootID {
		// Cross-boot (or unbindable) cursor: fail safe to tail-seek. This is the
		// expected state after a reboot, so it is not surfaced as an error.
		return 0, false, nil
	}
	return cf.Seq, true, nil
}

// saveCursor durably records seq for the given boot using the same crash-safe
// temp+rename+fsync pattern the executor's accelerator state uses: a torn
// write can never leave a partially rewritten cursor that would be misread as
// a valid, smaller resume point.
func saveCursor(path, bootID string, seq uint64) (retErr error) {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(cursorFile{Version: cursorVersion, BootID: bootID, Seq: seq})
	if err != nil {
		return fmt.Errorf("encode kmsg cursor: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create kmsg cursor directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".kmsg-cursor-*")
	if err != nil {
		return fmt.Errorf("create kmsg cursor: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect kmsg cursor: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write kmsg cursor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync kmsg cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close kmsg cursor: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace kmsg cursor: %w", err)
	}
	// fsync the directory so the rename survives a power loss, matching the
	// durability the accelerator-state writer guarantees.
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open kmsg cursor directory: %w", err)
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("sync kmsg cursor directory: %w", err)
	}
	return nil
}
