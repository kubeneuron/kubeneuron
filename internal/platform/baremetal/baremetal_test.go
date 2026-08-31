package baremetal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestBaremetalCountsCordonHolders is the compile-time guarantee made concrete.
//
// Counting holders used to be an OPTIONAL interface with a fallback to the
// unguarded Cordon/Uncordon pair, and a platform that did not implement it —
// this one — silently got the single-owner behaviour. That is the P0 the
// counting exists to prevent: a machine has several GPUs, two incidents can
// hold it, and the first to finish handed it back while the other was still
// resetting a GPU on it. A new adapter would have compiled, passed its own
// tests, and been wrong only in production.
func TestBaremetalCountsCordonHolders(t *testing.T) {
	p, err := New("", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := p.CordonForOwner(ctx, "node-a", "inc-1", "ecc-dbe"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", "inc-2", "xid-79"); err != nil {
		t.Fatal(err)
	}

	released, remaining, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("the first incident to finish released a node the second still holds; on a " +
			"multi-GPU machine that returns tenant work to a node whose other GPU is about " +
			"to be reset")
	}
	if remaining != 1 {
		t.Fatalf("remaining holders = %d, want 1", remaining)
	}

	released, remaining, err = p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !released || remaining != 0 {
		t.Fatalf("the last holder leaving did not release the node: released=%v remaining=%d",
			released, remaining)
	}

	// Releasing an owner that is not there is a no-op, not an error: steps are
	// retried and replayed, and a release that already happened must stay quiet.
	if _, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"}); err != nil {
		t.Fatalf("a replayed release returned an error: %v", err)
	}
}

// TestBaremetalUnownedCordonIsItsOwnHolder: a caller acting on the node rather
// than on behalf of an incident still must not release somebody else's hold,
// and must not have its own hold released by an incident finishing.
func TestBaremetalUnownedCordonIsItsOwnHolder(t *testing.T) {
	p, err := New("", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := p.Cordon(ctx, "node-a", "operator did this by hand"); err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", "inc-1", "ecc-dbe"); err != nil {
		t.Fatal(err)
	}

	released, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("an incident's release returned a node that was also being held without an " +
			"owner; the unowned hold is somebody's deliberate act and outlives the incident")
	}

	if err := p.Uncordon(ctx, "node-a"); err != nil {
		t.Fatal(err)
	}
	if released, _, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"nobody"}); err != nil || released {
		t.Fatalf("releasing an absent owner on a free node reported a release: released=%v err=%v",
			released, err)
	}
}

func TestCordonJournalSurvivesRestartAndRunsTheHookOnlyAtTheEdges(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hook.log")
	scriptPath := filepath.Join(dir, "cordon-hook")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> "+shellQuote(logPath)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hooks := Hooks{CordonScript: scriptPath, CordonStateFile: filepath.Join(dir, "cordons.json")}
	p, err := New("", hooks)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.CordonForOwner(ctx, "node-a", "inc-1", "ecc-dbe"); err != nil {
		t.Fatal(err)
	}

	// A new controller process sees the first holder. Joining and releasing it
	// must not repeat either physical edge; only the final holder uncordons.
	p, err = New("", hooks)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CordonForOwner(ctx, "node-a", "inc-2", "xid-79"); err != nil {
		t.Fatal(err)
	}
	if released, remaining, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-1"}); err != nil || released || remaining != 1 {
		t.Fatalf("first release = released=%v remaining=%d err=%v, want false/1/nil", released, remaining, err)
	}
	if released, remaining, err := p.ReleaseCordonOwners(ctx, "node-a", []string{"inc-2"}); err != nil || !released || remaining != 0 {
		t.Fatalf("last release = released=%v remaining=%d err=%v, want true/0/nil", released, remaining, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(data)), []string{"cordon", "node-a", "uncordon", "node-a"}; !slicesEqual(got, want) {
		t.Fatalf("hook calls = %q, want %q", got, want)
	}
}

func TestConcurrentFirstHoldersRunOneCordonHook(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hook.log")
	scriptPath := filepath.Join(dir, "cordon-hook")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+shellQuote(logPath)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := New("", Hooks{CordonScript: scriptPath, CordonStateFile: filepath.Join(dir, "cordons.json")})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			errs <- p.CordonForOwner(context.Background(), "node-a", owner, "test")
		}(string(rune('a' + i)))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "cordon" {
		t.Fatalf("concurrent holders ran hook %q, want one cordon", got)
	}
}

func TestCordonHookRequiresDurableJournalAndIncompleteJournalFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := New("", Hooks{CordonScript: "/bin/true"}); err == nil {
		t.Fatal("a physical cordon hook without a durable owner journal was accepted")
	}
	statePath := filepath.Join(dir, "cordons.json")
	if err := os.WriteFile(statePath, []byte(`{"version":1,"cordons":{"node-a":{"owners":["inc-1"],"reason":"test","phase":"cordoning"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New("", Hooks{CordonStateFile: statePath}); err == nil {
		t.Fatal("a journal interrupted in the middle of a hook was accepted; startup must stop rather than guess whether the node is safe")
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
