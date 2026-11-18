// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"strings"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

func TestControllerGCPWiring(t *testing.T) {
	installation := testKubeNeuron()
	installation.UID = "installation-a"
	installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: "gcp", GCP: &kubeneuronv1alpha1.GCPCloudSpec{
		Project: "ml-project", Location: "us-central1", ClusterName: "training",
	}}
	deployment, err := controllerDeployment(installation, &Snapshot{Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"KUBENEURON_GKE_PROJECT":  "ml-project",
		"KUBENEURON_GKE_LOCATION": "us-central1",
		"KUBENEURON_GKE_CLUSTER":  "training",
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
	if !containsString(deployment.Spec.Template.Spec.Containers[0].Args, "--cloud-provider=gcp") {
		t.Fatalf("controller args miss the provider")
	}
	// Workload Identity Federation for GKE needs no annotation or pod label,
	// and no node events are read.
	if sa := controllerServiceAccount(installation); len(sa.Annotations) != 0 {
		t.Fatalf("SA annotations = %v", sa.Annotations)
	}
	if rules := nodeEventsRules(installation); rules != nil {
		t.Fatalf("node event rules = %v", rules)
	}
	condition := cloudRecoveryCondition(installation)
	if condition.Reason != "NotReadyOnlyRepair" || !strings.Contains(condition.Message, "experimental, not validated against a live GKE cluster; start in DryRun") {
		t.Fatalf("condition = %+v", condition)
	}
}
