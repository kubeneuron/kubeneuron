// Package controller wires the kubeneuron-controller: signal ingestion,
// incident lifecycle, playbook execution through safety gates, approvals,
// and notifications. See docs/design.md for the full remediation flow.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/actuator"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/detect"
	"github.com/kubeneuron/kubeneuron/internal/metrics"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/platform"
	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// Controller is the control-plane brain.
type Controller struct {
	store    store.Store
	sink     store.EventSink
	gate     *safety.Gate
	flap     *safety.FlapDetector
	platform platform.Platform
	actuator actuator.Actuator
	notifier notify.Notifier
	log      *slog.Logger

	signals        chan types.Signal
	droppedSignals atomic.Uint64
	eventOutbox    store.EventOutbox
	eventWake      chan struct{}
	eventWorkerID  string

	// inFlight tracks incidents whose current step is executing in its own
	// goroutine, so the reconcile tick never double-drives them.
	inFlightMu sync.Mutex
	inFlight   map[string]bool

	// armingHoldSince records when each incident FIRST hit the
	// arming-in-flight hold (in scope, agent freshly unarmed). The
	// propagation grace is measured from this first observation — never from
	// StateChangedAt, which pre-ages during pauses, maintenance windows, and
	// playbook cooldowns that hold an incident in EVALUATING without
	// touching it, and would make the very first arming check "expire" a
	// grace that never ran. Entries are cleared on any non-hold arming
	// verdict and on every state transition, so the map cannot leak.
	armingHoldMu    sync.Mutex
	armingHoldSince map[string]time.Time

	// recoveredSlots holds the per-step (reboot-class) gate occupancy seeded on
	// leadership acquisition from durable EXECUTING incidents (see
	// RebuildGateOccupancy). Each entry is released exactly once — the first
	// time its incident leaves EXECUTING — so the reserved step slot is handed
	// back as recovery re-admits or closes it. The incident's REMEDIATION slot
	// has no in-memory tracking at all: ownership is the durable
	// Incident.RemediationSlotHeld bit, written atomically with the EXECUTING
	// and halting transitions.
	recoveredMu    sync.Mutex
	recoveredSlots map[string]recoveredSlot

	// reconcileEvery paces the walk; set before Run only, never reloaded.
	reconcileEvery time.Duration

	// runtime is the current immutable RuntimeConfig snapshot (never nil);
	// runtimeMu serializes read-modify-write installs (the reloader vs the
	// per-field test hooks). See runtimeconfig.go.
	runtime   atomic.Pointer[RuntimeConfig]
	runtimeMu sync.Mutex

	// pinnedEvidence holds the accelerator evidence captured when an incident
	// quiesced the vendor stack, keyed by incident ID. See pinAcceleratorEvidence.
	pinnedEvidenceMu sync.Mutex
	pinnedEvidence   map[string]pinnedAcceleratorEvidence

	// cordonReported remembers which stuck cordons have been announced.
	cordonReported cordonReportedKeys
}

// SetSignalCatalog installs the declarative signal-override catalog.
func (c *Controller) SetSignalCatalog(catalog *detect.Catalog) {
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Catalog = catalog })
}

// ApplyNodeConfigs persists per-node desired state: the listed paused nodes
// become the complete pause set (the walk consults it before every step).
func (c *Controller) ApplyNodeConfigs(ctx context.Context, configs []config.NodeConfig) error {
	var paused []string
	for _, n := range configs {
		if n.Paused {
			paused = append(paused, n.NodeName)
		}
	}
	if err := c.store.ApplyNodeConfigPauses(ctx, paused); err != nil {
		return err
	}
	if len(paused) > 0 {
		c.log.Warn("nodes paused by GPUNodeConfig", "nodes", paused)
	}
	return nil
}

// SetMaintenanceWindows installs the maintenance windows the walk consults;
// execution holds while a window covering the target node is active.
func (c *Controller) SetMaintenanceWindows(windows []config.MaintenanceWindow) {
	windows = append([]config.MaintenanceWindow(nil), windows...)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Windows = windows })
}

// SetEngine swaps the playbook engine in place. Config changes reload the
// engine here instead of rolling the controller Deployment: under leader
// election a rolling update deadlocks (only the leader is Ready, so a new pod
// can never become Ready to retire the old one), which left config changes
// unable to take effect at all.
func (c *Controller) SetEngine(engine *playbook.Engine) {
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.Engine = engine })
}

