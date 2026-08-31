package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// StateStore persists small safety-state snapshots so cooldowns and flap
// history survive a controller restart. Persistence is write-through and
// best-effort: the in-memory state stays authoritative, and a failed write
// is logged rather than blocking the gate — losing a snapshot degrades to
// the old restart-amnesia behavior, never to a blocked remediation path.
type StateStore interface {
	SaveSafetyState(ctx context.Context, kind string, payload []byte) error
	LoadSafetyState(ctx context.Context, kind string) ([]byte, error)
}

// statePersistenceTimeout bounds I/O while the gate mutex is held. A stalled
// database must not turn the pause endpoint or a reconcile admission check
// into an unbounded process-wide lock.
const statePersistenceTimeout = 5 * time.Second

func boundedStateContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, statePersistenceTimeout)
}

const (
	stateKindCooldowns = "cooldowns"
	stateKindFlap      = "flap"
	stateKindPause     = "pause"
)

// pauseSnapshot is deliberately separate from cooldowns: a pause is a
// durable operator intent, not a cache that may safely age out.  Actor and
// ChangedAt are retained so the restored state has an accountable explanation.
type pauseSnapshot struct {
	Paused    bool      `json:"paused"`
	Actor     string    `json:"actor,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitempty"`
}

// RestoreAndPersist loads previously stored cooldowns into the gate and
// enables write-through persistence for future changes. Expired cooldowns
// are dropped during restore.
func (g *Gate) RestoreAndPersist(ctx context.Context, store StateStore, log *slog.Logger) error {
	persistCtx, cancel := boundedStateContext(ctx)
	defer cancel()
	payload, err := store.LoadSafetyState(persistCtx, stateKindCooldowns)
	if err != nil {
		return fmt.Errorf("load persisted cooldowns: %w", err)
	}
	pausePayload, err := store.LoadSafetyState(persistCtx, stateKindPause)
	if err != nil {
		return fmt.Errorf("load persisted pause: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if payload != nil {
		restored := map[string]time.Time{}
		if err := json.Unmarshal(payload, &restored); err != nil {
			return fmt.Errorf("decode persisted cooldowns: %w", err)
		}
		now := g.now()
		for key, until := range restored {
			if now.Before(until) {
				g.cooldownUntil[key] = until
			}
		}
	}
	if pausePayload != nil {
		var pause pauseSnapshot
		if err := json.Unmarshal(pausePayload, &pause); err != nil {
			return fmt.Errorf("decode persisted pause: %w", err)
		}
		g.paused, g.pauseActor, g.pauseChangedAt = pause.Paused, pause.Actor, pause.ChangedAt
	}
	g.store, g.storeLog = store, log
	g.persistLocked(persistCtx)
	if err := g.persistPauseLocked(persistCtx, g.paused, g.pauseActor, g.pauseChangedAt); err != nil && g.storeLog != nil {
		g.storeLog.Warn("persisting restored global pause failed; state survives in memory only", "err", err)
	}
	return nil
}

// persistLocked snapshots the cooldown map. Called with g.mu held.
func (g *Gate) persistLocked(ctx context.Context) {
	if g.store == nil {
		return
	}
	payload, err := json.Marshal(g.cooldownUntil)
	if err == nil {
		err = g.store.SaveSafetyState(ctx, stateKindCooldowns, payload)
	}
	if err != nil && g.storeLog != nil {
		g.storeLog.Warn("persisting gate cooldowns failed; state survives in memory only", "err", err)
	}
}

// persistPauseLocked writes an explicit pause replacement. Called with g.mu
// held. Unlike cooldown persistence, its error reaches SetPaused so the API
// never acknowledges a pause that cannot survive failover.
func (g *Gate) persistPauseLocked(ctx context.Context, paused bool, actor string, changedAt time.Time) error {
	if g.store == nil {
		return fmt.Errorf("global pause persistence is unavailable")
	}
	payload, err := json.Marshal(pauseSnapshot{Paused: paused, Actor: actor, ChangedAt: changedAt})
	if err != nil {
		return err
	}
	return g.store.SaveSafetyState(ctx, stateKindPause, payload)
}

// flapSnapshot is the persisted form of FlapDetector state.
type flapSnapshot struct {
	Reopens        map[string][]time.Time `json:"reopens"`
	PendingResolve map[string]time.Time   `json:"pending_resolve"`
}

// RestoreAndPersist loads previously stored flap history into the detector
// and enables write-through persistence. Entries outside the window are
// dropped on the next Record call via the normal GC.
func (f *FlapDetector) RestoreAndPersist(ctx context.Context, store StateStore, log *slog.Logger) error {
	persistCtx, cancel := boundedStateContext(ctx)
	defer cancel()
	payload, err := store.LoadSafetyState(persistCtx, stateKindFlap)
	if err != nil {
		return fmt.Errorf("load persisted flap state: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if payload != nil {
		var snap flapSnapshot
		if err := json.Unmarshal(payload, &snap); err != nil {
			return fmt.Errorf("decode persisted flap state: %w", err)
		}
		if snap.Reopens != nil {
			f.reopens = snap.Reopens
		}
		if snap.PendingResolve != nil {
			f.pendingResolve = snap.PendingResolve
		}
		f.gcLocked()
	}
	f.store, f.storeLog = store, log
	f.persistLocked(persistCtx)
	return nil
}

// persistLocked snapshots the flap state. Called with f.mu held.
func (f *FlapDetector) persistLocked(ctx context.Context) {
	if f.store == nil {
		return
	}
	payload, err := json.Marshal(flapSnapshot{Reopens: f.reopens, PendingResolve: f.pendingResolve})
	if err == nil {
		err = f.store.SaveSafetyState(ctx, stateKindFlap, payload)
	}
	if err != nil && f.storeLog != nil {
		f.storeLog.Warn("persisting flap history failed; state survives in memory only", "err", err)
	}
}
