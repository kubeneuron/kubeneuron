package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"log/slog"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/httpapi"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
)

// runtimeConfigReloadInterval is how often the controller re-reads its mounted
// configuration. It is a poll, not an event, because a mounted ConfigMap is
// updated by the kubelet on its own sync period (tens of seconds) and there is
// nothing to gain from reacting faster than the file can change.
const runtimeConfigReloadInterval = 15 * time.Second

// applyRuntimeConfig loads every runtime configuration file and installs it on
// the running controller and API without restarting the process.
//
// This is the whole of option C. Configuration used to reach the controller
// only by rolling the Deployment (the operator stamped a config-digest on the
// pod template), and under leader election that rollout deadlocks: only the
// leader is Ready, so a new pod can never become Ready to retire the old one,
// and the stale-config leader serves forever. Reloading in place removes the
// rollout from the config path entirely — a new profile or playbook takes
// effect on the next poll, on every replica, with no pod ever restarting.
//
// It is all-or-nothing per call: a file that fails to parse aborts the whole
// apply and leaves the previous configuration in force, because a half-applied
// configuration (new engine, old profiles) is worse than a stale-but-coherent
// one.
func applyRuntimeConfig(
	ctx context.Context,
	ctrl *controller.Controller,
	api *httpapi.Server,
	paths runtimeConfigPaths,
	log *slog.Logger,
) error {
	return applyRuntimeConfigWithNodePauses(ctx, ctrl, api, paths, log, true)
}

// applyRuntimeConfigWithNodePauses installs local runtime configuration on any
// replica, but persists the cluster-wide GPUNodeConfig pause set only when the
// caller owns the leader lease. A standby must be ready to take over without
// being allowed to erase the current leader's durable node intent.
func applyRuntimeConfigWithNodePauses(
	ctx context.Context,
	ctrl *controller.Controller,
	api *httpapi.Server,
	paths runtimeConfigPaths,
	log *slog.Logger,
	persistNodePauses bool,
) error {
	cfg, err := config.Load(paths.policies)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	books, err := playbook.LoadDir(paths.playbooks)
	if err != nil {
		return fmt.Errorf("loading playbooks: %w", err)
	}
	policies := make([]playbook.Policy, 0, len(cfg.Policies))
	for _, p := range cfg.Policies {
		policies = append(policies, playbook.Policy{Class: p.Match.Class, Vendor: p.Match.Vendor, Playbook: p.Playbook, Params: p.Params})
	}
	engine, err := playbook.NewEngine(books, policies)
	if err != nil {
		return fmt.Errorf("building playbook engine: %w", err)
	}

	var windows []config.MaintenanceWindow
	if paths.windows != "" {
		windows, err = config.LoadWindows(paths.windows)
		if err != nil {
			return fmt.Errorf("loading maintenance windows: %w", err)
		}
	}

	var catalog *detect.Catalog
	signalRules := 0
	if paths.mappings != "" {
		overrides, err := config.LoadSignalOverrides(paths.mappings)
		if err != nil {
			return fmt.Errorf("loading signal mappings: %w", err)
		}
		signalRules = len(overrides)
		if len(overrides) > 0 {
			catalog, err = detect.NewCatalog(overrides)
			if err != nil {
				return fmt.Errorf("building signal catalog: %w", err)
			}
		}
	}

	var nodeConfigs []config.NodeConfig
	if paths.nodeConfigs != "" {
		nodeConfigs, err = config.LoadNodeConfigs(paths.nodeConfigs)
		if err != nil {
			return fmt.Errorf("loading node configs: %w", err)
		}
	}

	// Everything parsed. Validate before the only fallible installation step.
	// The node pause set is durable cluster state, not a process-local runtime
	// setting, so only the leader is allowed to write it.
	if err := controller.ValidateAcceleratorRuntimeProfiles(cfg.AcceleratorProfiles); err != nil {
		return fmt.Errorf("validating accelerator runtime profiles: %w", err)
	}
	if persistNodePauses {
		if err := ctrl.ApplyNodeConfigs(ctx, nodeConfigs); err != nil {
			return fmt.Errorf("applying node configs: %w", err)
		}
	}
	// ONE atomic install: a reconcile pass observes either the whole previous
	// configuration or the whole new one, never a mixture of generations. The
	// controller installs first, the API's alert catalog second — the two
	// components classify independent inputs, so the sub-second skew between
	// them is harmless.
	// The operator writes the compiled snapshot's digest beside the config
	// files it mounts (the "config-digest" ConfigMap key). Reading it here —
	// and only on a fully successful apply — is what lets /readyz, the
	// operator API, and the info metric answer "which configuration is
	// live?". Absent file (file-based deployments, older operators) means no
	// identity, never an error.
	sourceDigest := readSourceDigest(paths)
	// The safety limits travel inside the immutable snapshot.  InstallRuntimeConfig
	// updates the gate's slot-accounting limits too, but execution reads this
	// snapshot for the mode so it cannot observe a newly Enabled gate with an old
	// empty destructive selector.
	limits := safety.Limits{
		MaxConcurrentRemediations: cfg.Safety.MaxConcurrentRemediations,
		MaxConcurrentReboots:      cfg.Safety.MaxConcurrentReboots,
		DryRun:                    cfg.Safety.DryRun,
	}
	if err := ctrl.InstallRuntimeConfigContext(ctx, controller.RuntimeConfig{
		SafetyLimits:        &limits,
		Engine:              engine,
		Catalog:             catalog,
		SourceDigest:        sourceDigest,
		Windows:             windows,
		AcceleratorProfiles: cfg.AcceleratorProfiles,
		QuiesceForbidden:    cfg.Safety.QuiesceForbidResetWhenPresent,
		DestructiveSelector: cfg.Safety.DestructiveExecutionNodeSelector,
		DegradedTaint:       degradedTaintPolicy(cfg.Safety.TaintDegradedNodes),
		VerifyQuiet:         cfg.Safety.VerifyQuietWindow.Std(),
		ApprovalTTL:         cfg.Approvals.TTL.Std(),
	}); err != nil {
		// Validation above makes this unreachable; retain the guard if validation
		// rules ever become stateful in a future controller version.
		return fmt.Errorf("installing runtime configuration: %w", err)
	}
	api.SetSignalCatalog(catalog)
	api.SetRuntimeConfigInfo(httpapi.RuntimeConfigInfo{
		SourceDigest:  sourceDigest,
		LoadedAt:      time.Now(),
		Playbooks:     len(books),
		Policies:      len(policies),
		SignalRules:   signalRules,
		ExecutionMode: executionModeOf(ctrl.Gate()),
		Confinement:   cfg.Safety.DestructiveExecutionNodeSelector,
	})
	// Reset unconditionally: a marker that disappeared (or two markers that
	// disagree mid-sync) must clear the series, not leave the previous
	// digest advertised while the API reports none.
	metrics.RuntimeConfigInfo.Reset()
	if sourceDigest != "" {
		metrics.RuntimeConfigInfo.WithLabelValues(sourceDigest).Set(1)
	}
	return nil
}

