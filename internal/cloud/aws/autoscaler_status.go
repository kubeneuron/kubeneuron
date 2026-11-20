// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"

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
	return autoscalerGroupStatus(cm.Data["status"], group, time.Now())
}

func autoscalerGroupStatus(status, group string, now time.Time) (bool, error) {
	if strings.HasPrefix(status, "Cluster-autoscaler status at ") {
		return false, &cloud.PoolRefusal{
			Code:   "legacy-autoscaler-status",
			Reason: "Cluster Autoscaler publishes a legacy text status report",
			Action: "upgrade Cluster Autoscaler to a version that publishes YAML or use RemoveFirst",
		}
	}
	var report struct {
		AutoscalerStatus string
		ClusterWide      struct {
			Health struct{ LastProbeTime time.Time }
		}
		NodeGroups []struct{ Name string }
	}
	// CA omits nodeGroups when it manages zero groups. An explicit null is malformed.
	report.NodeGroups = []struct{ Name string }{}
	if err := yaml.Unmarshal([]byte(status), &report); err != nil {
		return false, errors.New("invalid Cluster Autoscaler status")
	}
	age := now.Sub(report.ClusterWide.Health.LastProbeTime)
	if report.AutoscalerStatus != "Running" || age < -time.Minute || age > 5*time.Minute || report.NodeGroups == nil {
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
