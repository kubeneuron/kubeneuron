package controller

import "sync"

// lru suppresses repeated log lines for conditions that persist, without
// growing without bound.
//
// The bound is the whole reason this type exists rather than a sync.Map. Every
// key here embeds a node name, and this control plane REPLACES nodes: each
// ReplaceNode mints a name that never recurs, and a cluster autoscaler mints
// more. An unbounded map keyed by node name grows for the life of the process
// and never shrinks — the same argument that removed the `node` label from
// kubeneuron_workloads_evicted_total, and it applies just as well to a map as
// to a metric.
//
// Eviction is deliberately crude: at capacity the whole set is dropped. The
// only cost of a lost entry is one repeated log line, so an exact LRU would be
// machinery bought for nothing. Capacity is sized so a normal fleet never
// evicts at all, and a fleet that does gets its suppression reset roughly once
// per that many distinct conditions.
type lru struct {
	mu    sync.Mutex
	cap   int
	seen  map[string]struct{}
	prior map[string]struct{}
}

func newLogOnce(capacity int) *lru {
	return &lru{cap: capacity, seen: make(map[string]struct{})}
}

// first reports whether this is the first time key has been seen, and records
// it. Concurrent callers race only to decide which one logs, never to corrupt
// the set.
func (l *lru) first(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[key]; ok {
		return false
	}
	// A generation behind the live set, so a key evicted a moment ago is still
	// suppressed for one more cycle rather than immediately re-logging.
	if _, ok := l.prior[key]; ok {
		return false
	}
	if len(l.seen) >= l.cap {
		l.prior = l.seen
		l.seen = make(map[string]struct{}, l.cap)
	}
	l.seen[key] = struct{}{}
	return true
}
