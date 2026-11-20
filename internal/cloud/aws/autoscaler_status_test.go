// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestAutoscalerGroupStatus(t *testing.T) {
	fresh := time.Now().UTC().Format(time.RFC3339)
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name, timestamp, groups string
		managed, wantErr        bool
	}{
		{"actual backing ASG", fresh, "[{name: workers}]", true, false},
		{"different ASG", fresh, "[{name: workers-2}]", false, false},
		{"no groups", fresh, "[]", false, false},
		{"expired status", stale, "[{name: workers}]", false, true},
		{"incomplete status", fresh, "null", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := fmt.Sprintf("autoscalerStatus: Running\nclusterWide:\n  health:\n    lastProbeTime: %q\nnodeGroups: %s\n", tc.timestamp, tc.groups)
			cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: "kube-system"}, Data: map[string]string{"status": status}}
			maps := fake.NewSimpleClientset(cm).CoreV1().ConfigMaps("kube-system")
			got, err := CheckAutoscalerGroup(t.Context(), maps, "workers")
			if got != tc.managed || (err != nil) != tc.wantErr {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	client := fake.NewSimpleClientset()
	maps := client.CoreV1().ConfigMaps("kube-system")
	if got, err := CheckAutoscalerGroup(t.Context(), maps, "workers"); got || err != nil {
		t.Fatalf("absent status: %v, %v", got, err)
	}
	client.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cluster-autoscaler-status", errors.New("access denied"))
	})
	if _, err := CheckAutoscalerGroup(t.Context(), maps, "workers"); !apierrors.IsForbidden(err) {
		t.Fatalf("denied status: %v", err)
	}
}

func TestUpstreamAutoscalerStatus(t *testing.T) {
	// Fixtures use cluster-autoscaler-1.32.0's clusterstate/utils/status_test.yaml
	// marshaled with its ClusterAutoscalerStatus and yaml.v2, as in WriteStatusConfigMap.
	// The empty case uses the same upstream writer with no node groups.
	probe := time.Date(2023, 11, 24, 4, 28, 19, 48988, time.UTC)
	for _, tc := range []struct {
		name             string
		age              time.Duration
		managed, wantErr bool
	}{
		{"running", 0, true, false}, {"empty", 0, false, false}, {"running", -time.Minute, true, false},
		{"running", -time.Minute - time.Second, false, true}, {"running", 5 * time.Minute, true, false},
		{"running", 5*time.Minute + time.Second, false, true},
	} {
		b, err := os.ReadFile("testdata/ca-1.32-" + tc.name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		got, err := autoscalerGroupStatus(string(b), "sample-node-group", probe.Add(tc.age))
		if got != tc.managed || (err != nil) != tc.wantErr {
			t.Errorf("%s age %s: got %v, %v", tc.name, tc.age, got, err)
		}
	}
	// WriteStatusConfigMap in cluster-autoscaler-1.28.0 writes this header even during initialization.
	_, err := autoscalerGroupStatus("Cluster-autoscaler status at 2023-11-24 04:28:19 +0000 UTC:\nInitializing", "sample-node-group", probe)
	if err == nil || !strings.Contains(err.Error(), "use RemoveFirst") {
		t.Fatalf("legacy status: %v", err)
	}
}
