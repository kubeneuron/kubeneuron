package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

// faultyQualificationStore is a controlled transactional store: it delegates
// every statement to the real SQLite store and its real transaction, but can
// make one write fail AFTER it has actually executed. A rollback then has to
// undo a genuine row change, so a passing test proves the store discarded the
// write rather than that the write was never attempted. It also records the
// statement order of every transaction so a test can assert that a key
// claim, the resource write, and the audit appends shared one transaction.
type faultyQualificationStore struct {
	store.OperationalTransactionalStore
	failCreate   error
	failUpdate   error
	failAuditAt  int // 1-based index of the audit append within one transaction that fails; 0 disables
	failAuditErr error
	transactions [][]string
}

type faultyQualificationTx struct {
	store.OperationalTx
	parent *faultyQualificationStore
	audits int
	calls  *[]string
}

func (f *faultyQualificationStore) WithOperationalTx(ctx context.Context, fn func(store.OperationalTx) error) error {
	calls := []string{}
	err := f.OperationalTransactionalStore.WithOperationalTx(ctx, func(tx store.OperationalTx) error {
		return fn(&faultyQualificationTx{OperationalTx: tx, parent: f, calls: &calls})
	})
	f.transactions = append(f.transactions, calls)
	return err
}

func (f *faultyQualificationTx) PutOperationalIdempotency(ctx context.Context, record *types.OperationalIdempotencyRecord) (*types.OperationalIdempotencyRecord, bool, error) {
	*f.calls = append(*f.calls, "put-key")
	return f.OperationalTx.PutOperationalIdempotency(ctx, record)
}

func (f *faultyQualificationTx) CreateOperationalResource(ctx context.Context, resource *types.OperationalResource) error {
	*f.calls = append(*f.calls, "create")
	if err := f.OperationalTx.CreateOperationalResource(ctx, resource); err != nil {
		return err
	}
	return f.parent.failCreate
}

func (f *faultyQualificationTx) UpdateOperationalResource(ctx context.Context, resource *types.OperationalResource, expectedVersion int) error {
	*f.calls = append(*f.calls, "update")
	if err := f.OperationalTx.UpdateOperationalResource(ctx, resource, expectedVersion); err != nil {
		return err
	}
	return f.parent.failUpdate
}

func (f *faultyQualificationTx) AppendOperationalAudit(ctx context.Context, event *types.OperationalAuditEvent) error {
	f.audits++
	*f.calls = append(*f.calls, "audit:"+event.Action)
	if err := f.OperationalTx.AppendOperationalAudit(ctx, event); err != nil {
		return err
	}
	if f.parent.failAuditAt == f.audits {
		return f.parent.failAuditErr
	}
	return nil
}

func (f *faultyQualificationStore) clear() {
	f.failCreate, f.failUpdate, f.failAuditAt, f.failAuditErr = nil, nil, 0, nil
}

func newFaultyQualificationManager(t *testing.T) (*Manager, *sqlite.Store, *faultyQualificationStore, *qualificationFixture) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	faulty := &faultyQualificationStore{OperationalTransactionalStore: st}
	fixture := newQualificationFixture()
	mgr := New(Options{
		Resources: faulty, Workflow: st,
		Now:                          func() time.Time { return fixture.now },
		ListNodes:                    fixture.list,
		BuildRuntimeContractCoverage: fixture.build,
	})
	return mgr, st, faulty, fixture
}

