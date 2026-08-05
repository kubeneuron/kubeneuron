package gpuhealth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateVersion guards the on-disk format of the emitted-XID record. A future
// incompatible change bumps it, and an unrecognized version is treated as "no
// record" so the watcher fails safe to baselining rather than misreading old
// state.
const stateVersion = 1

// stateFile durably records which level-triggered (GPU, code) observations have
// already been emitted this boot, so a routine agent DaemonSet rollout does not
// re-emit DCGM's retained last-XID and re-open incidents for long-remediated
// faults.
//
// BootID binds the record to the boot it was written under, mirroring the kmsg
// cursor: nv-hostengine's retained XID field is cleared when the host reboots
// (and a reboot is an ordinary escalation-ladder rung), so a record from a prior
// boot must not suppress a genuinely new fault on the new boot. A boot-ID
// mismatch is treated as "no usable record" — the watcher re-baselines rather
// than replaying stale, cross-boot emissions.
type stateFile struct {
	Version int               `json:"version"`
	BootID  string            `json:"boot_id,omitempty"`
	Emitted map[string]uint64 `json:"emitted"`
}

// loadHealthState reads the emitted-XID record for the current boot. A missing,
// unreadable, version-mismatched, or cross-boot record returns ok=false so the
// caller fails safe to baselining level series on first sight (never re-emitting
// a possibly long-remediated retained XID). An unreadable record surfaces its
// error so the caller can log it; it must not crash and must not spam.
func loadHealthState(path, bootID string) (map[string]uint64, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read gpuhealth state: %w", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, false, fmt.Errorf("decode gpuhealth state: %w", err)
	}
	if sf.Version != stateVersion {
		return nil, false, fmt.Errorf("unsupported gpuhealth state version %d", sf.Version)
	}
	if bootID == "" || sf.BootID == "" || sf.BootID != bootID {
		// Cross-boot (or unbindable) record: fail safe to baselining. The retained
		// XID field was cleared by the reboot, so this is the expected state and is
		// not surfaced as an error.
		return nil, false, nil
	}
	return sf.Emitted, true, nil
}

// saveHealthState durably records the emitted-XID map for the given boot using
// the same crash-safe temp+rename+fsync pattern as the kmsg cursor: a torn write
// can never leave a partially rewritten record that would be misread as a valid
// but incomplete set of emissions.
func saveHealthState(path, bootID string, emitted map[string]uint64) (retErr error) {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(stateFile{Version: stateVersion, BootID: bootID, Emitted: emitted})
	if err != nil {
		return fmt.Errorf("encode gpuhealth state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create gpuhealth state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".gpuhealth-state-*")
	if err != nil {
		return fmt.Errorf("create gpuhealth state: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) && retErr == nil {
			retErr = err
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect gpuhealth state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write gpuhealth state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync gpuhealth state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close gpuhealth state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace gpuhealth state: %w", err)
	}
	// fsync the directory so the rename survives a power loss, matching the
	// durability the kmsg cursor writer guarantees.
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open gpuhealth state directory: %w", err)
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("sync gpuhealth state directory: %w", err)
	}
	return nil
}
