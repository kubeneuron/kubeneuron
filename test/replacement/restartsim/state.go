// SPDX-License-Identifier: Apache-2.0

// Package restartsim stores the simulated cloud response on a rehearsal volume.
package restartsim

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func Load(path string, dst any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, dst)
}

// Save publishes a complete snapshot before the harness can observe acceptance.
func Save(path string, state any) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".restart-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
