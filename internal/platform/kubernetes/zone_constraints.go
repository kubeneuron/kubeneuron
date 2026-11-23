// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"slices"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// requiresZone reports a positive restriction to the original zone. Terms are
// alternatives; a term without a zone restriction can allow another zone.
func requiresZone(selector *core.NodeSelector, zone string, keys ...string) bool {
	if selector == nil || len(selector.NodeSelectorTerms) == 0 || zone == "" {
		return false
	}
	for _, term := range selector.NodeSelectorTerms {
		var allowed []string
		restricted := false
		for _, expr := range term.MatchExpressions {
			if !slices.Contains(keys, expr.Key) || expr.Operator != core.NodeSelectorOpIn {
				continue
			}
			if !restricted {
				allowed = slices.Clone(expr.Values)
				restricted = true
				continue
			}
			allowed = slices.DeleteFunc(allowed, func(value string) bool { return !slices.Contains(expr.Values, value) })
		}
		if len(allowed) != 1 || allowed[0] != zone {
			return false
		}
	}
	return true
}

// NodeZoneConstraints counts distinct pods and bound persistent volumes.
// Shared claims and volumes are read once for this node snapshot.
func (p *Platform) NodeZoneConstraints(ctx context.Context, node, zone string) (int, int, error) {
	pods, err := p.nodePods(ctx, node)
	if err != nil {
		return 0, 0, err
	}
	claims := map[string]string{}
	volumes := map[string]bool{}
	podCount, volumeCount := 0, 0
	for _, pod := range pods {
		tied := zone != "" && (pod.Spec.NodeSelector[core.LabelTopologyZone] == zone || pod.Spec.NodeSelector[core.LabelFailureDomainBetaZone] == zone)
		if a := pod.Spec.Affinity; a != nil && a.NodeAffinity != nil {
			tied = tied || requiresZone(a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution, zone, core.LabelTopologyZone, core.LabelFailureDomainBetaZone)
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			name := v.PersistentVolumeClaim.ClaimName
			key := pod.Namespace + "/" + name
			pvName, ok := claims[key]
			if !ok {
				pvc, err := p.client.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return 0, 0, err
				}
				pvName = pvc.Spec.VolumeName
				claims[key] = pvName
			}
			if pvName == "" {
				continue
			}
			zonal, ok := volumes[pvName]
			if !ok {
				pv, err := p.client.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				if err != nil {
					return 0, 0, err
				}
				zonal = pv.Spec.NodeAffinity != nil && requiresZone(pv.Spec.NodeAffinity.Required, zone, core.LabelTopologyZone, core.LabelFailureDomainBetaZone, "topology.disk.csi.azure.com/zone", "topology.ebs.csi.aws.com/zone")
				volumes[pvName] = zonal
				if zonal {
					volumeCount++
				}
			}
			tied = tied || zonal
		}
		if tied {
			podCount++
		}
	}
	return podCount, volumeCount, nil
}
