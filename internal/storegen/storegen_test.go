// SPDX-License-Identifier: Apache-2.0

package storegen

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type memStore map[string][]byte

func (m memStore) LoadSafetyState(_ context.Context, kind string) ([]byte, error) {
	return m[kind], nil
}

func (m memStore) SaveSafetyState(_ context.Context, kind string, payload []byte) error {
	m[kind] = append([]byte(nil), payload...)
	return nil
}

func (m memStore) clone() memStore {
	c := memStore{}
	for k, v := range m {
		c[k] = append([]byte(nil), v...)
	}
	return c
}

const ns, lease = "kube-neuron", "fleet-controller"

var owner = metav1.OwnerReference{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: "fleet", UID: "root-uid"}

func tracker(client *fake.Clientset, store Store) *Tracker {
	return New(store, client.CoordinationV1().Leases(ns), lease, &owner)
}

func older(t *testing.T, tr *Tracker) bool {
	t.Helper()
	got, err := tr.Older(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func advance(t *testing.T, tr *Tracker) {
	t.Helper()
	if err := tr.Advance(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOlderOnlyForAStoreBehindItsCopy(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	store := memStore{}
	tr := tracker(client, store)

	// First installation: no Lease, no generation.
	if older(t, tr) {
		t.Fatal("first installation reported as older")
	}
	advance(t, tr)
	got, err := client.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{})
	if err != nil || got.Annotations[Annotation] != "1" || string(store[stateKind]) != "1" {
		t.Fatalf("after first start: lease %v err %v store %q", got, err, store[stateKind])
	}

	backup := store.clone()

	// Ordinary restarts and a change of leader see the same store.
	for range 3 {
		if older(t, tr) {
			t.Fatal("restart reported as older")
		}
		advance(t, tr)
	}

	// The store from the backup is behind the copy.
	restored := tracker(client, backup)
	if !older(t, restored) {
		t.Fatal("restored store not reported as older")
	}
	advance(t, restored)
	if older(t, restored) {
		t.Fatal("still older after advancing")
	}
}

func TestOlderWithoutACopy(t *testing.T) {
	// An upgrade from a version without the copy: the Lease exists, held by
	// leader election, with no annotation, and the store has no generation.
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: lease, Namespace: ns}})
	tr := tracker(client, memStore{})
	if older(t, tr) {
		t.Fatal("upgrade reported as older")
	}
	advance(t, tr)
	if older(t, tr) {
		t.Fatal("restart after upgrade reported as older")
	}

	// A store with a generation whose Lease was deleted.
	store := memStore{stateKind: []byte("7")}
	if older(t, tracker(fake.NewClientset(), store)) {
		t.Fatal("deleted Lease reported as older")
	}
}

func TestOlderReportsAnUnreadableCopy(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(coordinationv1.Resource("leases"), lease, errors.New("denied"))
	})
	if _, err := tracker(client, memStore{}).Older(context.Background()); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("got %v, want ErrNoCopy", err)
	}

	bad := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: lease, Namespace: ns, Annotations: map[string]string{Annotation: "x"},
	}})
	if _, err := tracker(bad, memStore{}).Older(context.Background()); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("got %v, want ErrNoCopy", err)
	}
}

func TestAdvanceWritesTheStoreFirstAndKeepsTheLease(t *testing.T) {
	ctx := context.Background()
	holder := "pod-a"
	renewed := metav1.NewMicroTime(time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC))
	client := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: lease, Namespace: ns, Annotations: map[string]string{"other": "kept"}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, RenewTime: &renewed},
	})
	store := memStore{}
	client.PrependReactor("update", "leases", func(action clienttesting.Action) (bool, runtime.Object, error) {
		written := action.(clienttesting.UpdateAction).GetObject().(*coordinationv1.Lease).Annotations[Annotation]
		if string(store[stateKind]) != written {
			t.Errorf("Lease written before the store: store %q, lease %q", store[stateKind], written)
		}
		return false, nil, nil
	})
	conflicts := 1
	client.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(coordinationv1.Resource("leases"), lease, errors.New("renewed"))
		}
		return false, nil, nil
	})
	advance(t, tracker(client, store))
	got, err := client.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[Annotation] != "1" || got.Annotations["other"] != "kept" || *got.Spec.HolderIdentity != holder ||
		!got.Spec.RenewTime.Equal(&renewed) || len(got.OwnerReferences) != 1 || got.OwnerReferences[0] != owner {
		t.Fatalf("lease after advance: %+v", got)
	}
	// A second write keeps one owner reference.
	advance(t, tracker(client, store))
	if got, _ = client.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{}); len(got.OwnerReferences) != 1 {
		t.Fatalf("owner references after a second advance: %v", got.OwnerReferences)
	}
}

