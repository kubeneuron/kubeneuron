package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func stackNode(labels map[string]string, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "gpu-1", Labels: labels, Annotations: annotations,
	}}
}

func TestQuiesceStopsOnlyTheComponentsThatWereRunning(t *testing.T) {
	node := stackNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm":          "true",
		"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		// The cluster deliberately runs no device plugin from the operator.
		"nvidia.com/gpu.deploy.device-plugin": "false",
	}, nil)
	p := &Platform{client: fake.NewSimpleClientset(node)}
	ctx := context.Background()

	quiesced, err := p.QuiesceAcceleratorStack(ctx, "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(quiesced) != 2 {
		t.Fatalf("quiesced = %v, want only the two running components", quiesced)
	}

	got, err := p.client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["nvidia.com/gpu.deploy.dcgm"] != "false" {
		t.Fatalf("dcgm label = %q, want false", got.Labels["nvidia.com/gpu.deploy.dcgm"])
	}
	if got.Labels["nvidia.com/gpu.deploy.device-plugin"] != "false" {
		t.Fatal("a component that was already off must be left exactly as it was")
	}
	if got.Annotations[acceleratorStackQuiescedAnnotation] != "dcgm,dcgm-exporter" {
		t.Fatalf("record = %q, want only what we switched off",
			got.Annotations[acceleratorStackQuiescedAnnotation])
	}
	if got.Annotations[acceleratorHostQuiescedAnnotation] != "true" {
		t.Fatal("host quiesce marker must survive until host restoration succeeds")
	}

	// Restoring must not enable the device plugin the cluster had switched off.
	restored, err := p.RestoreAcceleratorStack(ctx, "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 {
		t.Fatalf("restored = %v, want the two we stopped", restored)
	}
	got, err = p.client.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["nvidia.com/gpu.deploy.dcgm"] != "true" || got.Labels["nvidia.com/gpu.deploy.dcgm-exporter"] != "true" {
		t.Fatalf("labels after restore = %v", got.Labels)
	}
	if got.Labels["nvidia.com/gpu.deploy.device-plugin"] != "false" {
		t.Fatal("restore turned on a component KubeNeuron never turned off")
	}
	if got.Annotations[acceleratorStackQuiescedAnnotation] != "" {
		t.Fatal("the record must be cleared once everything is back")
	}
	if got.Annotations[acceleratorHostQuiescedAnnotation] != "" {
		t.Fatal("the host marker must be cleared with the completed restore")
	}
}

func TestQuiesceRecordsHostRecoveryEvenWithoutOperatorComponents(t *testing.T) {
	p := &Platform{client: fake.NewSimpleClientset(stackNode(nil, nil))}
	if _, err := p.QuiesceAcceleratorStack(context.Background(), "gpu-1"); err != nil {
		t.Fatal(err)
	}
	nodes, err := p.QuiescedNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != "gpu-1" {
		t.Fatalf("quiesced nodes = %v, want host-only recovery marker", nodes)
	}
	if _, err := p.RestoreAcceleratorStack(context.Background(), "gpu-1"); err != nil {
		t.Fatal(err)
	}
	nodes, err = p.QuiescedNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("quiesced nodes after restore = %v", nodes)
	}
}

func TestRestoreIsSafeOnANodeThatWasNeverQuiesced(t *testing.T) {
	p := &Platform{client: fake.NewSimpleClientset(stackNode(map[string]string{
		"nvidia.com/gpu.deploy.dcgm": "true",
	}, nil))}
	restored, err := p.RestoreAcceleratorStack(context.Background(), "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing", restored)
	}
}
