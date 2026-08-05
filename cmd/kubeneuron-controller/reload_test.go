package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/store"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type failingNodeConfigStore struct {
	store.Store
	err error
}

func (s failingNodeConfigStore) ApplyNodeConfigPauses(context.Context, []string) error {
	return s.err
}

// The reloader decides whether to re-apply by hashing the mounted files. It
// must see a content change, must ignore a rewrite that changes nothing, and
// must treat a configured-but-absent optional file as a stable empty rather
// than churning on every poll.
func TestRuntimeConfigDigestDetectsChange(t *testing.T) {
	dir := t.TempDir()
	policies := filepath.Join(dir, "policies.yaml")
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(policies, "safety: {}\n")
	write(filepath.Join(playbooks, "a.yaml"), "name: a\n")

	paths := runtimeConfigPaths{
		policies:  policies,
		playbooks: playbooks,
		windows:   filepath.Join(dir, "windows.yaml"), // absent, optional
	}

	d1, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}

	// An absent optional file must not make the digest move.
	d2, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("digest must be stable when nothing changed, including an absent optional file")
	}

	// A changed policy file must move the digest.
	write(policies, "safety: {dry_run: true}\n")
	d3, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}
	if d3 == d1 {
		t.Fatal("digest must change when a config file changes")
	}

	// An added playbook must move the digest too.
	write(filepath.Join(playbooks, "b.yaml"), "name: b\n")
	d4, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}
	if d4 == d3 {
		t.Fatal("digest must change when a playbook is added")
	}

	// A ConfigMap directory mount is not a plain directory: the kubelet keeps
	// its atomic-update machinery beside the real files as dot-prefixed dirs
	// and symlinks (..data -> ..2026_..._timestamp). Hashing those tried to
	// read a directory and errored, which on a live cluster made the reloader
	// skip forever. The digest must ignore them and stay computable.
	tsDir := filepath.Join(playbooks, "..2026_07_30_17_43_08.111")
	if err := os.MkdirAll(tsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsDir, "a.yaml"), []byte("name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tsDir, filepath.Join(playbooks, "..data")); err != nil {
		t.Fatal(err)
	}
	d5, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatalf("digest must stay computable over a ConfigMap-style directory: %v", err)
	}
	if d5 != d4 {
		t.Fatal("the kubelet's internal ..data/..timestamp entries must not affect the digest")
	}
}

// Applying node pause state is the only runtime-reload operation that can
// fail after every file has parsed. It must happen before installing any live
// settings, otherwise a transient database failure exposes a half-new config.
func TestApplyRuntimeConfigKeepsProfilesWhenNodeConfigStoreFails(t *testing.T) {
	ctx := context.Background()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertNode(ctx, &types.Node{Name: "node-a", Labels: map[string]string{"accelerator": "nvidia-h100"}}); err != nil {
		t.Fatal(err)
	}
	storeFailure := errors.New("database unavailable")
	ctrl := controller.New(
		failingNodeConfigStore{Store: st, err: storeFailure},
		nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	old := config.AcceleratorRuntimeProfile{
		Name:              "old-profile",
		NodeSelector:      map[string]string{"accelerator": "nvidia-h100"},
		Vendor:            types.AcceleratorVendorNVIDIA,
		ProfileDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DriverVersion:     "550.54.15",
		RuntimeVersion:    "dcgm-3.3.0",
		ProfileUID:        "profile-old",
		ProfileGeneration: 1,
		MaxReportAge:      config.Duration(time.Minute),
	}
	if err := ctrl.SetAcceleratorRuntimeProfiles([]config.AcceleratorRuntimeProfile{old}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	policies := filepath.Join(dir, "policies.yaml")
	if err := os.WriteFile(policies, []byte(`
policies:
  - match: {class: xid-app}
    playbook: observe
accelerator_profiles:
  - name: new-profile
    node_selector: {accelerator: nvidia-h100}
    vendor: nvidia
    profile_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    driver_version: "550.54.15"
    runtime_version: dcgm-3.3.0
    profile_uid: profile-new
    profile_generation: 2
    max_report_age: 1m
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "observe.yaml"), []byte("name: observe\ntarget: gpu\nsteps:\n  - name: observe\n    action: notify.observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = applyRuntimeConfig(ctx, ctrl, httpapi.New(ctrl), runtimeConfigPaths{policies: policies, playbooks: playbooks}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !errors.Is(err, storeFailure) {
		t.Fatalf("apply runtime config = %v, want node-config store failure", err)
	}
	profile, err := ctrl.AcceleratorObservationProfile(ctx, "node-a", types.AcceleratorVendorNVIDIA)
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil || profile.ProfileDigest != old.ProfileDigest {
		t.Fatalf("profile after failed reload = %#v, want old profile %q", profile, old.ProfileDigest)
	}
}
