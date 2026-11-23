// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
	storesqlite "github.com/kubeneuron/kubeneuron/internal/store/sqlite"
)

func firstAlertSummary(uid string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "fleet" + inventory.Suffix, Namespace: "kube-neuron",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: "fleet", UID: k8stypes.UID(uid), Controller: ptr.To(true)}},
	}}
}

func TestUnchangedInventorySkipsConfigMapUpdate(t *testing.T) {
	c, _ := newIngestTestController(t)
	kube := fake.NewClientset(firstAlertSummary("uid-a"))
	maps := kube.CoreV1().ConfigMaps("kube-neuron")
	publish := func() {
		t.Helper()
		if err := c.publishInventory(t.Context(), maps, "fleet", "uid-a"); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	cm, err := maps.Get(t.Context(), "fleet"+inventory.Suffix, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cm.Data["nodeLevelActions"]; got != "false" {
		t.Fatalf("nodeLevelActions = %q, want false", got)
	}
	kube.ClearActions()
	publish()
	for _, action := range kube.Actions() {
		if action.Matches("update", "configmaps") {
			t.Fatal("unchanged inventory updated its ConfigMap")
		}
	}
}

func TestFirstAlertAcceptedSurvivesInventoryPublishes(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	kube := fake.NewClientset(firstAlertSummary("uid-a"))
	maps := kube.CoreV1().ConfigMaps("kube-neuron")
	c := New(st, nil, nil, nil, nil, wholeDeviceTopologyPlatform(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ConfigureFirstAlertRecord(maps, "fleet", "uid-a")
	read := func() map[string]string {
		t.Helper()
		cm, err := maps.Get(ctx, "fleet"+inventory.Suffix, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return cm.Data
	}

	if err := c.publishInventory(ctx, maps, "fleet", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := inventory.FirstAlert(read()); ok {
		t.Fatal("no alert accepted yet, but the summary has a first alert")
	}
	if err := c.RecordAlertAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok := inventory.FirstAlert(read())
	if !ok || read()["schemaVersion"] != "1" {
		t.Fatalf("after the first alert: %v", read())
	}

	// A second alert, also from a fresh controller that has not cached the
	// record, keeps the first date.
	if err := c.RecordAlertAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	again := New(st, nil, nil, nil, nil, wholeDeviceTopologyPlatform(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	again.ConfigureFirstAlertRecord(maps, "fleet", "uid-a")
	if err := again.RecordAlertAccepted(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := inventory.FirstAlert(read()); !got.Equal(first) {
		t.Fatalf("second alert moved the first date: %v, want %v", got, first)
	}

	// Republishing the inventory replaces its own keys only: a stale
	// optional key goes, the first date stays.
	cm, _ := maps.Get(ctx, "fleet"+inventory.Suffix, metav1.GetOptions{})
	cm.Data["diagnostic"] = "SMIMissing"
	cm.Data["observedAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := maps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.publishInventory(ctx, maps, "fleet", "uid-a"); err != nil {
		t.Fatal(err)
	}
	data := read()
	if got, _ := inventory.FirstAlert(data); !got.Equal(first) {
		t.Fatalf("publish lost the first date: %v", data)
	}
	if _, stale := data["diagnostic"]; stale {
		t.Fatalf("publish kept a stale inventory key: %v", data)
	}
}

func TestFirstAlertRecordRetriesConflictAndChecksOwner(t *testing.T) {
	ctx := t.Context()
	kube := fake.NewClientset(firstAlertSummary("uid-a"))
	conflicts := 1
	kube.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "fleet"+inventory.Suffix, nil)
		}
		return false, nil, nil
	})
	maps := kube.CoreV1().ConfigMaps("kube-neuron")
	c := &Controller{}
	c.ConfigureFirstAlertRecord(maps, "fleet", "uid-a")
	if err := c.RecordAlertAccepted(ctx); err != nil {
		t.Fatalf("a conflict is retried: %v", err)
	}
	cm, _ := maps.Get(ctx, "fleet"+inventory.Suffix, metav1.GetOptions{})
	if _, ok := inventory.FirstAlert(cm.Data); !ok {
		t.Fatalf("first alert not recorded after a conflict: %v", cm.Data)
	}

	other := &Controller{}
	other.ConfigureFirstAlertRecord(fake.NewClientset(firstAlertSummary("uid-b")).CoreV1().ConfigMaps("kube-neuron"), "fleet", "uid-a")
	if err := other.RecordAlertAccepted(ctx); err == nil {
		t.Fatal("wrote the first alert into another installation's summary")
	}
}