// currentEngine returns the LIVE engine (unpinned). Reconcile-path reads use
// c.runtimeConfig(ctx).Engine so they observe the snapshot pinned for their
// advance; this accessor remains for callers with no call-tree pin.
func (c *Controller) currentEngine() *playbook.Engine {
	return c.runtime.Load().Engine
}

// SetQuiesceForbiddenHolders installs the process names whose presence on a node
// forbids a per-device reset there. See the CRD field of the same name: some
// processes hold the GPU and must not be stopped, and declaring them turns a
// reset into an early refusal rather than a cordoned node and a late failure.
func (c *Controller) SetQuiesceForbiddenHolders(names []string) {
	names = append([]string(nil), names...)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.QuiesceForbidden = names })
}

func (c *Controller) forbiddenHolders(ctx context.Context) []string {
	return c.runtimeConfig(ctx).QuiesceForbidden
}

// SetDestructiveNodeSelector installs the compiled
// spec.safety.destructiveExecution.nodeSelector — the declared blast radius.
// On an Enabled install the controller refuses to run a destructive platform
// step (cordon, drain, evict, recycle/replace) against a node whose labels do
// not match it, so the confinement the agent DaemonSet already has covers the
// controller path too. An empty selector disables the check (every other
// execution mode is globally dry-run, so nothing executes anyway).
func (c *Controller) SetDestructiveNodeSelector(selector map[string]string) {
	var copied map[string]string
	if len(selector) > 0 {
		copied = make(map[string]string, len(selector))
		for k, v := range selector {
			copied[k] = v
		}
	}
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.DestructiveSelector = copied })
}

func (c *Controller) destructiveNodeSelector(ctx context.Context) map[string]string {
	return c.runtimeConfig(ctx).DestructiveSelector
}

// SetAcceleratorRuntimeProfiles installs the server-owned runtime contracts
// consulted before a physical NVIDIA reset. These profiles only narrow what a
// ready agent may do; they never change the global execution mode. An empty
// set is intentional default-deny for accelerator remediation.
func (c *Controller) SetAcceleratorRuntimeProfiles(profiles []config.AcceleratorRuntimeProfile) error {
	if err := ValidateAcceleratorRuntimeProfiles(profiles); err != nil {
		return err
	}
	profiles = append([]config.AcceleratorRuntimeProfile(nil), profiles...)
	c.mutateRuntimeConfig(func(rc *RuntimeConfig) { rc.AcceleratorProfiles = profiles })
	return nil
}

// ValidateAcceleratorRuntimeProfiles performs the non-mutating half of profile
// installation. Runtime reload validates before its only fallible store write,
// so a failed reload cannot expose any part of a new configuration.
func ValidateAcceleratorRuntimeProfiles(profiles []config.AcceleratorRuntimeProfile) error {
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if err := profile.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[profile.Name]; duplicate {
			return fmt.Errorf("duplicate accelerator runtime profile %q", profile.Name)
		}
		seen[profile.Name] = struct{}{}
	}
	return nil
}

// criticalEnqueueWait bounds how long a critical signal may block its
// ingestion goroutine waiting for queue space before it is counted dropped.
const criticalEnqueueWait = 2 * time.Second

// actionLeaseDuration is the minimum lease for actions without an explicit
// timeout. The store extends a claim to cover any declared action timeout;
// lease renewal is a later protocol extension.
const actionLeaseDuration = time.Hour

// eventOutboxLeaseDuration bounds recovery after a controller dies while an
// event is being classified. Raw events are already durable at this point;
// the lease merely serializes their asynchronous hand-off to the signal
// queue.
const eventOutboxLeaseDuration = 30 * time.Second

const eventOutboxDrainLimit = 64

