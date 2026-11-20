// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	core "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/yaml"
)

// CheckAutoscalerGroup reads a fresh status for each check, including the final
// check before a resize. A missing status still requires the EKS declaration.
func CheckAutoscalerGroup(ctx context.Context, maps core.ConfigMapInterface, group string) (bool, error) {
	cm, err := maps.Get(ctx, "cluster-autoscaler-status", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var report struct {
		AutoscalerStatus string
		ClusterWide      struct {
			Health struct{ LastProbeTime time.Time }
		}
		NodeGroups []struct{ Name string }
	}
	if err := yaml.Unmarshal([]byte(cm.Data["status"]), &report); err != nil {
		return false, errors.New("invalid Cluster Autoscaler status")
	}
	age := time.Since(report.ClusterWide.Health.LastProbeTime)
	if report.AutoscalerStatus != "Running" || age < 0 || age > 5*time.Minute || report.NodeGroups == nil {
		return false, errors.New("stale or incomplete Cluster Autoscaler status")
	}
	matched := false
	for _, entry := range report.NodeGroups {
		if entry.Name == "" {
			return false, errors.New("unnamed group in Cluster Autoscaler status")
		}
		if entry.Name == group {
			matched = true
		}
	}
	return matched, nil
}
