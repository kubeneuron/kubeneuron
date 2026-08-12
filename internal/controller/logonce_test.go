package controller

import (
	"strconv"
	"testing"
)

// The bound is the point. Node names are not a bounded set in a fleet this
// control plane replaces nodes in, so a suppression set keyed by them must not
// grow for the life of the process.
func TestLogOnceIsBounded(t *testing.T) {
	l := newLogOnce(8)
	for i := 0; i < 1000; i++ {
		l.first("node-" + strconv.Itoa(i))
	}
	l.mu.Lock()
	live, prior := len(l.seen), len(l.prior)
	l.mu.Unlock()
	if live > 8 || prior > 8 {
		t.Fatalf("retained %d live + %d prior keys with a capacity of 8; the set is unbounded", live, prior)
	}
}

func TestLogOnceSuppressesARepeat(t *testing.T) {
	l := newLogOnce(8)
	if !l.first("a") {
		t.Fatal("first sighting must report true")
	}
	if l.first("a") {
		t.Fatal("a repeat must be suppressed")
	}
	// A key evicted from the live set is still suppressed one generation on,
	// so a fleet at capacity does not re-log everything every cycle.
	for i := 0; i < 8; i++ {
		l.first("filler-" + strconv.Itoa(i))
	}
	if l.first("a") {
		t.Fatal("a key one generation old must still be suppressed")
	}
}