// New assembles a controller.
func New(
	st store.Store,
	sink store.EventSink,
	engine *playbook.Engine,
	gate *safety.Gate,
	flap *safety.FlapDetector,
	plat platform.Platform,
	act actuator.Actuator,
	notifier notify.Notifier,
	log *slog.Logger,
) *Controller {
	c := &Controller{
		store:           st,
		sink:            sink,
		gate:            gate,
		flap:            flap,
		platform:        plat,
		actuator:        act,
		notifier:        notifier,
		log:             log,
		signals:         make(chan types.Signal, 256),
		eventWake:       make(chan struct{}, 1),
		eventWorkerID:   fmt.Sprintf("controller-%d", time.Now().UnixNano()),
		inFlight:        map[string]bool{},
		armingHoldSince: map[string]time.Time{},
		recoveredSlots:  map[string]recoveredSlot{},
		reconcileEvery:  defaultReconcileInterval,
	}
	c.runtime.Store(&RuntimeConfig{
		Engine:      engine,
		VerifyQuiet: defaultVerifyQuietWindow,
		ApprovalTTL: defaultApprovalTTL,
	})
	if outbox, ok := sink.(store.EventOutbox); ok {
		c.eventOutbox = outbox
	}
	return c
}

// SetReconcileInterval overrides the reconcile tick (tests, dev).
func (c *Controller) SetReconcileInterval(d time.Duration) {
	if d > 0 {
		c.reconcileEvery = d
	}
}

// Run starts the reconcile loop and blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) error {
	tick := time.NewTicker(c.reconcileEvery)
	defer tick.Stop()
	outboxTick := time.NewTicker(eventOutboxLeaseDuration)
	defer outboxTick.Stop()
	// Before the first reconcile, rebuild the safety gate's occupancy from the
	// durable EXECUTING incidents so the concurrency caps hold across a leader
	// failover (see RebuildGateOccupancy). Best-effort: a store error here only
	// degrades to the pre-fix transient undercount, never to a blocked walk.
	if err := c.RebuildGateOccupancy(ctx); err != nil {
		c.log.Warn("rebuilding safety gate occupancy failed; concurrency caps may transiently undercount until recovery re-admits each incident", "err", err)
	}
	// Drain once before waiting for new input so a restart resumes events that
	// were archived before the previous controller process died.
	c.drainEventOutbox(ctx)
	// Janitors run on their own goroutine, NOT the walk thread. They are
	// idempotent reconcilers that talk only to the store and the platform, and
	// they can legitimately block on slow externals (an unresponsive agent's
	// bounded restore wait, apiserver calls) — which previously stretched every
	// reconcile tick and delayed signal draining exactly when a fleet event
	// made the janitors busiest. Walk-vs-janitor writes to the same incident
	// are already safe: janitors skip inFlight incidents and every store write
	// is guarded by optimistic versioning, so a racing update degrades to one
	// side's ErrConflict and a retry next tick.
	go c.runJanitors(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-c.signals:
			if err := c.ingest(ctx, sig); err != nil {
				c.log.Error("signal ingestion failed", "err", err, "class", sig.Class, "node", sig.Target.Node)
			}
		case <-tick.C:
			c.reconcile(ctx)
		case <-c.eventWake:
			c.drainEventOutbox(ctx)
		case <-outboxTick.C:
			c.drainEventOutbox(ctx)
		}
	}
}