// degradedTaintPolicy resolves the compiled degraded-node taint setting. An
// absent key is off, which is the whole default: a controller loading a
// configuration written before this feature existed must not start marking
// nodes. The effect is normalized by config.Validate before this is reached;
// InstallRuntimeConfig rejects anything else.
func degradedTaintPolicy(compiled *config.TaintDegradedNodes) controller.DegradedTaintPolicy {
	if compiled == nil || !compiled.Enabled {
		return controller.DegradedTaintPolicy{}
	}
	return controller.DegradedTaintPolicy{Enabled: true, Effect: compiled.Effect}
}

// watchRuntimeConfig re-applies the configuration whenever the mounted files
// change, until ctx is done. It runs on every replica, leader or not, so a
// standby that later wins the lease already holds current local configuration.
// In HA, node pauses are watched by a separate leader-context loop below: a
// process-wide reloader must never become a shared-state writer merely because
// it observed a transient leadership flag.
func watchRuntimeConfig(
	ctx context.Context,
	ctrl *controller.Controller,
	api *httpapi.Server,
	paths runtimeConfigPaths,
	log *slog.Logger,
	persistNodePauses bool,
) {
	last, _ := runtimeConfigDigest(paths)
	tick := time.NewTicker(runtimeConfigReloadInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		digest, err := runtimeConfigDigest(paths)
		if err != nil || digest == last {
			continue
		}
		if err := applyRuntimeConfigWithNodePauses(ctx, ctrl, api, paths, log, persistNodePauses); err != nil {
			// Keep the previous configuration and retry on the next tick; do
			// not advance the digest, so a transient read during the kubelet's
			// atomic symlink swap is retried rather than skipped.
			log.Warn("runtime configuration reload failed; keeping previous configuration", "err", err)
			continue
		}
		last = digest
		log.Info("runtime configuration reloaded in place", "digest", digest[:12])
	}
}

