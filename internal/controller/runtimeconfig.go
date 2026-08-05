package controller

// This file is the runtime-configuration seam: one immutable snapshot of
// everything the reload path may change while incidents are live, installed
// atomically. Before it, eight independently-locked fields were applied by
// eight sequential setters — a reconcile pass could observe a new engine
// beside an old destructive selector (mixed generations), and two of the
// fields (the timings) were guarded by nothing at all.

import (
	"context"
	"fmt"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
)

// RuntimeConfig is one immutable, coherent view of the reloadable runtime
// configuration. Install a whole snapshot with InstallRuntimeConfig (or, for
// tests, the per-field Set* wrappers, which copy-on-write a fresh snapshot);
// nothing may ever mutate an installed snapshot. Node pause state is
// deliberately NOT here — it is store-persisted, transactional, and must
// survive restarts.
type RuntimeConfig struct {
	// Engine selects playbooks and yields next steps; never nil after New.
	Engine *playbook.Engine
	// Catalog classifies signals; nil means built-ins only (every Catalog
	// method is nil-receiver-safe).
	Catalog *detect.Catalog
	// Windows are the maintenance windows the walk holds execution under.
	Windows []config.MaintenanceWindow
	// AcceleratorProfiles are the server-owned runtime contracts consulted
	// before a physical reset. Empty is deliberate default-deny.
	AcceleratorProfiles []config.AcceleratorRuntimeProfile
	// QuiesceForbidden are process names whose presence forbids a per-device
	// reset on a node.
	QuiesceForbidden []string
	// DestructiveSelector is the compiled destructiveExecution.nodeSelector —
	// the declared blast radius; empty disables the confinement check.
	DestructiveSelector map[string]string
	// DegradedTaint is the compiled spec.safety.taintDegradedNodes. The zero
	// value is disabled, which is what every configuration written before the
	// field existed decodes to — the feature can only arrive by being asked
	// for.
	DegradedTaint DegradedTaintPolicy
	// SourceDigest identifies the operator-compiled snapshot these settings
	// came from (the config-digest key the operator writes into the mounted
	// ConfigMaps). Empty for file-based deployments with no operator. It is
	// identity only — nothing branches on it; it exists so /readyz, the
	// operator API, and the info metric can say WHICH configuration is live.
	SourceDigest string
	// VerifyQuiet and ApprovalTTL are always > 0 (defaulted at seed;
	// InstallRuntimeConfig treats zero as keep-current, matching the
	// SetTimings contract).
	VerifyQuiet time.Duration
	ApprovalTTL time.Duration
}

// DegradedTaintPolicy is the resolved degraded-node taint setting. Effect is
// meaningless while Enabled is false, and Enabled is false in the zero value:
// a snapshot installed by any path that has not heard of this feature leaves
// node scheduling alone.
type DegradedTaintPolicy struct {
	Enabled bool
	Effect  string
}

// pinnedRuntimeConfigKey carries one snapshot through a call tree, so every
// read within one advance (and the step goroutine it spawns, which captures
// the same ctx) observes the SAME configuration generation.
type pinnedRuntimeConfigKey struct{}

// pinRuntimeConfig pins the current snapshot into ctx. advance() pins once
// per incident pass; whole-pass or process-wide pinning would only delay
// config visibility without adding coherence (each advance is independent).
func (c *Controller) pinRuntimeConfig(ctx context.Context) context.Context {
	return context.WithValue(ctx, pinnedRuntimeConfigKey{}, c.runtime.Load())
}

// runtimeConfig returns the snapshot pinned in ctx when one is present —
// keeping every read within a pinned call tree on one generation — or the
// live snapshot otherwise. Never nil.
func (c *Controller) runtimeConfig(ctx context.Context) *RuntimeConfig {
	if rc, ok := ctx.Value(pinnedRuntimeConfigKey{}).(*RuntimeConfig); ok {
		return rc
	}
	return c.runtime.Load()
}

// InstallRuntimeConfig atomically replaces the whole runtime configuration.
// This is the reload path's single install point: a reconcile pass observes
// either the old snapshot or the new one, never a mixture. The snapshot is
// defensively copied on the way in; zero timings keep their current values.
func (c *Controller) InstallRuntimeConfig(rc RuntimeConfig) error {
	if err := ValidateAcceleratorRuntimeProfiles(rc.AcceleratorProfiles); err != nil {
		return err
	}
	// Reject an unrecognised effect rather than repairing it: the reload path
	// keeps the previous configuration in force on an error, and a node
	// scheduling hint that silently becomes something other than what was
	// asked for is worse than a configuration that visibly refuses to load.
	if rc.DegradedTaint.Enabled {
		switch rc.DegradedTaint.Effect {
		case "":
			rc.DegradedTaint.Effect = config.TaintEffectPreferNoSchedule
		case config.TaintEffectPreferNoSchedule, config.TaintEffectNoSchedule:
		default:
			return fmt.Errorf("degraded-node taint effect %q is not %s or %s",
				rc.DegradedTaint.Effect, config.TaintEffectPreferNoSchedule, config.TaintEffectNoSchedule)
		}
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	cur := c.runtime.Load()
	if rc.VerifyQuiet <= 0 {
		rc.VerifyQuiet = cur.VerifyQuiet
	}
	if rc.ApprovalTTL <= 0 {
		rc.ApprovalTTL = cur.ApprovalTTL
	}
	c.runtime.Store(copyRuntimeConfig(&rc))
	return nil
}

// mutateRuntimeConfig is the copy-on-write core behind the per-field Set*
// hooks: it clones the current snapshot, applies one change, and installs the
// clone. runtimeMu serializes read-modify-write installs so a test hook and
// the reloader cannot lose each other's update.
func (c *Controller) mutateRuntimeConfig(mutate func(rc *RuntimeConfig)) {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	rc := *c.runtime.Load()
	mutate(&rc)
	c.runtime.Store(&rc)
}

// copyRuntimeConfig deep-copies the snapshot's owned collections so no caller
// retains a mutable alias into an installed snapshot.
func copyRuntimeConfig(rc *RuntimeConfig) *RuntimeConfig {
	out := *rc
	out.Windows = append([]config.MaintenanceWindow(nil), rc.Windows...)
	out.AcceleratorProfiles = append([]config.AcceleratorRuntimeProfile(nil), rc.AcceleratorProfiles...)
	out.QuiesceForbidden = append([]string(nil), rc.QuiesceForbidden...)
	if len(rc.DestructiveSelector) > 0 {
		out.DestructiveSelector = make(map[string]string, len(rc.DestructiveSelector))
		for k, v := range rc.DestructiveSelector {
			out.DestructiveSelector[k] = v
		}
	} else {
		out.DestructiveSelector = nil
	}
	return &out
}
