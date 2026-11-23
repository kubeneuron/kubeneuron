// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func journalPod(name, node, component string, phase corev1.PodPhase, conditions ...corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-neuron-system", Labels: map[string]string{
			"app.kubernetes.io/name": "kube-neuron", "app.kubernetes.io/instance": "fleet", "app.kubernetes.io/component": component,
		}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase, Conditions: conditions},
	}
}

func TestJournalNodesReadsWaitingVariantPods(t *testing.T) {
	scheduled := metav1.NewTime(time.Now().Add(-5 * time.Minute).Truncate(time.Second))
	node := func(name, label string) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID("uid-" + name), Labels: map[string]string{}},
			Spec:   corev1.NodeSpec{ProviderID: "kind://docker/c/" + name},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OSImage: "Debian GNU/Linux 12 (bookworm)"}}}
		if label != "" {
			n.Labels["kubeneuron.io/journal-dir"] = label
		}
		if name != "cpu" {
			n.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		}
		return n
	}
	notReady := corev1.PodCondition{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse}
	p := &Platform{client: fake.NewSimpleClientset(
		node("stuck", "run"), node("running", "run"), node("other", ""), node("cpu", ""),
		journalPod("a", "stuck", "agent-detect-journal-run", corev1.PodPending,
			corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: scheduled}, notReady),
		journalPod("b", "running", "agent-detect-journal-run", corev1.PodRunning),
		// Not a journal variant: the primary agent waiting does not count.
		journalPod("c", "other", "agent", corev1.PodPending, notReady),
	)}
	nodes, err := p.JournalNodes(context.Background(), "kube-neuron-system", "fleet")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, n := range nodes {
		byName[n.Name] = i
	}
	stuck := nodes[byName["stuck"]]
	if !stuck.WaitingSince.Equal(scheduled.Time) || stuck.Dir != "run" || !stuck.Accelerator || stuck.ProviderID != "kind://docker/c/stuck" || stuck.UID != "uid-stuck" {
		t.Fatalf("stuck = %+v", stuck)
	}
	for _, name := range []string{"running", "other", "cpu"} {
		if n := nodes[byName[name]]; !n.WaitingSince.IsZero() {
			t.Errorf("%s waits since %v", name, n.WaitingSince)
		}
	}
	if nodes[byName["cpu"]].Accelerator {
		t.Error("cpu node is an accelerator node")
	}
}

func TestPodWaitingToStartIgnoresAnImagePull(t *testing.T) {
	pulling := journalPod("p", "n", "agent-detect-journal-var", corev1.PodPending,
		corev1.PodCondition{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue})
	pulling.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	if _, ok := podWaitingToStart(pulling); ok {
		t.Fatal("an image pull counted as a mount wait")
	}
	old := journalPod("o", "n", "agent-detect-journal-var", corev1.PodPending)
	old.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
	if _, ok := podWaitingToStart(old); !ok {
		t.Fatal("ContainerCreating without the condition did not count")
	}
}

func TestSetJournalDirTouchesOnlyItsLabel(t *testing.T) {
	p := &Platform{client: fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{"keep": "me"}}})}
	ctx := context.Background()
	if err := p.SetJournalDir(ctx, "n", "var"); err != nil {
		t.Fatal(err)
	}
	n, _ := p.client.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
	if n.Labels["kubeneuron.io/journal-dir"] != "var" || n.Labels["keep"] != "me" {
		t.Fatalf("labels = %v", n.Labels)
	}
	if err := p.SetJournalDir(ctx, "n", ""); err != nil {
		t.Fatal(err)
	}
	n, _ = p.client.CoreV1().Nodes().Get(ctx, "n", metav1.GetOptions{})
	if _, ok := n.Labels["kubeneuron.io/journal-dir"]; ok || n.Labels["keep"] != "me" {
		t.Fatalf("labels = %v", n.Labels)
	}
}