// runJanitors drives the three background reconcilers until ctx ends:
// monitoring that a playbook switched off must come back even when the
// playbook never reaches its restore step, a node a playbook cordoned must
// not stay out of service because the playbook died before uncordoning it,
// and incidents on deleted nodes must be closed as replaced.
func (c *Controller) runJanitors(ctx context.Context) {
	tick := time.NewTicker(c.reconcileEvery)
	defer tick.Stop()
	for {
		// One pinned configuration snapshot per janitor pass, exactly like
		// the walk pins one per advance: a pass must never observe mixed
		// generations across its loop.
		passCtx := c.pinRuntimeConfig(ctx)
		c.restoreAbandonedAcceleratorStacks(passCtx)
		c.reconcileCordonedNodes(passCtx)
		c.resolveIncidentsOnVanishedNodes(passCtx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// --- httpapi.Backend implementation ---

// HandleSignal implements httpapi.Backend. When the queue is full, a
// critical signal briefly blocks its ingestion goroutine for a chance to
// enqueue — silently losing e.g. an XID 79 during a signal storm is worse
// than bounded backpressure. Every loss is counted and logged with the
// running total so storms are visible, never silent.
func (c *Controller) HandleSignal(r *http.Request, sig types.Signal) {
	c.enqueueSignal(r, sig)
}

// enqueueSignal adds a directly received signal to the bounded in-memory
// queue. It returns false when it could not be accepted; HTTP ingestion uses
// the return only for logging/metrics, while the durable outbox keeps the raw
// event leased for retry instead of treating that case as a loss.
func (c *Controller) enqueueSignal(r *http.Request, sig types.Signal) bool {
	select {
	case c.signals <- sig:
		return true
	default:
	}
	if sig.Severity == types.SeverityCritical {
		done := context.Background().Done()
		if r != nil {
			done = r.Context().Done()
		}
		t := time.NewTimer(criticalEnqueueWait)
		defer t.Stop()
		select {
		case c.signals <- sig:
			return true
		case <-done:
		case <-t.C:
		}
	}
	metrics.SignalsDropped.Inc()
	n := c.droppedSignals.Add(1)
	c.log.Error("signal queue full, dropping",
		"class", sig.Class, "node", sig.Target.Node, "severity", sig.Severity, "dropped_total", n)
	return false
}

// DroppedSignals reports how many signals were lost to queue overflow since
// start; the raw agent events behind them remain archived in the event sink.
func (c *Controller) DroppedSignals() uint64 { return c.droppedSignals.Load() }

// HandleAgentEvent implements httpapi.Backend: archive the raw event before
// acknowledging it, then classify and ingest it as a signal when actionable.
// If the archive is unavailable the caller receives a non-2xx response, so
// the agent keeps the event in its fsynced spool for retry. Replay duplicates
// (same EventID already archived) are dropped here so at-least-once delivery
// cannot double-count signals.
func (c *Controller) HandleAgentEvent(r *http.Request, ev types.AgentEvent) error {
	if c.sink == nil {
		return errors.New("event sink is not configured")
	}
	fresh, err := c.sink.WriteEvent(r.Context(), &ev)
	if err != nil {
		c.log.Error("event archival failed", "err", err)
		return fmt.Errorf("archive agent event: %w", err)
	}
	if !fresh {
		metrics.DuplicateEvents.Inc()
		c.log.Debug("duplicate agent event ignored", "event_id", ev.EventID, "node", ev.Node, "xid", ev.XID)
		return nil
	}
	if c.eventOutbox != nil {
		// The raw event and its workflow item are one transaction. A 2xx here
		// is therefore safe even if this controller exits before classification:
		// Run resumes the durable item on startup.
		select {
		case c.eventWake <- struct{}{}:
		default:
		}
		return nil
	}
	if sig, ok := c.runtimeConfig(r.Context()).Catalog.SignalFromAgentEvent(ev); ok {
		c.HandleSignal(r, sig)
	}
	return nil
}

// drainEventOutbox commits durable event processing and its incident/audit
// mutation together. If the controller exits, either both are committed or
// the lease expires for a clean retry; no transient signal queue lies between
// the durable boundaries.
func (c *Controller) drainEventOutbox(ctx context.Context) {
	if c.eventOutbox == nil {
		return
	}
	for range eventOutboxDrainLimit {
		claimed, err := c.eventOutbox.ClaimNextEvent(ctx, c.eventWorkerID, eventOutboxLeaseDuration)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			c.log.Error("event outbox claim failed", "err", err)
			return
		}
		if sig, ok := c.runtimeConfig(ctx).Catalog.SignalFromAgentEvent(claimed.Event); ok {
			if err := c.ingestClaimedEvent(ctx, claimed, sig); err != nil {
				if !errors.Is(err, store.ErrEventLeaseLost) {
					c.log.Error("event outbox workflow failed", "err", err, "event_id", claimed.Event.EventID)
				}
				return
			}
			continue
		}
		// A non-actionable raw event must still be acknowledged durably; the
		// empty callback means only the outbox completion is committed.
		if err := c.eventOutbox.ProcessClaimedEvent(ctx, claimed.OutboxID, claimed.LeaseToken, func(store.Tx) error { return nil }); err != nil {
			if !errors.Is(err, store.ErrEventLeaseLost) {
				c.log.Error("event outbox completion failed", "err", err, "event_id", claimed.Event.EventID)
			}
			return
		}
	}
}

// RegisterNode implements httpapi.Backend. The controller, rather than the
// agent, is authoritative for the persisted heartbeat timestamp.
func (c *Controller) RegisterNode(r *http.Request, registration types.AgentRegistration) (types.AgentArming, error) {
	n := &types.Node{
		Name:          registration.Name,
		UID:           registration.NodeUID,
		GPUs:          registration.GPUs,
		BootID:        registration.BootID,
		AgentLastSeen: time.Now(),
	}
	// The tri-state mapping is deliberate: absent (a v1 registration, or an
	// old agent) is UNKNOWN, never a declared value in either direction.
	if registration.DestructiveArmed != nil {
		if *registration.DestructiveArmed {
			n.AgentArming = types.AgentArmingArmed
		} else {
			n.AgentArming = types.AgentArmingUnarmed
		}
	}
	if err := c.store.UpsertAgentRegistration(r.Context(), n); err != nil {
		c.log.Error("node registration failed", "err", err, "node", n.Name)
		return types.AgentArmingUnknown, err
	}
	return c.servedArming(r.Context(), registration.Name), nil
}

// servedArming answers whether one node's agent should arm its destructive
// executor: the compiled spec.safety.destructiveExecution.nodeSelector
// matched against the node's labels — the same computation the controller
// path's blast-radius confinement uses, so the two boundaries can never
// disagree about which nodes are in scope. Fail-closed on every uncertainty:
// no selector (not an Enabled install), unresolvable labels, or a non-match
// all answer unarmed; the next registration tick recovers a transient blip.
// A stale-armed window is inert by construction — destructive actions are
// dispatched by this same controller, which re-confines them at admission.
func (c *Controller) servedArming(ctx context.Context, node string) types.AgentArming {
	selector := c.runtimeConfig(ctx).DestructiveSelector
	if len(selector) == 0 {
		return types.AgentArmingUnarmed
	}
	labels, err := c.nodeLabelsForConfinement(ctx, node)
	if err != nil {
		return types.AgentArmingUnarmed
	}
	if labelsMatchSelector(selector, labels) {
		return types.AgentArmingArmed
	}
	return types.AgentArmingUnarmed
}

// HandleAcceleratorReport implements httpapi.AcceleratorReportBackend. The
// report is an authenticated, agent-owned observation; accepting it does not
// select a playbook, change an incident, or grant remediation permission. A
// controller without the durable report capability fails closed rather than
// acknowledging data it cannot retain for a later capability gate.
func (c *Controller) HandleAcceleratorReport(r *http.Request, report types.AgentAcceleratorReport) error {
	reports, ok := c.store.(store.AcceleratorReportStore)
	if !ok {
		return errors.New("accelerator report store is not configured")
	}
	if err := reports.UpsertAcceleratorReport(r.Context(), &report); err != nil {
		if !errors.Is(err, store.ErrStaleAcceleratorReport) {
			c.log.Error("accelerator report persistence failed", "err", err, "node", report.Node, "vendor", report.Vendor)
		}
		return err
	}
	c.log.Debug("accelerator report persisted", "node", report.Node, "vendor", report.Vendor,
		"readiness", report.Readiness, "observed_at", report.ObservedAt)
	return nil
}

// NextAction implements httpapi.Backend: atomically claim the node's oldest
// available queued action. The opaque claim token is returned only to the
// authenticated node and must accompany its result.
func (c *Controller) NextAction(r *http.Request, node string) (*types.QueuedAction, error) {
	bootID := r.Header.Get(types.AgentBootIDHeader)
	queued, err := c.store.ClaimNextAction(r.Context(), node, bootID, actionLeaseDuration)
	if err == store.ErrNotFound {
		return nil, nil
	}
	return queued, err
}

// CompleteAction implements httpapi.Backend. The action must belong to the
// authenticated node; one node can never complete another node's work.
func (c *Controller) CompleteAction(r *http.Request, node, actionID, leaseToken string, res types.ActionResult) error {
	queued, err := c.store.GetAction(r.Context(), actionID)
	if err != nil {
		return err
	}
	if queued.Node != node {
		return fmt.Errorf("action %s belongs to node %s, not %s: %w", actionID, queued.Node, node, store.ErrActionForeignNode)
	}
	return c.store.CompleteClaimedAction(r.Context(), actionID, leaseToken, r.Header.Get(types.AgentBootIDHeader), res)
}

// --- incident lifecycle ---

// ingest attaches a signal to an existing open incident or opens a new one.
// Its incident and audit mutations share a transaction even for direct signal
// sources, matching the durable outbox path below.
func (c *Controller) ingest(ctx context.Context, sig types.Signal) error {
	var opened *types.Incident
	if err := c.store.WithTx(ctx, func(tx store.Tx) error {
		var err error
		opened, err = c.ingestTx(ctx, tx, sig)
		return err
	}); err != nil {
		return err
	}
	return c.afterSignalCommitted(ctx, sig, opened)
}

// ingestClaimedEvent processes an outbox item in the same transaction as its
// incident/audit mutation. A crash can therefore leave either both changes
// committed or neither; a reclaimed item can never increase SignalSeen twice.
func (c *Controller) ingestClaimedEvent(ctx context.Context, claimed *store.ClaimedEvent, sig types.Signal) error {
	var opened *types.Incident
	if err := c.eventOutbox.ProcessClaimedEvent(ctx, claimed.OutboxID, claimed.LeaseToken, func(tx store.Tx) error {
		var err error
		opened, err = c.ingestTx(ctx, tx, sig)
		return err
	}); err != nil {
		return err
	}
	return c.afterSignalCommitted(ctx, sig, opened)
}

// ingestTx performs the durable incident mutation for one signal. It does not
// notify or update metrics: those effects happen only after the transaction
// commits, so an outbox retry has no duplicated incident accounting.
func (c *Controller) ingestTx(ctx context.Context, tx store.Tx, sig types.Signal) (*types.Incident, error) {
	inc, err := tx.GetOpenIncident(ctx, sig.Target, sig.Class)
	switch {
	case err == store.ErrNotFound:
		return c.openIncidentTx(ctx, tx, sig)
	case err != nil:
		return nil, err
	}
	inc.SignalSeen++
	inc.UpdatedAt = time.Now()
	if err := tx.UpdateIncident(ctx, inc); err != nil {
		return nil, err
	}
	return nil, nil
}

func (c *Controller) openIncidentTx(ctx context.Context, tx store.Tx, sig types.Signal) (*types.Incident, error) {
	now := time.Now()
	dryRun := true
	if c.gate != nil {
		dryRun = c.gate.DryRun()
	}
	inc := &types.Incident{
		ID:             incidentID(sig, now),
		Target:         sig.Target,
		Class:          sig.Class,
		State:          types.StateOpen,
		DryRun:         dryRun,
		SignalSeen:     1,
		OpenedAt:       now,
		UpdatedAt:      now,
		StateChangedAt: now,
	}
	if engine := c.runtimeConfig(ctx).Engine; engine != nil {
		if book, ok := engine.Select(sig); ok {
			inc.Playbook = book.Name
		}
	}
	if err := tx.CreateIncident(ctx, inc); err != nil {
		return nil, err
	}
	if err := tx.AppendAudit(ctx, &types.AuditEntry{
		IncidentID: inc.ID, Time: now,
		FromState: inc.State, ToState: inc.State,
		Actor: "system", Action: "open",
		Params: sig.Evidence, DryRun: inc.DryRun,
	}); err != nil {
		return nil, err
	}
	return inc, nil
}

func (c *Controller) afterSignalCommitted(ctx context.Context, sig types.Signal, opened *types.Incident) error {
	metrics.SignalsTotal.WithLabelValues(string(sig.Source), string(sig.Class)).Inc()
	if opened == nil {
		return nil
	}
	metrics.IncidentsOpened.WithLabelValues(string(opened.Class)).Inc()
	if c.notifier == nil {
		return nil
	}
	return c.notifier.Notify(ctx, notify.NotifyEvent{
		Kind: notify.EventOpened, Incident: opened,
		Message: fmt.Sprintf("incident opened: %s on %s (playbook: %s)", opened.Class, opened.Target.Node, orNone(opened.Playbook)),
	})
}

// incidentID is deterministic enough for uniqueness and readable in Slack:
// <class>-<node>-<hash8>.
func incidentID(sig types.Signal, t time.Time) string {
	h := sha256.Sum256([]byte(sig.Target.Node + sig.Target.GPUUUID + string(sig.Class) + t.Format(time.RFC3339Nano)))
	return fmt.Sprintf("%s-%s-%s", sig.Class, sig.Target.Node, hex.EncodeToString(h[:4]))
}

func orNone(s string) string {
	if s == "" {
		return "none (observe only)"
	}
	return s
}