// The Lease created without leader election belongs to the installation, so
// deleting the installation deletes the copy with it.
func TestAdvanceCreatesALeaseOwnedByTheInstallation(t *testing.T) {
	client := fake.NewClientset()
	advance(t, tracker(client, memStore{}))
	got, err := client.CoordinationV1().Leases(ns).Get(context.Background(), lease, metav1.GetOptions{})
	if err != nil || len(got.OwnerReferences) != 1 || got.OwnerReferences[0] != owner || got.Spec.HolderIdentity != nil {
		t.Fatalf("created lease %+v, %v", got, err)
	}
}

func TestAStoreAheadOfItsCopyIsNotOlder(t *testing.T) {
	// The controller stopped between writing the store and the copy.
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: lease, Namespace: ns, Annotations: map[string]string{Annotation: "4"},
	}})
	store := memStore{stateKind: []byte("5")}
	tr := tracker(client, store)
	if older(t, tr) {
		t.Fatal("store ahead of its copy reported as older")
	}
	advance(t, tr)
	if string(store[stateKind]) != "6" {
		t.Fatalf("store generation %q, want 6", store[stateKind])
	}
}

// A Lease left by an earlier installation of the same name, which garbage
// collection has not removed yet, is not this installation's copy.
func TestALeaseOfAnEarlierInstallationIsNoCopy(t *testing.T) {
	ctx := context.Background()
	earlier := owner
	earlier.UID = "earlier-root-uid"
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: lease, Namespace: ns, Annotations: map[string]string{Annotation: "40"},
		OwnerReferences: []metav1.OwnerReference{earlier},
	}})
	tr := tracker(client, memStore{})
	if got, err := tr.Older(ctx); got || !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Older = %v, %v; want not older and ErrNoCopy", got, err)
	}
	advance(t, tr)
	got, err := client.CoordinationV1().Leases(ns).Get(ctx, lease, metav1.GetOptions{})
	if err != nil || got.Annotations[Annotation] != "1" || len(got.OwnerReferences) != 1 || got.OwnerReferences[0] != owner {
		t.Fatalf("lease after advance: %+v, %v", got, err)
	}
	if older(t, tr) {
		t.Fatal("older after taking the Lease over")
	}
}

type slowStore struct {
	mu   sync.Mutex
	data memStore
}

func (s *slowStore) LoadSafetyState(ctx context.Context, kind string) ([]byte, error) {
	s.mu.Lock()
	payload := append([]byte(nil), s.data[kind]...)
	s.mu.Unlock()
	time.Sleep(time.Millisecond)
	if len(payload) == 0 {
		return nil, nil
	}
	return payload, nil
}

func (s *slowStore) SaveSafetyState(ctx context.Context, kind string, payload []byte) error {
	time.Sleep(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SaveSafetyState(ctx, kind, payload)
}

// The periodic raise and the one after a backup run at the same time; the
// store must never end up behind its copy.
func TestConcurrentAdvanceKeepsTheStoreAhead(t *testing.T) {
	client := fake.NewClientset()
	store := &slowStore{data: memStore{}}
	tr := tracker(client, store)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tr.Advance(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if older(t, tr) {
		t.Fatal("concurrent raises left the store behind its copy")
	}
	if got := string(store.data[stateKind]); got != "16" {
		t.Fatalf("store generation %s, want 16", got)
	}
}
