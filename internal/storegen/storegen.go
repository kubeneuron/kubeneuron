// SPDX-License-Identifier: Apache-2.0

// Package storegen tells whether the workflow store is older than the one the
// controller last used, for example after a restore from a backup.
//
// The store keeps a generation number that the controller raises; a copy of
// it lives in an annotation on the controller's Lease, which a restore of the
// store does not touch. The store is always written first, so a store in use
// is never behind its copy. A store behind its copy was replaced by an older
// one.
package storegen

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
)

// Annotation holds the copy of the store generation on the Lease.
const Annotation = "kubeneuron.io/store-generation"

const stateKind = "store_generation"

// Store is the part of the workflow store that holds the generation.
type Store interface {
	LoadSafetyState(ctx context.Context, kind string) ([]byte, error)
	SaveSafetyState(ctx context.Context, kind string, payload []byte) error
}

// Tracker compares and raises the store generation and its copy.
type Tracker struct {
	// mu serialises the read-then-write of Advance: the periodic raise and
	// the one after a backup run concurrently, and a late writer must not
	// put an older number into the store after a newer one reached the copy.
	mu     sync.Mutex
	store  Store
	leases coordinationclient.LeaseInterface
	name   string
	owner  *metav1.OwnerReference
}

// New returns a Tracker that keeps the copy on the named Lease. A non-nil
// owner is added to the Lease's owner references, so the copy is deleted
// with the installation and a new installation of the same name does not
// find it.
func New(store Store, leases coordinationclient.LeaseInterface, lease string, owner *metav1.OwnerReference) *Tracker {
	return &Tracker{store: store, leases: leases, name: lease, owner: owner}
}

// ErrNoCopy means the copy could not be read, so nothing can be said about
// the store.
var ErrNoCopy = errors.New("store generation copy is unavailable")

// errOtherInstallation marks a copy owned by another installation of the
// same name, whose Lease garbage collection has not removed yet.
var errOtherInstallation = fmt.Errorf("%w: the Lease belongs to an earlier installation", ErrNoCopy)

// Older reports whether the store is behind its copy. A missing copy (first
// installation, an upgrade from a version without it, a deleted Lease) is not
// older. An unreadable copy returns ErrNoCopy.
func (t *Tracker) Older(ctx context.Context) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stored, err := t.stored(ctx)
	if err != nil {
		return false, err
	}
	copied, ok, err := t.copied(ctx)
	if err != nil {
		return false, err
	}
	return ok && stored < copied, nil
}

// Advance raises the generation past both the store and its copy, writing
// the store first and then the copy.
func (t *Tracker) Advance(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	stored, err := t.stored(ctx)
	if err != nil {
		return err
	}
	copied, _, err := t.copied(ctx)
	if errors.Is(err, errOtherInstallation) {
		copied, err = 0, nil
	}
	if err != nil {
		return err
	}
	next := max(stored, copied) + 1
	if err := t.store.SaveSafetyState(ctx, stateKind, []byte(strconv.FormatUint(next, 10))); err != nil {
		return fmt.Errorf("save store generation: %w", err)
	}
	// The leader election client updates the same Lease, so a write can
	// meet a newer version once; read it again and retry.
	for attempt := 0; ; attempt++ {
		err = t.writeCopy(ctx, next)
		if !apierrors.IsConflict(err) || attempt == 2 {
			return err
		}
	}
}

func (t *Tracker) stored(ctx context.Context) (uint64, error) {
	payload, err := t.store.LoadSafetyState(ctx, stateKind)
	if err != nil {
		return 0, fmt.Errorf("load store generation: %w", err)
	}
	if payload == nil {
		return 0, nil
	}
	n, err := strconv.ParseUint(string(payload), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode store generation: %w", err)
	}
	return n, nil
}

func (t *Tracker) copied(ctx context.Context) (uint64, bool, error) {
	lease, err := t.leases.Get(ctx, t.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrNoCopy, err)
	}
	if t.owner != nil && t.ownedByOther(lease) {
		return 0, false, errOtherInstallation
	}
	value, ok := lease.Annotations[Annotation]
	if !ok {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: annotation %s=%q", ErrNoCopy, Annotation, value)
	}
	return n, true, nil
}

func (t *Tracker) writeCopy(ctx context.Context, n uint64) error {
	value := strconv.FormatUint(n, 10)
	lease, err := t.leases.Get(ctx, t.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Without leader election nobody else creates the Lease; it then
		// only carries the copy.
		created := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: t.name, Annotations: map[string]string{Annotation: value},
		}}
		t.own(created)
		_, err = t.leases.Create(ctx, created, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return apierrors.NewConflict(coordinationv1.Resource("leases"), t.name, err)
		}
		return err
	}
	if err != nil {
		return err
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[Annotation] = value
	t.own(lease)
	// Only the annotation and owner references change; the holder and renew
	// time are written back as read, and a renewal in between is a conflict.
	_, err = t.leases.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

// own makes this installation the Lease's KubeNeuron owner, replacing an
// earlier installation's reference: left beside it, that reference would
// let garbage collection delete the Lease of the new installation.
func (t *Tracker) own(lease *coordinationv1.Lease) {
	if t.owner == nil {
		return
	}
	refs := []metav1.OwnerReference{*t.owner}
	for _, ref := range lease.OwnerReferences {
		if !isInstallation(ref) {
			refs = append(refs, ref)
		}
	}
	lease.OwnerReferences = refs
}

func (t *Tracker) ownedByOther(lease *coordinationv1.Lease) bool {
	for _, ref := range lease.OwnerReferences {
		if isInstallation(ref) && ref.UID != t.owner.UID {
			return true
		}
	}
	return false
}

func isInstallation(ref metav1.OwnerReference) bool {
	return ref.Kind == "KubeNeuron" && strings.HasPrefix(ref.APIVersion, "kubeneuron.io/")
}
