package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type failingNodeConfigStore struct {
	store.Store
	err error
}

func TestRunRejectsUnsupportedBareMetalBeforeOpeningState(t *testing.T) {
	err := run(
		slog.New(slog.NewTextHandler(io.Discard, nil)), ":0", agentServerConfig{}, runtimeConfigPaths{},
		"", "baremetal", "", "", false, humanAuth{}, "", false, notifyFiles{},
		false, 0, 0, "", "", "sqlite", "", electionConfig{}, "", "",
	)
	if err == nil || !strings.Contains(err.Error(), "baremetal is not supported") {
		t.Fatalf("baremetal startup error = %v, want a clear unsupported-platform error", err)
	}
}

func TestAlertmanagerWebhookRequiresExplicitInsecureOptIn(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	request := func(api *httpapi.Server) int {
		rec := httptest.NewRecorder()
		api.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/webhooks/alertmanager", strings.NewReader(`{"alerts":[]}`)))
		return rec.Code
	}

	secure := httpapi.New(nil)
	if err := configureAlertmanagerWebhook(secure, "", false, log); err != nil {
		t.Fatal(err)
	}
	if got := request(secure); got != http.StatusUnauthorized {
		t.Fatalf("webhook without token or opt-in = %d, want 401", got)
	}

	development := httpapi.New(nil)
	if err := configureAlertmanagerWebhook(development, "", true, log); err != nil {
		t.Fatal(err)
	}
	if got := request(development); got != http.StatusAccepted {
		t.Fatalf("explicit insecure webhook = %d, want 202", got)
	}
	if err := configureAlertmanagerWebhook(httpapi.New(nil), "/token", true, log); err == nil {
		t.Fatal("token file plus insecure opt-in must be rejected")
	}
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

// R12 (review M2): the digest markers feed the CHANGE detector, and a
// playbooks-only edit must therefore reload. Two markers that disagree mean
// the ConfigMaps are mid-sync: publish no identity rather than one that
// describes half of what is loaded.
func TestConfigDigestMarkersDriveReloadAndDisagreementPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	policies := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(policies,
		[]byte("policies:\n  - match: {class: gsp-error}\n    playbook: observe\nsafety: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "observe.yaml"),
		[]byte("name: observe\ntarget: gpu\nsteps:\n  - name: observe\n    action: notify.observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := runtimeConfigPaths{policies: policies, playbooks: playbooks}

	before, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}
	// A marker landing in the PLAYBOOKS mount alone must move the digest —
	// otherwise a playbooks-only rollout never reloads and the published
	// identity stays wrong forever.
	if err := os.WriteFile(filepath.Join(playbooks, "config-digest"), []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := runtimeConfigDigest(paths)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("a config-digest marker change must move the reload digest")
	}

	// Only one marker: that is the identity.
	if got := readSourceDigest(paths); got != "abc123" {
		t.Fatalf("source digest = %q, want abc123", got)
	}
	// Markers disagree (mid-sync): no identity at all.
	if err := os.WriteFile(filepath.Join(dir, "config-digest"), []byte("def456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSourceDigest(paths); got != "" {
		t.Fatalf("source digest = %q, want empty while the mounts disagree", got)
	}
	// Both agree: identity again.
	if err := os.WriteFile(filepath.Join(dir, "config-digest"), []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readSourceDigest(paths); got != "abc123" {
		t.Fatalf("source digest = %q after both mounts agree, want abc123", got)
	}
}

// R11.3: a successful apply publishes the operator-compiled snapshot digest
// (the "config-digest" file beside the mounted config) as the loaded-config
// identity — on readyz, and only after everything parsed. An absent file is
// a file-based deployment, not an error.
func TestApplyRuntimeConfigPublishesSourceDigest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	policies := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(policies,
		[]byte("policies:\n  - match: {class: gsp-error}\n    playbook: observe\nsafety: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "observe.yaml"),
		[]byte("name: observe\ntarget: gpu\nsteps:\n  - name: observe\n    action: notify.observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctrl := controller.New(st, nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := httpapi.New(ctrl)
	paths := runtimeConfigPaths{policies: policies, playbooks: playbooks}
	logSink := slog.New(slog.NewTextHandler(io.Discard, nil))

	readyz := func() string {
		rec := httptest.NewRecorder()
		api.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Body.String()
	}

	// No digest file: identity absent, readyz stays plain.
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, logSink); err != nil {
		t.Fatal(err)
	}
	if got := readyz(); got != "ready" {
		t.Fatalf("readyz without digest = %q, want plain ready", got)
	}

	// Digest file present: identity published on readyz.
	if err := os.WriteFile(filepath.Join(dir, "config-digest"), []byte("abc123def456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, logSink); err != nil {
		t.Fatal(err)
	}
	if got := readyz(); got != "ready config=abc123def456" {
		t.Fatalf("readyz with digest = %q", got)
	}

	// A failed apply must NOT advance the published identity.
	if err := os.WriteFile(filepath.Join(dir, "config-digest"), []byte("NEWDIGEST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "broken.yaml"), []byte("name: [unparsable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, logSink); err == nil {
		t.Fatal("apply with a broken playbook must fail")
	}
	if got := readyz(); got != "ready config=abc123def456" {
		t.Fatalf("readyz after failed apply = %q, want the previous identity retained", got)
	}
}

// TestReloadAppliesExecutionMode is the defect a real-cluster cordon phase
// found on its first successful run.
//
// The operator deliberately keeps the config-digest OFF the controller's pod
// template, so an ordinary configuration change reloads in place instead of
// rolling the Deployment — which under leader election would deadlock. But the
// reload never re-applied the safety gate's dry-run flag: it was read once, at
// process start, and `SetDryRun` had no caller anywhere in the tree.
//
// So `spec.safety.executionMode` did nothing to a running controller. Both
// directions are wrong and the second is dangerous: switching DryRun→Enabled
// left an installation that believes it is armed executing nothing, and
// switching Enabled→DryRun — the lever an operator reaches for to STOP damage
// — left it executing.
func TestReloadAppliesExecutionMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "observe.yaml"),
		[]byte("name: observe\ntarget: gpu\nsteps:\n  - name: observe\n    action: notify.observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policies := filepath.Join(dir, "runtime.yaml")
	write := func(dryRun bool, remediations int) {
		t.Helper()
		body := fmt.Sprintf(
			"policies:\n  - match: {class: gsp-error}\n    playbook: observe\n"+
				"safety:\n  dry_run: %t\n  max_concurrent_remediations: %d\n",
			dryRun, remediations)
		if err := os.WriteFile(policies, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 2, DryRun: true})
	ctrl := controller.New(st, nil, nil, gate, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := httpapi.New(ctrl)
	paths := runtimeConfigPaths{policies: policies, playbooks: playbooks}
	logSink := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The operator switches the installation to Enabled.
	write(false, 4)
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, logSink); err != nil {
		t.Fatal(err)
	}
	if gate.DryRun() {
		t.Fatal("executionMode: Enabled reloaded, but the gate is still in dry-run; " +
			"the installation believes it is armed and executes nothing")
	}

	// And back — the lever an operator pulls to stop damage.
	write(true, 4)
	if err := applyRuntimeConfig(ctx, ctrl, api, paths, logSink); err != nil {
		t.Fatal(err)
	}
	if !gate.DryRun() {
		t.Fatal("executionMode: DryRun reloaded, but the gate is still executing; " +
			"the documented way to stop remediation does not stop it")
	}
}

func TestStandbyRuntimeReloadDoesNotWriteLeaderOwnedNodePauses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	playbooks := filepath.Join(dir, "playbooks")
	if err := os.MkdirAll(playbooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(playbooks, "observe.yaml"),
		[]byte("name: observe\ntarget: gpu\nsteps:\n  - name: observe\n    action: notify.observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policies := filepath.Join(dir, "policies.yaml")
	if err := os.WriteFile(policies, []byte("policies:\n  - match: {class: gsp-error}\n    playbook: observe\nsafety: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nodeConfigs := filepath.Join(dir, "nodes.yaml")
	if err := os.WriteFile(nodeConfigs, []byte("nodes:\n  - node_name: leader-intent\n    paused: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ApplyNodeConfigPauses(ctx, []string{"old-leader-intent"}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(st, nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := httpapi.New(ctrl)
	paths := runtimeConfigPaths{policies: policies, playbooks: playbooks, nodeConfigs: nodeConfigs}
	logSink := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := applyRuntimeConfigWithNodePauses(ctx, ctrl, api, paths, logSink, false); err != nil {
		t.Fatalf("standby reload: %v", err)
	}
	old, err := st.GetNode(ctx, "old-leader-intent")
	if err != nil || !old.Paused {
		t.Fatalf("standby changed leader pause = %+v, %v; want retained paused node", old, err)
	}
	if _, err := st.GetNode(ctx, "leader-intent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("standby created configured node = %v, want ErrNotFound", err)
	}

	if err := applyRuntimeConfigWithNodePauses(ctx, ctrl, api, paths, logSink, true); err != nil {
		t.Fatalf("leader reload: %v", err)
	}
	old, err = st.GetNode(ctx, "old-leader-intent")
	if err != nil || old.Paused {
		t.Fatalf("leader did not replace old pause set = %+v, %v", old, err)
	}
	current, err := st.GetNode(ctx, "leader-intent")
	if err != nil || !current.Paused {
		t.Fatalf("leader pause set = %+v, %v; want leader-intent paused", current, err)
	}
}