// watchNodeConfigPauses runs only in the leader callback's context. It owns
// the shared GPUNodeConfig pause set after the initial leader apply, while the
// process-wide runtime watcher above remains deliberately local on standbys.
func watchNodeConfigPauses(ctx context.Context, ctrl *controller.Controller, path string, log *slog.Logger) {
	if path == "" {
		return
	}
	last, _ := nodeConfigDigest(path)
	tick := time.NewTicker(runtimeConfigReloadInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		digest, err := nodeConfigDigest(path)
		if err != nil || digest == last {
			continue
		}
		configs, err := config.LoadNodeConfigs(path)
		if err != nil {
			log.Warn("node configuration reload failed; keeping previous pause set", "err", err)
			continue
		}
		if err := ctrl.ApplyNodeConfigs(ctx, configs); err != nil {
			// Do not advance the digest: a lost/cancelled leadership context exits
			// this loop, while a transient store failure is retried by this leader.
			if ctx.Err() == nil {
				log.Warn("applying node configuration reload failed; keeping previous pause set", "err", err)
			}
			continue
		}
		last = digest
		log.Info("leader node configuration reloaded in place", "digest", digest[:12])
	}
}

func nodeConfigDigest(path string) (string, error) {
	h := sha256.New()
	if err := hashFile(h, path); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runtimeConfigDigest hashes every configuration input so a change can be
// detected without parsing. The playbooks directory is walked so an added or
// removed playbook counts as a change.
func runtimeConfigDigest(paths runtimeConfigPaths) (string, error) {
	h := sha256.New()
	files := []string{paths.policies, paths.windows, paths.mappings, paths.nodeConfigs}
	// The operator's config-digest markers ride in BOTH mounted ConfigMaps
	// and must feed the change detector, not just the published identity:
	// the two mounts sync independently, so a change confined to the
	// playbooks ConfigMap would otherwise never trigger a reload and the
	// advertised identity would stay wrong permanently — the exact lie this
	// feature exists to prevent (round-12 review M2).
	files = append(files, configDigestPaths(paths)...)
	for _, f := range files {
		if f == "" {
			continue
		}
		if err := hashFile(h, f); err != nil {
			return "", err
		}
	}
	if paths.playbooks != "" {
		entries, err := os.ReadDir(paths.playbooks)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			// A ConfigMap directory mount contains the kubelet's own entries
			// (..data, ..2026_..._timestamp) alongside the real files. They are
			// directories and symlinks that begin with a dot; hashing them would
			// try to read a directory and fail, which previously made the whole
			// digest error out and the reloader skip forever.
			if strings.HasPrefix(e.Name(), ".") || e.IsDir() {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if err := hashFile(h, filepath.Join(paths.playbooks, name)); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// configDigestPaths names the operator-written digest markers beside each
// mounted configuration source. Both are optional: a file-based deployment
// with no operator has neither.
func configDigestPaths(paths runtimeConfigPaths) []string {
	var out []string
	if paths.policies != "" {
		out = append(out, filepath.Join(filepath.Dir(paths.policies), "config-digest"))
	}
	if paths.playbooks != "" {
		out = append(out, filepath.Join(paths.playbooks, "config-digest"))
	}
	return out
}

// readSourceDigest returns the operator-compiled snapshot digest the mounted
// configuration claims, or "" when no marker is present (file deployments,
// older operators). When both markers exist and DISAGREE, the two ConfigMaps
// are mid-sync: report "" rather than a digest that describes only half of
// what is loaded.
func readSourceDigest(paths runtimeConfigPaths) string {
	seen := ""
	for _, marker := range configDigestPaths(paths) {
		raw, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			continue
		}
		if seen != "" && seen != value {
			return ""
		}
		seen = value
	}
	return seen
}

func hashFile(h interface{ Write([]byte) (int, error) }, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A configured-but-absent optional file is a stable "empty", not an
			// error: it must not make the digest change on every poll.
			_, _ = h.Write([]byte("\x00missing:" + path))
			return nil
		}
		return err
	}
	_, _ = h.Write([]byte(path))
	_, _ = h.Write(data)
	return nil
}

// executionModeOf reports what the gate is doing, in the vocabulary the CRD
// uses, so an operator comparing the API answer to spec.safety.executionMode
// is comparing like with like. Read from the live gate rather than from the
// config that was just parsed: the point is to report what took EFFECT, not
// what was requested.
func executionModeOf(gate *safety.Gate) string {
	switch {
	case gate == nil:
		return "unknown"
	case gate.Paused():
		return "paused"
	case gate.DryRun():
		return "dry-run"
	default:
		return "enabled"
	}
}
