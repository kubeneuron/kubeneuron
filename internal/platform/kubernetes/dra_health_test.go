// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubeneuron/kubeneuron/internal/platform"
)

func taintedSlice(name, node, driver string, devices ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(devices))
	for _, d := range devices {
		list = append(list, d)
	}
	spec := map[string]any{"driver": driver, "pool": map[string]any{"name": node, "generation": int64(1), "resourceSliceCount": int64(1)}, "devices": list}
	if node != "" {
		spec["nodeName"] = node
	} else {
		spec["allNodes"] = true
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.k8s.io/v1", "kind": "ResourceSlice", "metadata": map[string]any{"name": name}, "spec": spec}}
}

func draHealthPlatform(nodes []string, objects ...runtime.Object) (*Platform, *dynamicfake.FakeDynamicClient) {
	core := make([]runtime.Object, 0, len(nodes))
	for _, n := range nodes {
		core = append(core, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n, UID: types.UID("uid-" + n)}})
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{draResourceSlices: "ResourceSliceList"}, objects...)
	return &Platform{client: fake.NewSimpleClientset(core...), dynamic: dyn}, dyn
}

func TestDRAHealthReadsTaintsOfExistingNodes(t *testing.T) {
	p, dyn := draHealthPlatform([]string{"n1"},
		taintedSlice("n1-gpu", "n1", "gpu.nvidia.com",
			map[string]any{"name": "gpu-0", "attributes": map[string]any{"uuid": map[string]any{"string": "GPU-0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"}},
				"taints": []any{map[string]any{"key": "gpu.nvidia.com/xid", "value": "79", "effect": "NoSchedule", "timeAdded": "2026-11-18T11:00:00Z"}}},
			map[string]any{"name": "gpu-1"}),
		// A node that does not exist, and a slice not bound to one node.
		taintedSlice("gone-gpu", "gone", "gpu.nvidia.com", map[string]any{"name": "gpu-0"}),
		taintedSlice("shared", "", "gpu.nvidia.com", map[string]any{"name": "x"}),
	)
	got, err := p.DRAHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "n1" || len(got[0].DRADevices) != 2 {
		t.Fatalf("snapshots %+v, want n1 with two devices", got)
	}
	d := got[0].DRADevices[0]
	if d.Driver != "gpu.nvidia.com" || d.Pool != "n1" || d.Device != "gpu-0" || d.UUID == "" || len(d.Taints) != 1 {
		t.Fatalf("device %+v", d)
	}
	if tt := d.Taints[0]; tt.Key != "gpu.nvidia.com/xid" || tt.Value != "79" || tt.Effect != "NoSchedule" || !tt.TimeAdded.Equal(time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("taint %+v", tt)
	}
	for _, a := range dyn.Actions() {
		if a.GetVerb() != "list" || a.GetResource() != draResourceSlices {
			t.Fatalf("unexpected dynamic action %s %v", a.GetVerb(), a.GetResource())
		}
	}
}

// A cluster that serves no resource.k8s.io/v1 ResourceSlices (before 1.34,
// or a beta version only) or refuses the read is a read error with a reason,
// never a crash and never an empty, healthy-looking answer.
func TestDRAHealthUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"not served": {apierrors.NewNotFound(draResourceSlices.GroupResource(), ""), draReasonAPIUnavailable},
		"forbidden":  {apierrors.NewForbidden(draResourceSlices.GroupResource(), "", errors.New("no")), draReasonForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			p, dyn := draHealthPlatform([]string{"n1"})
			dyn.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			got, err := p.DRAHealth(context.Background())
			var derr *platform.DRAHealthError
			if got != nil || !errors.As(err, &derr) || derr.Reason != tc.reason {
				t.Fatalf("%+v, %v: want a %s read error", got, err, tc.reason)
			}
		})
	}
	if _, err := (&Platform{}).DRAHealth(context.Background()); err == nil {
		t.Fatal("no dynamic client read as a success")
	}
}

// With the gate off, the API server drops the taints: the devices are read
// with none, which the profiles read as no signal, not as health.
func TestDRAHealthWithoutTaints(t *testing.T) {
	p, _ := draHealthPlatform([]string{"n1"}, taintedSlice("n1-gpu", "n1", "gpu.nvidia.com", map[string]any{"name": "gpu-0"}))
	got, err := p.DRAHealth(context.Background())
	if err != nil || len(got) != 1 || len(got[0].DRADevices[0].Taints) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}
