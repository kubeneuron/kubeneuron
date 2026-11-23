// SPDX-License-Identifier: Apache-2.0
package controller

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/kubeneuron/kubeneuron/internal/inventory"
)

// The agent DaemonSet's arguments say how its agents read the GPUs, and so
// what an empty inventory means.
func TestInventoryDiagnosticFromTheAgentDaemonSet(t *testing.T) {
	for args, want := range map[string][]string{
		"RuntimeDidNotExposeGPUs":  {"--nvidia-observation", "--nvidia-runtime-devices"},
		"DeviceAccessDisabled":     {"--nvidia-observation"},
		"HostToolingNotConfigured": {"--nvidia-observation", "--enable-node-level-actions"},
		"":                         {"--require-real-driver", "--enable-node-level-actions"},
	} {
		t.Run(args, func(t *testing.T) {
			ds := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "fleet-agent", Namespace: "kube-neuron", OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: "fleet", UID: "uid-a", Controller: ptr.To(true)}}},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Args: want}}}},
				},
			}
			c := &Controller{}
			c.ConfigureInventoryDiagnostics(fake.NewClientset(ds), "kube-neuron", "fleet", "uid-a")
			got, err := c.inventoryDiagnostic(t.Context())
			if err != nil || got != args {
				t.Fatalf("diagnostic = %q, %v; want %q", got, err, args)
			}
		})
	}
}

// With a RuntimeClass the owner named, agent Pods that cannot be created, on
// a fresh install or in a rollout that stopped, or whose sandbox does not
// start, name it, and on how many nodes.
func TestInventoryDiagnosticNamesTheAgentRuntimeClass(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	fresh := metav1.NewTime(time.Now().Add(-30 * time.Second))
	pod := func(name, node string, class *string, created metav1.Time, sandbox corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-neuron", Labels: map[string]string{"app": "agent"}, CreationTimestamp: created,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "fleet-agent", UID: "uid-ds", Controller: ptr.To(true)}}},
			Spec:   corev1.PodSpec{NodeName: node, RuntimeClassName: class},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReadyToStartContainers, Status: sandbox}}},
		}
	}
	nvidia := ptr.To("nvidia")
	for _, tc := range []struct {
		name         string
		runtimeClass *string
		status       appsv1.DaemonSetStatus
		pods         []*corev1.Pod
		want         string
		nodes        int
	}{
		{"fresh install, no Pod created", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberUnavailable: 2}, nil, "AgentRuntimeClassPodsNotCreated", 0},
		// The field was added to a running installation: one old Pod went,
		// its successor was refused, the others still run.
		{"rollout stopped", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 2, NumberUnavailable: 1},
			[]*corev1.Pod{pod("a", "n1", nil, old, corev1.ConditionTrue), pod("b", "n2", nil, old, corev1.ConditionTrue)}, "AgentRuntimeClassPodsNotCreated", 0},
		// A Pod of the new template exists: the rollout is under way.
		{"rollout under way", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 3, NumberUnavailable: 1},
			[]*corev1.Pod{pod("a", "n1", nil, old, corev1.ConditionTrue), pod("c", "n3", nvidia, fresh, corev1.ConditionFalse)}, "", 0},
		{"sandbox does not start", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberUnavailable: 2},
			[]*corev1.Pod{pod("a", "n1", nvidia, old, corev1.ConditionFalse), pod("b", "n2", nvidia, old, corev1.ConditionFalse), pod("c", "n3", nvidia, old, corev1.ConditionTrue)},
			"AgentRuntimeClassNotStarting", 2},
		{"sandbox still starting", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberUnavailable: 1},
			[]*corev1.Pod{pod("a", "n1", nvidia, fresh, corev1.ConditionFalse)}, "", 0},
		{"running", nvidia, appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1},
			[]*corev1.Pod{pod("a", "n1", nvidia, old, corev1.ConditionTrue)}, "", 0},
		{"no runtime class", nil, appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberUnavailable: 2},
			[]*corev1.Pod{pod("a", "n1", nil, old, corev1.ConditionFalse)}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "fleet-agent", Namespace: "kube-neuron", UID: "uid-ds", OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "kubeneuron.io/v1alpha1", Kind: "KubeNeuron", Name: "fleet", UID: "uid-a", Controller: ptr.To(true)}}},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "agent"}},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RuntimeClassName: tc.runtimeClass,
						Containers: []corev1.Container{{Name: "agent", Args: []string{"--nvidia-observation", "--nvidia-runtime-devices"}}}}},
				},
				Status: tc.status,
			}
			objects := []runtime.Object{ds}
			for _, p := range tc.pods {
				objects = append(objects, p)
			}
			c := &Controller{}
			c.ConfigureInventoryDiagnostics(fake.NewClientset(objects...), "kube-neuron", "fleet", "uid-a")
			got, nodes, err := c.runtimeClassDiagnostic(t.Context())
			if err != nil || got != tc.want || nodes != tc.nodes {
				t.Fatalf("diagnostic = %q on %d nodes, %v; want %q on %d", got, nodes, err, tc.want, tc.nodes)
			}
			if got == "" {
				return
			}
			text := inventory.DiagnosticText(inventory.Summary{Diagnostic: got, DiagnosticNodes: nodes})
			if inventory.DiagnosticMessage(got) == "" || nodes > 0 && !strings.Contains(text, " on 2 nodes.") {
				t.Fatalf("%s reads %q", got, text)
			}
		})
	}
}
