package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// acceleratorHostState is the exact state the quiesce is allowed to change.
// It intentionally records only the node-wide persistence daemon and the
// targeted GPU's persistence mode; nothing else on the host is inferred.
type acceleratorHostState struct {
	GPUIndex int `json:"gpu_index"`
	// GPUUUID is the stable identity of the quiesced device. Restore re-applies
	// persistence mode against this UUID, not GPUIndex, so an enumeration shift
	// between quiesce and restore cannot enable persistence on whichever device
	// now holds the old index. It is also the snapshot's identity for retries.
	GPUUUID               string `json:"gpu_uuid"`
	PersistenceKnown      bool   `json:"persistence_known"`
	PersistenceWasEnabled bool   `json:"persistence_was_enabled"`
	ServiceWasActive      bool   `json:"service_was_active"`
}

type acceleratorHostStateFile struct {
	Version int                             `json:"version"`
	Nodes   map[string]acceleratorHostState `json:"nodes"`
}

const acceleratorHostStateVersion = 1

func (e *Executor) loadAcceleratorHostState(key string) (acceleratorHostState, bool, error) {
	data, err := os.ReadFile(e.acceleratorHostStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return acceleratorHostState{}, false, nil
	}
	if err != nil {
		return acceleratorHostState{}, false, fmt.Errorf("read accelerator host state: %w", err)
	}
	var stateFile acceleratorHostStateFile
	if err := json.Unmarshal(data, &stateFile); err != nil {
		return acceleratorHostState{}, false, fmt.Errorf("decode accelerator host state: %w", err)
	}
	if stateFile.Version != acceleratorHostStateVersion || stateFile.Nodes == nil {
		return acceleratorHostState{}, false, fmt.Errorf("unsupported accelerator host state")
	}
	state, ok := stateFile.Nodes[key]
	return state, ok, nil
}

func (e *Executor) saveAcceleratorHostState(key string, state acceleratorHostState) error {
	stateFile, err := e.readAcceleratorHostStateFile()
	if err != nil {
		return err
	}
	if previous, exists := stateFile.Nodes[key]; exists {
		// The stored snapshot is the PRE-mutation host state — the thing restore
		// must put back. A retry after a partially-completed quiesce recomputes the
		// live state, which by then reflects the mutations attempt 1 already made
		// (daemon stopped, persistence disabled), so it legitimately differs from
		// the snapshot. Comparing the two and erroring made every retry fail until
		// the incident died. Keep the original snapshot and proceed; only a change
		// of device identity is a genuine conflict, because restore could not honor
		// a key that now names a different GPU.
		if previous.GPUIndex != state.GPUIndex || previous.GPUUUID != state.GPUUUID {
			return fmt.Errorf("accelerator host state for %q conflicts with an earlier quiesce of a different GPU (stored index %d uuid %q, now index %d uuid %q)",
				key, previous.GPUIndex, previous.GPUUUID, state.GPUIndex, state.GPUUUID)
		}
		return nil
	}
	stateFile.Nodes[key] = state
	return e.writeAcceleratorHostStateFile(stateFile)
}

func (e *Executor) deleteAcceleratorHostState(key string) error {
	// No os.ErrNotExist branch: readAcceleratorHostStateFile turns a missing
	// file into a valid empty one and can never return it. A dead check here
	// reads as a handled case and is not one, which is worse than its absence.
	stateFile, err := e.readAcceleratorHostStateFile()
	if err != nil {
		return err
	}
	if _, exists := stateFile.Nodes[key]; !exists {
		return nil
	}
	delete(stateFile.Nodes, key)
	return e.writeAcceleratorHostStateFile(stateFile)
}

func (e *Executor) readAcceleratorHostStateFile() (acceleratorHostStateFile, error) {
	data, err := os.ReadFile(e.acceleratorHostStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return acceleratorHostStateFile{Version: acceleratorHostStateVersion, Nodes: map[string]acceleratorHostState{}}, nil
	}
	if err != nil {
		return acceleratorHostStateFile{}, fmt.Errorf("read accelerator host state: %w", err)
	}
	var stateFile acceleratorHostStateFile
	if err := json.Unmarshal(data, &stateFile); err != nil {
		return acceleratorHostStateFile{}, fmt.Errorf("decode accelerator host state: %w", err)
	}
	if stateFile.Version != acceleratorHostStateVersion || stateFile.Nodes == nil {
		return acceleratorHostStateFile{}, fmt.Errorf("unsupported accelerator host state")
	}
	return stateFile, nil
}

func (e *Executor) writeAcceleratorHostStateFile(stateFile acceleratorHostStateFile) error {
	data, err := json.Marshal(stateFile)
	if err != nil {
		return fmt.Errorf("encode accelerator host state: %w", err)
	}
	dir := filepath.Dir(e.acceleratorHostStatePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create accelerator host state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".accelerator-host-state-*")
	if err != nil {
		return fmt.Errorf("create accelerator host state: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect accelerator host state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write accelerator host state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync accelerator host state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close accelerator host state: %w", err)
	}
	if err := os.Rename(tmpPath, e.acceleratorHostStatePath); err != nil {
		return fmt.Errorf("replace accelerator host state: %w", err)
	}
	// fsync the directory as well as the file: rename is atomic, but without
	// this sync a power loss may still make the durable snapshot disappear.
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open accelerator host state directory: %w", err)
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("sync accelerator host state directory: %w", err)
	}
	return nil
}

func hostStateKey(params map[string]string) (string, error) {
	key := strings.TrimSpace(params["host_state_key"])
	if key == "" {
		return "", fmt.Errorf("host_state_key param is required")
	}
	if len(key) > 253 || strings.ContainsAny(key, "/\\\x00") {
		return "", fmt.Errorf("host_state_key is invalid")
	}
	return key, nil
}
