// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

var _ platform.JournalLabeler = (*Platform)(nil)

// journalVariantComponents are the component labels of the agent Pods that
// mount a journal directory.
var journalVariantComponents = []string{"agent-detect-journal-var", "agent-detect-journal-run"}

// JournalNodes implements platform.JournalLabeler. The Pods are read with
// one label-selected list in the installation's namespace.
func (p *Platform) JournalNodes(ctx context.Context, namespace, installation string) ([]platform.JournalNode, error) {
	nodes, err := p.allNodes(ctx)
	if err != nil {
		return nil, err
	}
	selector := labels.SelectorFromSet(labels.Set{
		"app.kubernetes.io/name":     "kube-neuron",
		"app.kubernetes.io/instance": installation,
	}).String() + ",app.kubernetes.io/component in (" + journalVariantComponents[0] + "," + journalVariantComponents[1] + ")"
	pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	waiting := map[string]time.Time{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if since, ok := podWaitingToStart(pod); ok && pod.Spec.NodeName != "" {
			waiting[pod.Spec.NodeName] = since
		}
	}
	out := make([]platform.JournalNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, platform.JournalNode{
			Name:         n.Name,
			UID:          string(n.UID),
			OSImage:      n.Status.NodeInfo.OSImage,
			ProviderID:   n.Spec.ProviderID,
			Labels:       n.Labels,
			Dir:          n.Labels[kubeneuronv1alpha1.JournalDirLabel],
			Accelerator:  len(nodeDevices(n, true)) != 0,
			WaitingSince: waiting[n.Name],
		})
	}
	return out, nil
}

// podWaitingToStart reports whether a scheduled Pod has not yet started its
// container because the kubelet has not prepared it: the sandbox and its
// volumes are not ready (PodReadyToStartContainers=False), or, on a kubelet
// without that condition, the container still waits in ContainerCreating.
// An image pull is not this: the sandbox is ready by then.
func podWaitingToStart(pod *corev1.Pod) (time.Time, bool) {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodPending {
		return time.Time{}, false
	}
	since := pod.CreationTimestamp.Time
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			since = c.LastTransitionTime.Time
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReadyToStartContainers {
			return since, c.Status == corev1.ConditionFalse
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.State.Waiting != nil && s.State.Waiting.Reason == "ContainerCreating" {
			return since, true
		}
	}
	return time.Time{}, false
}

// SetJournalDir implements platform.JournalLabeler with a merge patch that
// touches only the one label.
func (p *Platform) SetJournalDir(ctx context.Context, node, dir string) error {
	var value any
	if dir != "" {
		value = dir
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]any{kubeneuronv1alpha1.JournalDirLabel: value}},
	})
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().Nodes().Patch(ctx, node, k8stypes.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
