// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestZoneConstraintsSharedVolume(t *testing.T) {
	claim := &core.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "ns"}, Spec: core.PersistentVolumeClaimSpec{VolumeName: "vol"}}
	zone := core.NodeSelectorTerm{MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelTopologyZone, Operator: core.NodeSelectorOpIn, Values: []string{"a"}}}}
	selector := &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{zone}}
	pv := &core.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "vol"}, Spec: core.PersistentVolumeSpec{NodeAffinity: &core.VolumeNodeAffinity{Required: selector}}}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "one", Namespace: "ns"}, Spec: core.PodSpec{NodeName: "node", Volumes: []core.Volume{{VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}}}}}
	other := pod.DeepCopy()
	other.Name = "two"
	for _, tc := range []struct {
		name, key     string
		alternative   bool
		pods, volumes int
	}{{"stable", core.LabelTopologyZone, false, 2, 1}, {"legacy", core.LabelFailureDomainBetaZone, false, 2, 1}, {"azure", "topology.disk.csi.azure.com/zone", false, 2, 1}, {"ebs", "topology.ebs.csi.aws.com/zone", false, 2, 1}, {"alternative zone", core.LabelTopologyZone, true, 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			volume := pv.DeepCopy()
			volume.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Key = tc.key
			if tc.alternative {
				other.Spec.NodeSelector = map[string]string{core.LabelFailureDomainBetaZone: "a"}
				alt := zone.DeepCopy()
				alt.MatchExpressions[0].Values = []string{"b"}
				volume.Spec.NodeAffinity.Required.NodeSelectorTerms = append(volume.Spec.NodeAffinity.Required.NodeSelectorTerms, *alt)
			}
			client := fake.NewSimpleClientset(pod, other, claim, volume)
			p := Platform{client: client}
			pods, vols, err := p.NodeZoneConstraints(context.Background(), "node", "a")
			if err != nil || pods != tc.pods || vols != tc.volumes {
				t.Fatalf("pods=%d volumes=%d err=%v", pods, vols, err)
			}
			if len(client.Actions()) != 3 {
				t.Fatalf("expected one pod list and two object reads, got %v", client.Actions())
			}
		})
	}
}