// qualificationChainIntact fails the test when the audit chain of id has a
// gap: every event must chain onto the hash of the event before it and the
// first onto the empty genesis hash. A rollback that left the head row
// advanced past a discarded event would break this on the next append.
func qualificationChainIntact(t *testing.T, st *sqlite.Store, id string) {
	t.Helper()
	events, err := st.ListOperationalAudit(context.Background(), types.ResourceRuntimeContractQualification, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	for index, event := range events {
		if event.PrevHash != previous || event.Hash == "" {
			t.Fatalf("audit event %d (%s) prev hash %q, want %q: chain broken", index, event.Action, event.PrevHash, previous)
		}
		previous = event.Hash
	}
}

func qualificationKeyAbsent(t *testing.T, st *sqlite.Store, actor, operation, key string) {
	t.Helper()
	if _, err := st.GetOperationalIdempotency(context.Background(), types.ResourceRuntimeContractQualification, actor, scopedIdempotencyKey(operation, key)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("idempotency key %s:%s after failed write = %v, want ErrNotFound (key must not be poisoned)", operation, key, err)
	}
}

func TestQualificationCreateRollsBackKeyAndResourceOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk full")
	for name, arm := range map[string]func(*faultyQualificationStore){
		"resource insert fails": func(f *faultyQualificationStore) { f.failCreate = boom },
		"create audit fails":    func(f *faultyQualificationStore) { f.failAuditAt, f.failAuditErr = 1, boom },
	} {
		t.Run(name, func(t *testing.T) {
			mgr, st, faulty, _ := newFaultyQualificationManager(t)
			arm(faulty)
			got, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("rb"))
			if !errors.Is(err, boom) || got != nil || replayed {
				t.Fatalf("create with a failing write = (%#v, %v, %v), want the write error", got, replayed, err)
			}
			qualificationKeyAbsent(t, st, "alice", "create", "rb")
			resources, listErr := st.ListOperationalResources(ctx, types.OperationalResourceFilter{Kind: types.ResourceRuntimeContractQualification, IncludeExpired: true})
			if listErr != nil || len(resources) != 0 {
				t.Fatalf("resources after rolled back create = (%d, %v), want none", len(resources), listErr)
			}
			events, auditErr := st.ListOperationalAuditEvents(ctx, types.OperationalAuditFilter{Kind: types.ResourceRuntimeContractQualification})
			if auditErr != nil || len(events) != 0 {
				t.Fatalf("audit after rolled back create = (%d, %v), want none", len(events), auditErr)
			}
			// The same key is retryable once the store recovers, and only then
			// does a further retry replay.
			faulty.clear()
			created, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("rb"))
			if err != nil || replayed || created == nil {
				t.Fatalf("retry after rollback = (%#v, %v, %v), want a fresh create", created, replayed, err)
			}
			again, replayed, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("rb"))
			if err != nil || !replayed || again.ID != created.ID {
				t.Fatalf("replay after successful retry = (%#v, %v, %v)", again, replayed, err)
			}
			if actions := auditActions(t, st, created.ID); actions != "create=Observing" {
				t.Fatalf("audit after retry = %q", actions)
			}
			qualificationChainIntact(t, st, created.ID)
			successful := faulty.transactions[len(faulty.transactions)-2]
			if strings.Join(successful, ",") != "put-key,create,audit:create" {
				t.Fatalf("successful create transaction = %v, want key claim, insert, and audit in one transaction", successful)
			}
			if replay := faulty.transactions[len(faulty.transactions)-1]; strings.Join(replay, ",") != "put-key" {
				t.Fatalf("replay transaction = %v, want the key claim alone", replay)
			}
		})
	}
}

// A concurrent writer advancing the resource between the read-only evidence
// capture and the write makes the version-guarded update fail. The whole
// transaction, including the key claimed moments earlier, rolls back, so the
// caller can re-read and retry with the same key and the key never replays an
// observation that was not recorded.
func TestQualificationObserveRollsBackKeyOnOptimisticConflict(t *testing.T) {
	mgr, st, fixture := newQualificationManager(t)
	ctx := context.Background()
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("conflict"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(time.Minute)
	// The coverage builder runs during capture, after the snapshot read and
	// before the transaction: the ideal moment for a competing writer.
	raced := false
	fixture.mutateReport = func(string, *types.AgentAcceleratorReport) {
		if raced {
			return
		}
		raced = true
		resource, err := st.GetOperationalResource(ctx, types.ResourceRuntimeContractQualification, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOperationalResource(ctx, resource, resource.Version); err != nil {
			t.Fatal(err)
		}
	}
	got, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: 1, IdempotencyKey: "race"})
	if !errors.Is(err, store.ErrOperationalConflict) || got != nil || replayed {
		t.Fatalf("observe racing a writer = (%#v, %v, %v), want ErrOperationalConflict", got, replayed, err)
	}
	if !raced {
		t.Fatal("the competing write never ran")
	}
	qualificationKeyAbsent(t, st, "bob", "observe", "race")
	stored, err := mgr.GetRuntimeContractQualification(ctx, created.ID)
	if err != nil || stored.ResourceVersion != 2 || stored.TotalObservations != 0 || stored.LastObservedAt != nil {
		t.Fatalf("stored after conflict = (%#v, %v), want only the competing writer's version bump", stored, err)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing" {
		t.Fatalf("audit after conflict = %q, want no observe event", actions)
	}
	// Re-read and retry with the same key: not a replay, a real observation.
	fixture.mutateReport = nil
	retried, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: stored.ResourceVersion, IdempotencyKey: "race"})
	if err != nil || replayed || retried.ResourceVersion != 3 || retried.TotalObservations != 1 || retried.SuccessfulSamples != 1 {
		t.Fatalf("retry after conflict = (%#v, %v, %v), want a recorded observation", retried, replayed, err)
	}
	replay, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: stored.ResourceVersion, IdempotencyKey: "race"})
	if err != nil || !replayed || replay.ResourceVersion != 3 || replay.TotalObservations != 1 {
		t.Fatalf("replay after retry = (%#v, %v, %v)", replay, replayed, err)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full" {
		t.Fatalf("audit after retry = %q", actions)
	}
	qualificationChainIntact(t, st, created.ID)
}

