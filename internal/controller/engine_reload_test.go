package controller

import (
	"io"
	"log/slog"
	"testing"

	"github.com/kubeneuron/kubeneuron/internal/playbook"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

func testEngine(t *testing.T, playbookName, class string) *playbook.Engine {
	t.Helper()
	book := &playbook.Playbook{
		Name:   playbookName,
		Target: "gpu",
		Steps:  []playbook.Step{{Name: "observe", Action: "notify.observe"}},
	}
	engine, err := playbook.NewEngine(
		map[string]*playbook.Playbook{playbookName: book},
		[]playbook.Policy{{Class: types.ProblemClass(class), Playbook: playbookName}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// The engine has to be swappable while the reconcile loop reads it, because a
// config change now reloads the engine in place rather than rolling the
// Deployment — a rollout deadlocks under leader election.
func TestSetEngineSwapsThePlaybookInUse(t *testing.T) {
	c := New(nil, nil, testEngine(t, "old-book", "old-class"), nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, ok := c.currentEngine().Playbook("old-book"); !ok {
		t.Fatal("initial engine must serve the original playbook")
	}

	c.SetEngine(testEngine(t, "new-book", "new-class"))

	if _, ok := c.currentEngine().Playbook("new-book"); !ok {
		t.Fatal("after SetEngine the new playbook must be live")
	}
	if _, ok := c.currentEngine().Playbook("old-book"); ok {
		t.Fatal("the old engine must be fully replaced, not merged")
	}
	if _, ok := c.currentEngine().PolicyFor(types.ProblemClass("new-class")); !ok {
		t.Fatal("the swapped engine's policies must be the ones in force")
	}
}

// A controller that has not been given an engine must not panic when the
// reconcile path reads it — reload installs it slightly after construction.
func TestCurrentEngineToleratesNoEngine(t *testing.T) {
	c := New(nil, nil, nil, nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c.currentEngine() != nil {
		t.Fatal("want nil engine before one is set")
	}
	c.SetEngine(testEngine(t, "book", "class"))
	if c.currentEngine() == nil {
		t.Fatal("want the engine live after SetEngine")
	}
}
