// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// healthRecycler is a provider that reads accelerator health.
type healthRecycler struct {
	fakeRecycler
	enabled bool
	points  map[string][]cloud.HealthPoint
	err     error
	asked   [][]string
}

func (h *healthRecycler) AcceleratorHealthEnabled() bool { return h.enabled }
func (h *healthRecycler) AcceleratorHealth(_ context.Context, ids []string) (map[string][]cloud.HealthPoint, error) {
	h.asked = append(h.asked, ids)
	return h.points, h.err
}

func deviceNode(name, providerID string, resourceName corev1.ResourceName) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{resourceName: resource.MustParse("4")},
			Allocatable: corev1.ResourceList{resourceName: resource.MustParse("4")},
		},
	}
}

func TestCloudAcceleratorHealthReadsTPUNodesOnly(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	rec := &healthRecycler{enabled: true, points: map[string][]cloud.HealthPoint{"tpu-vm-1": {{At: at, Status: "HEALTHY"}}}}
	p := &Platform{client: fake.NewSimpleClientset(
		deviceNode("tpu-1", "gce://proj-a/us-central2-b/tpu-vm-1", "google.com/tpu"),
		deviceNode("gpu-1", "gce://proj-a/us-central2-b/gpu-vm-1", "nvidia.com/gpu"),
	), recycler: rec}
	if !p.CloudAcceleratorHealthEnabled() {
		t.Fatal("not enabled")
	}
	got, err := p.CloudAcceleratorHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node != "tpu-1" || len(got[0].Cloud) != 1 || got[0].Cloud[0].Status != "HEALTHY" {
		t.Fatalf("snapshots %+v, want tpu-1 with its point", got)
	}
	if got[0].CreatedAt.IsZero() || got[0].ProviderID == "" {
		t.Errorf("snapshot lacks the node's creation time or provider ID: %+v", got[0])
	}
	if len(rec.asked) != 1 || len(rec.asked[0]) != 1 || rec.asked[0][0] != "tpu-vm-1" {
		t.Errorf("asked about %v, want the TPU node's instance only", rec.asked)
	}
}

func TestCloudAcceleratorHealthAsksNothingWithoutTPUNodes(t *testing.T) {
	rec := &healthRecycler{enabled: true}
	p := &Platform{client: fake.NewSimpleClientset(deviceNode("gpu-1", "gce://proj-a/us-central2-b/gpu-vm-1", "nvidia.com/gpu")), recycler: rec}
	got, err := p.CloudAcceleratorHealth(context.Background())
	if got != nil || err != nil || len(rec.asked) != 0 {
		t.Fatalf("%+v, %v, asked %v: without a TPU node the cloud is not asked", got, err, rec.asked)
	}
	off := &healthRecycler{}
	p = &Platform{client: fake.NewSimpleClientset(deviceNode("tpu-1", "gce://proj-a/us-central2-b/tpu-vm-1", "google.com/tpu")), recycler: off}
	if p.CloudAcceleratorHealthEnabled() {
		t.Fatal("enabled while the provider has it off")
	}
	if got, err := p.CloudAcceleratorHealth(context.Background()); got != nil || err != nil || len(off.asked) != 0 {
		t.Fatalf("%+v, %v, asked %v: turned off, the cloud is not asked", got, err, off.asked)
	}
}

func TestCloudAcceleratorHealthFailureMarksEveryNode(t *testing.T) {
	rec := &healthRecycler{enabled: true, err: &cloud.HealthReadError{Reason: cloud.HealthReadPermissionDenied, Err: errors.New("403")}}
	p := &Platform{client: fake.NewSimpleClientset(
		deviceNode("tpu-1", "gce://proj-a/us-central2-b/tpu-vm-1", "google.com/tpu"),
		deviceNode("tpu-2", "gce://proj-a/us-central2-b/tpu-vm-2", "google.com/tpu"),
	), recycler: rec}
	got, err := p.CloudAcceleratorHealth(context.Background())
	if err == nil || len(got) != 2 {
		t.Fatalf("%+v, %v: want both nodes and the error", got, err)
	}
	for _, s := range got {
		if s.CloudError != cloud.HealthReadPermissionDenied || len(s.Cloud) != 0 {
			t.Errorf("%s: error %q points %v", s.Node, s.CloudError, s.Cloud)
		}
	}
}
