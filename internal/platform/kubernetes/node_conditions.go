// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeConditions reads one coherent view directly from the API at decision time.
func (p *Platform) NodeConditions(ctx context.Context, name string) (*types.NodeConditionSnapshot, error) {
	node, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	snapshot := &types.NodeConditionSnapshot{NodeUID: string(node.UID), ObservedAt: time.Now().UTC()}
	for _, kind := range []corev1.NodeConditionType{corev1.NodeReady, corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable} {
		obs := types.NodeConditionObservation{Type: string(kind), Status: string(corev1.ConditionUnknown)}
		for _, condition := range node.Status.Conditions {
			if condition.Type != kind {
				continue
			}
			switch condition.Status {
			case corev1.ConditionFalse, corev1.ConditionTrue:
				obs.Status = string(condition.Status)
			}
			if !condition.LastTransitionTime.IsZero() {
				ts := condition.LastTransitionTime.UTC()
				obs.LastTransitionTime = &ts
			}
			break
		}
		snapshot.Conditions = append(snapshot.Conditions, obs)
	}
	return snapshot, nil
}
