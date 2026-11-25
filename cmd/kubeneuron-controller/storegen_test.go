// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/internal/controller"
	"github.com/kubeneuron/kubeneuron/internal/notify"
	"github.com/kubeneuron/kubeneuron/internal/safety"
	"github.com/kubeneuron/kubeneuron/internal/store"
	"github.com/kubeneuron/kubeneuron/internal/store/postgres"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
	"github.com/kubeneuron/kubeneuron/internal/storegen"
	"github.com/kubeneuron/kubeneuron/pkg/types"
)

const testLeaseNamespace, testLease = "kube-neuron", "fleet-controller"

type generationStore interface {
	store.Store
	safety.StateStore
}

type countingNotifier struct {
	mu     sync.Mutex
	events []notify.NotifyEvent
}

func (n *countingNotifier) Notify(_ context.Context, ev notify.NotifyEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return nil
}

func (n *countingNotifier) RequestApproval(context.Context, *types.Incident, string) error {
	return nil
}

func (n *countingNotifier) storeOlder() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, ev := range n.events {
		if ev.Kind == notify.EventStoreOlder && ev.Message == notify.StoreOlderMessage {
			count++
		}
	}
	return count
}

// start runs what a controller does with the store generation when it
// starts or becomes leader, and returns its gate.
func start(t *testing.T, st generationStore, client *fake.Clientset, notifier notify.Notifier) *safety.Gate {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1})
	if err := gate.RestoreAndPersist(ctx, st, log); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(st, nil, nil, gate, nil, nil, nil, notifier, log)
	tracker := storegen.New(st, client.CoordinationV1().Leases(testLeaseNamespace), testLease, nil)
	if err := checkStoreGeneration(ctx, tracker, ctrl, log); err != nil {
		t.Fatal(err)
	}
	return gate
}

// copyRows is a backup of the rows the store generation and the pause live
// in; putting them back is a restore.
type copyRows map[string][]byte

func backup(t *testing.T, st generationStore) copyRows {
	t.Helper()
	rows := copyRows{}
	for _, kind := range []string{"store_generation", "pause"} {
		payload, err := st.LoadSafetyState(context.Background(), kind)
		if err != nil {
			t.Fatal(err)
		}
		rows[kind] = payload
	}
	return rows
}

func restore(t *testing.T, st generationStore, rows copyRows) {
	t.Helper()
	for kind, payload := range rows {
		if payload == nil {
			payload = []byte(`{"paused":false}`)
			if kind == "store_generation" {
				payload = []byte("0")
			}
		}
		if err := st.SaveSafetyState(context.Background(), kind, payload); err != nil {
			t.Fatal(err)
		}
	}
}

