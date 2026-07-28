package safety

import (
	"log/slog"
	"sync"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// FlapDetector quarantines targets that keep bouncing between resolved and
// reopened: when a (target, class) pair resolves and reopens Threshold times
// within Window, automation stops and the incident goes to NEEDS_HUMAN.
//
// A reopen only counts when a resolution was recorded for the pair since the
// last counted reopen: a stream of brand-new incidents (nothing ever
// resolved) is churn for the concurrency gate, not flapping, and a retried
// open transition must not count twice.
type FlapDetector struct {
	mu        sync.Mutex
	threshold int
	window    time.Duration
	reopens   map[string][]time.Time // key: target|class
	// pendingResolve[key] holds the time of the pair's most recent
	// resolution not yet consumed by a reopen.
	pendingResolve map[string]time.Time
	now            func() time.Time
	// store, when set via RestoreAndPersist, receives a write-through
	// snapshot after every change so restarts keep flap history.
	store    StateStore
	storeLog *slog.Logger
}

// NewFlapDetector builds a detector; threshold <= 0 disables detection.
func NewFlapDetector(threshold int, window time.Duration) *FlapDetector {
	return &FlapDetector{
		threshold:      threshold,
		window:         window,
		reopens:        map[string][]time.Time{},
		pendingResolve: map[string]time.Time{},
		now:            time.Now,
	}
}

// RecordResolved registers that an incident on (target, class) resolved. The
// next RecordReopen for the pair counts as one flap cycle.
func (f *FlapDetector) RecordResolved(target types.Target, class types.ProblemClass) {
	if f.threshold <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingResolve[targetKey(target)+"|"+string(class)] = f.now()
	f.gcLocked()
	f.persistLocked()
}

// RecordReopen registers that a new incident opened on (target, class) and
// returns true when the pair is now flapping and must be quarantined. The
// call is idempotent until the next resolution: without a pending resolution
// it only reports the current verdict and counts nothing.
func (f *FlapDetector) RecordReopen(target types.Target, class types.ProblemClass) bool {
	if f.threshold <= 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	key := targetKey(target) + "|" + string(class)
	cutoff := f.now().Add(-f.window)
	kept := f.reopens[key][:0]
	for _, t := range f.reopens[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if resolvedAt, ok := f.pendingResolve[key]; ok {
		delete(f.pendingResolve, key)
		if resolvedAt.After(cutoff) {
			kept = append(kept, f.now())
		}
	}
	if len(kept) == 0 {
		delete(f.reopens, key)
	} else {
		f.reopens[key] = kept
	}
	f.gcLocked()
	f.persistLocked()
	return len(kept) >= f.threshold
}

// gcLocked drops pairs with no activity inside the window so the maps do not
// grow without bound across months of distinct nodes. Called with f.mu held.
func (f *FlapDetector) gcLocked() {
	cutoff := f.now().Add(-f.window)
	for key, times := range f.reopens {
		live := false
		for _, t := range times {
			if t.After(cutoff) {
				live = true
				break
			}
		}
		if !live {
			delete(f.reopens, key)
		}
	}
	for key, t := range f.pendingResolve {
		if !t.After(cutoff) {
			delete(f.pendingResolve, key)
		}
	}
}
