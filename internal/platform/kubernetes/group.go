// SPDX-License-Identifier: LicenseRef-KubeNeuron

package kubernetes

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeneuron/kubeneuron/internal/checkpoint"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// groupListPage is the page size of the bounded namespace read behind
// ListGroupMembers, and groupReadCap the most Pods it will ever scan for one
// group. A group's membership is an annotation, which no server-side selector
// can match, so the read is a paged namespace list filtered client-side and
// capped: past the cap the group is reported truncated, which the controller
// treats as too large to coordinate.
const (
	groupListPage = 500
	groupReadCap  = 4000
)

// ListGroupMembers implements platform.WorkloadCheckpointer.
func (p *Platform) ListGroupMembers(ctx context.Context, policy checkpoint.Policy, namespace, group string, limit int) ([]platform.Workload, bool, error) {
	if namespace == "" || group == "" {
		return nil, false, fmt.Errorf("%w: group listing needs a namespace and a group", platform.ErrCheckpointScope)
	}
	if !policy.AllowsNamespace(namespace) {
		return nil, false, fmt.Errorf("%w: namespace %q is not on the checkpoint allowlist", platform.ErrCheckpointScope, namespace)
	}
	if limit <= 0 {
		limit = checkpoint.DefaultMaxGroupSize
	}
	var (
		members   []platform.Workload
		scanned   int
		continue_ string
	)
	for {
		list, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: groupListPage, Continue: continue_})
		if err != nil {
			return nil, false, fmt.Errorf("checkpoint group %s/%s: listing pods: %w", namespace, group, err)
		}
		for i := range list.Items {
			pod := &list.Items[i]
			scanned++
			if pod.Annotations[checkpoint.AnnotationGroup] != group || podTerminal(pod) {
				continue
			}
			if len(members) >= limit {
				return members, true, nil
			}
			members = append(members, platform.Workload{
				Name: pod.Name, Namespace: pod.Namespace, Kind: "Pod",
				UsesGPU: podUsesGPU(pod), DrainExclusion: drainExclusion(pod),
				UID: string(pod.UID), Annotations: copyAnnotations(pod.Annotations), Node: pod.Spec.NodeName,
			})
		}
		if scanned >= groupReadCap {
			return members, true, nil
		}
		continue_ = list.Continue
		if continue_ == "" {
			return members, false, nil
		}
	}
}
