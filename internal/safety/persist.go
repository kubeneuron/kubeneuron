package safety

import (
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
	SaveSafetyState(kind string, payload []byte) error
	LoadSafetyState(kind string) ([]byte, error)
}

const (
	stateKindCooldowns = "cooldowns"
	stateKindFlap      = "flap"
)

// RestoreAndPersist loads previously stored cooldowns into the gate and
// enables write-through persistence for future changes. Expired cooldowns
// are dropped during restore.
func (g *Gate) RestoreAndPersist(store StateStore, log *slog.Logger) error {
	payload, err := store.LoadSafetyState(stateKindCooldowns)
	if err != nil {
		return fmt.Errorf("load persisted cooldowns: %w", err)
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
	g.store, g.storeLog = store, log
	g.persistLocked()
	return nil
}

// persistLocked snapshots the cooldown map. Called with g.mu held.
func (g *Gate) persistLocked() {
	if g.store == nil {
		return
	}
	payload, err := json.Marshal(g.cooldownUntil)
	if err == nil {
		err = g.store.SaveSafetyState(stateKindCooldowns, payload)
	}
	if err != nil && g.storeLog != nil {
		g.storeLog.Warn("persisting gate cooldowns failed; state survives in memory only", "err", err)
	}
}

// flapSnapshot is the persisted form of FlapDetector state.
type flapSnapshot struct {
	Reopens        map[string][]time.Time `json:"reopens"`
	PendingResolve map[string]time.Time   `json:"pending_resolve"`
}

// RestoreAndPersist loads previously stored flap history into the detector
// and enables write-through persistence. Entries outside the window are
// dropped on the next Record call via the normal GC.
func (f *FlapDetector) RestoreAndPersist(store StateStore, log *slog.Logger) error {
	payload, err := store.LoadSafetyState(stateKindFlap)
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
	f.persistLocked()
	return nil
}

// persistLocked snapshots the flap state. Called with f.mu held.
func (f *FlapDetector) persistLocked() {
	if f.store == nil {
		return
	}
	payload, err := json.Marshal(flapSnapshot{Reopens: f.reopens, PendingResolve: f.pendingResolve})
	if err == nil {
		err = f.store.SaveSafetyState(stateKindFlap, payload)
	}
	if err != nil && f.storeLog != nil {
		f.storeLog.Warn("persisting flap history failed; state survives in memory only", "err", err)
	}
}