// An observation that also transitions the lifecycle writes the resource and
// two audit events. Whichever of the three fails, none of them and no key
// survives; when all succeed they were one transaction.
func TestQualificationObserveRollsBackUpdateAndBothAuditsTogether(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("connection reset")
	for name, tc := range map[string]struct {
		arm  func(*faultyQualificationStore)
		want error
	}{
		"resource update fails":  {func(f *faultyQualificationStore) { f.failUpdate = boom }, boom},
		"observe audit fails":    {func(f *faultyQualificationStore) { f.failAuditAt, f.failAuditErr = 1, boom }, boom},
		"transition audit fails": {func(f *faultyQualificationStore) { f.failAuditAt, f.failAuditErr = 2, boom }, boom},
		"transition audit chain conflict": {func(f *faultyQualificationStore) {
			f.failAuditAt, f.failAuditErr = 2, store.ErrOperationalConflict
		}, store.ErrOperationalConflict},
	} {
		t.Run(name, func(t *testing.T) {
			mgr, st, faulty, fixture := newFaultyQualificationManager(t)
			created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("atomic"))
			if err != nil {
				t.Fatal(err)
			}
			// Recreating a node is drift: the observation invalidates, which is
			// an observe event plus an invalidate event with a state change.
			fixture.nodes[1] = qualificationNode("gpu-a", "uid-a-reborn")
			fixture.now = fixture.now.Add(time.Minute)
			tc.arm(faulty)
			got, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: 1, IdempotencyKey: "tx"})
			if !errors.Is(err, tc.want) || got != nil || replayed {
				t.Fatalf("observe with a failing write = (%#v, %v, %v), want %v", got, replayed, err, tc.want)
			}
			qualificationKeyAbsent(t, st, "bob", "observe", "tx")
			stored, getErr := mgr.GetRuntimeContractQualification(ctx, created.ID)
			if getErr != nil || stored.ResourceVersion != 1 || stored.State != QualificationObserving || stored.TotalObservations != 0 || stored.InvalidatedAt != nil {
				t.Fatalf("stored after rollback = (%#v, %v), want version 1 Observing with no observation", stored, getErr)
			}
			if actions := auditActions(t, st, created.ID); actions != "create=Observing" {
				t.Fatalf("audit after rollback = %q, want no partial observe/invalidate events", actions)
			}
			// The same key retries into a real observation, not a replay of a
			// state that was never committed; afterwards it replays.
			faulty.clear()
			retried, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: 1, IdempotencyKey: "tx"})
			if err != nil || replayed || retried.State != QualificationInvalidated || retried.ResourceVersion != 2 || retried.TotalObservations != 1 {
				t.Fatalf("retry after rollback = (%#v, %v, %v), want Invalidated at version 2", retried, replayed, err)
			}
			if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=drift,invalidate=Invalidated" {
				t.Fatalf("audit after retry = %q", actions)
			}
			qualificationChainIntact(t, st, created.ID)
			successful := faulty.transactions[len(faulty.transactions)-1]
			if strings.Join(successful, ",") != "put-key,update,audit:observe,audit:invalidate" {
				t.Fatalf("successful observe transaction = %v, want key claim, update, and both audits in one transaction", successful)
			}
			replay, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{ExpectedVersion: 1, IdempotencyKey: "tx"})
			if err != nil || !replayed || replay.ResourceVersion != 2 || replay.TotalObservations != 1 || replay.State != QualificationInvalidated {
				t.Fatalf("replay = (%#v, %v, %v)", replay, replayed, err)
			}
			if len(faulty.transactions) != 3 {
				t.Fatalf("a pre-checked replay must not open a write transaction: %v", faulty.transactions)
			}
		})
	}
}