func testStoreGeneration(t *testing.T, open func(t *testing.T) generationStore) {
	ctx := context.Background()

	t.Run("first installation, restarts and a change of leader", func(t *testing.T) {
		st, client, notifier := open(t), fake.NewClientset(), &countingNotifier{}
		for range 3 {
			if gate := start(t, st, client, notifier); gate.Paused() {
				t.Fatal("paused without an older store")
			}
		}
		if n := notifier.storeOlder(); n != 0 {
			t.Fatalf("%d notices, want none", n)
		}
	})

	t.Run("upgrade from a version without the copy", func(t *testing.T) {
		st, notifier := open(t), &countingNotifier{}
		holder := "controller-0"
		client := fake.NewClientset(&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: testLease, Namespace: testLeaseNamespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		})
		if gate := start(t, st, client, notifier); gate.Paused() || notifier.storeOlder() != 0 {
			t.Fatal("upgrade paused automation")
		}
	})

	// The Lease is owned by the installation and deleted with it, so a new
	// installation of the same name with a clean store finds no copy.
	t.Run("reinstall after the Lease went with the installation", func(t *testing.T) {
		st, notifier := open(t), &countingNotifier{}
		start(t, st, fake.NewClientset(), notifier)
		if gate := start(t, st, fake.NewClientset(), notifier); gate.Paused() || notifier.storeOlder() != 0 {
			t.Fatal("a missing copy paused automation")
		}
	})

	t.Run("older store pauses once until resume", func(t *testing.T) {
		st, client, notifier := open(t), fake.NewClientset(), &countingNotifier{}
		start(t, st, client, notifier)
		rows := backup(t, st)
		start(t, st, client, notifier)

		restore(t, st, rows)
		gate := start(t, st, client, notifier)
		if !gate.Paused() || !gate.PausedForOlderStore() || notifier.storeOlder() != 1 {
			t.Fatalf("after restore: paused=%v older=%v notices=%d", gate.Paused(), gate.PausedForOlderStore(), notifier.storeOlder())
		}
		// A restart before resume keeps the pause and sends nothing more.
		gate = start(t, st, client, notifier)
		if !gate.PausedForOlderStore() || notifier.storeOlder() != 1 {
			t.Fatalf("restart after restore: older=%v notices=%d", gate.PausedForOlderStore(), notifier.storeOlder())
		}
		if err := gate.SetPaused(ctx, false, "alice"); err != nil {
			t.Fatal(err)
		}
		if gate = start(t, st, client, notifier); gate.Paused() || notifier.storeOlder() != 1 {
			t.Fatalf("after resume: paused=%v notices=%d", gate.Paused(), notifier.storeOlder())
		}
	})

	t.Run("a stop after the pause and before the generation is raised", func(t *testing.T) {
		st, client, notifier := open(t), fake.NewClientset(), &countingNotifier{}
		start(t, st, client, notifier)
		rows := backup(t, st)
		start(t, st, client, notifier)
		restore(t, st, rows)
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		gate := safety.NewGate(safety.Limits{MaxConcurrentRemediations: 1})
		if err := gate.RestoreAndPersist(ctx, st, log); err != nil {
			t.Fatal(err)
		}
		if err := controller.New(st, nil, nil, gate, nil, nil, nil, notifier, log).PauseForOlderStore(ctx); err != nil {
			t.Fatal(err)
		}
		// The store is still behind its copy; the next start pauses again
		// but sends no second notice.
		if gate = start(t, st, client, notifier); !gate.PausedForOlderStore() || notifier.storeOlder() != 1 {
			t.Fatalf("older=%v notices=%d", gate.PausedForOlderStore(), notifier.storeOlder())
		}
	})

	t.Run("restored store that was already paused", func(t *testing.T) {
		st, client, notifier := open(t), fake.NewClientset(), &countingNotifier{}
		gate := start(t, st, client, notifier)
		if err := gate.SetPaused(ctx, true, "alice"); err != nil {
			t.Fatal(err)
		}
		rows := backup(t, st)
		start(t, st, client, notifier)
		restore(t, st, rows)
		if gate = start(t, st, client, notifier); !gate.PausedForOlderStore() || notifier.storeOlder() != 1 {
			t.Fatalf("older=%v notices=%d", gate.PausedForOlderStore(), notifier.storeOlder())
		}
	})
}

func TestStoreGenerationSQLite(t *testing.T) {
	testStoreGeneration(t, func(t *testing.T) generationStore {
		st, err := storesqlite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	})
}

func TestStoreGenerationPostgres(t *testing.T) {
	dsn := os.Getenv("KUBENEURON_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL test DSN not set")
	}
	testStoreGeneration(t, func(t *testing.T) generationStore {
		ctx := context.Background()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		schema := fmt.Sprintf("store_generation_%d", time.Now().UnixNano())
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid PostgreSQL test URL")
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		st, err := postgres.Open(ctx, u.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	})
}

// A snapshot from the backup endpoint, restored as the database file, is
// recognised: the endpoint raises the generation after writing it.
func TestStoreGenerationSQLiteSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	st, err := storesqlite.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	client, notifier := fake.NewClientset(), &countingNotifier{}
	start(t, st, client, notifier)
	snapshot := filepath.Join(dir, "snapshot.db")
	if err := st.BackupTo(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := storegen.New(st, client.CoordinationV1().Leases(testLeaseNamespace), testLease, nil).Advance(ctx); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	restored, err := storesqlite.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if gate := start(t, restored, client, notifier); !gate.PausedForOlderStore() || notifier.storeOlder() != 1 {
		t.Fatalf("restored snapshot: older=%v notices=%d", gate.PausedForOlderStore(), notifier.storeOlder())
	}
}