// Failures keep recurring across retries until the store recovers; every
// failed attempt leaves the key free, and the first success is a real write.
func TestQualificationObserveRepeatedFailuresNeverPoisonKey(t *testing.T) {
	mgr, st, faulty, fixture := newFaultyQualificationManager(t)
	ctx := context.Background()
	created, _, err := mgr.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("repeat"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(time.Minute)
	boom := errors.New("transient")
	for attempt := 0; attempt < 3; attempt++ {
		if attempt%2 == 0 {
			faulty.failUpdate = boom
		} else {
			faulty.failAuditAt, faulty.failAuditErr = 1, boom
		}
		if _, _, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "same"}); !errors.Is(err, boom) {
			t.Fatalf("attempt %d = %v, want the transient error", attempt, err)
		}
		faulty.clear()
		qualificationKeyAbsent(t, st, "bob", "observe", "same")
		if stored, _ := mgr.GetRuntimeContractQualification(ctx, created.ID); stored.ResourceVersion != 1 || stored.TotalObservations != 0 {
			t.Fatalf("attempt %d left state behind: %#v", attempt, stored)
		}
	}
	got, replayed, err := mgr.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "same"})
	if err != nil || replayed || got.ResourceVersion != 2 || got.TotalObservations != 1 {
		t.Fatalf("first healthy attempt = (%#v, %v, %v), want a recorded observation", got, replayed, err)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing,observe=full" {
		t.Fatalf("audit = %q", actions)
	}
	qualificationChainIntact(t, st, created.ID)
}

// plainOperationalStore hides the transactional capability of the SQLite
// store: it is what an out-of-tree OperationalStore without WithOperationalTx
// looks like to the manager.
type plainOperationalStore struct{ store.OperationalStore }

func TestQualificationRejectsNontransactionalOperationalStore(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := newQualificationFixture()
	transactional := New(Options{Resources: st, Workflow: st, Now: func() time.Time { return fixture.now }, ListNodes: fixture.list, BuildRuntimeContractCoverage: fixture.build})
	created, _, err := transactional.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("seed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(plainOperationalStore{st}).(store.OperationalTransactionalStore); ok {
		t.Fatal("test double must not expose WithOperationalTx")
	}
	plain := New(Options{Resources: plainOperationalStore{st}, Workflow: st, Now: func() time.Time { return fixture.now }, ListNodes: fixture.list, BuildRuntimeContractCoverage: fixture.build})
	if _, _, err := plain.CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("plain")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("create on a nontransactional store = %v, want ErrUnavailable", err)
	}
	qualificationKeyAbsent(t, st, "alice", "create", "plain")
	fixture.now = fixture.now.Add(time.Minute)
	if _, _, err := plain.ObserveRuntimeContractQualification(ctx, "bob", created.ID, RuntimeContractQualificationMutation{IdempotencyKey: "plain"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("observe on a nontransactional store = %v, want ErrUnavailable", err)
	}
	qualificationKeyAbsent(t, st, "bob", "observe", "plain")
	if stored, err := plain.GetRuntimeContractQualification(ctx, created.ID); err != nil || stored.ResourceVersion != 1 || stored.TotalObservations != 0 {
		t.Fatalf("get on a nontransactional store = (%#v, %v), want the untouched read to work", stored, err)
	}
	if listed, err := plain.ListRuntimeContractQualifications(ctx, OperationalListOptions{IncludeExpired: true}); err != nil || len(listed) != 1 {
		t.Fatalf("list on a nontransactional store = (%d, %v), want reads to work", len(listed), err)
	}
	if actions := auditActions(t, st, created.ID); actions != "create=Observing" {
		t.Fatalf("audit after rejected writes = %q", actions)
	}
	// A manager without any store at all fails the same way.
	if _, _, err := New(Options{ListNodes: fixture.list, BuildRuntimeContractCoverage: fixture.build}).CreateRuntimeContractQualification(ctx, "alice", qualificationRequest("none")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("create without a store = %v, want ErrUnavailable", err)
	}
}
