// SPDX-License-Identifier: Apache-2.0
package operator

import (
	"strings"
	"testing"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	// the real provider declarations, as the operator binary registers them
	_ "github.com/kubeneuron/kubeneuron/internal/cloud/providers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCloudConfigurationIsNotProofOfRecoveryAvailability(t *testing.T) {
	installation := testKubeNeuron()
	installation.Generation = 7
	c := cloudRecoveryCondition(installation)
	if c.Status != metav1.ConditionFalse || c.Reason != "ProviderNotConfigured" || c.ObservedGeneration != 7 {
		t.Fatalf("missing provider: %+v", c)
	}
	installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: "aws"}
	c = cloudRecoveryCondition(installation)
	if c.Status != metav1.ConditionUnknown || c.Reason != "EligibilityNotEstablished" {
		t.Fatalf("configuration must not claim eligibility: %+v", c)
	}
}

func TestCloudPackExplainsDefaultReplacementAvailability(t *testing.T) {
	installation := testKubeNeuron()
	installation.Labels = map[string]string{"kubeneuron.io/policy-pack": "cloud"}
	for _, provider := range []string{"", "aws"} {
		if provider != "" {
			installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: provider}
		}
		condition := cloudRecoveryCondition(installation)
		if !strings.Contains(condition.Message, "observation and dry-run") || !strings.Contains(condition.Message, "add-first support") {
			t.Fatalf("provider %q: %s", provider, condition.Message)
		}
	}
}

// A fresh installation applies config/cloud-policies whatever its provider.
// The pack's optional recycle ladder needs reinitialize-in-place, which only
// some providers declare; no policy reaches it, so it must not make the
// installation invalid, and a policy that does reach it still must.
func TestCloudPackIsValidForEveryProvider(t *testing.T) {
	load := func() (*kubeneuronv1alpha1.KubeNeuron, []kubeneuronv1alpha1.GPUPlaybook, []kubeneuronv1alpha1.GPURemediationPolicy) {
		playbooks, policies := loadPack(t, "../../config/cloud-policies")
		installation := testKubeNeuron()
		installation.Name = packInstallation
		installation.Spec.Safety.NodeLevelActions = false // the installation default
		return installation, playbooks, policies
	}
	for _, provider := range []string{"aws", "azure", "gcp"} {
		t.Run(provider, func(t *testing.T) {
			installation, playbooks, policies := load()
			installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: provider}
			if _, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil); err != nil {
				t.Fatalf("the cloud pack is invalid on %s: %v", provider, err)
			}
		})
	}
	const recycle = "kubeneuron-cloud-recycle"
	for _, tc := range []struct {
		name string
		bind func([]kubeneuronv1alpha1.GPUPlaybook, []kubeneuronv1alpha1.GPURemediationPolicy)
	}{
		{"a policy names the recycle ladder", func(_ []kubeneuronv1alpha1.GPUPlaybook, policies []kubeneuronv1alpha1.GPURemediationPolicy) {
			policies[0].Spec.PlaybookRef = recycle
		}},
		{"a policy reaches it by escalation", func(playbooks []kubeneuronv1alpha1.GPUPlaybook, policies []kubeneuronv1alpha1.GPURemediationPolicy) {
			for i := range playbooks {
				if playbooks[i].Name == policies[0].Spec.PlaybookRef {
					playbooks[i].Spec.OnFailure = &kubeneuronv1alpha1.PlaybookFailure{PlaybookRef: recycle}
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installation, playbooks, policies := load()
			installation.Spec.Cloud = &kubeneuronv1alpha1.CloudSpec{Provider: "azure"}
			tc.bind(playbooks, policies)
			if _, err := CompileSnapshot(installation, policies, playbooks, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "reinitialize-in-place") {
				t.Fatalf("azure with a reachable recycle ladder: %v", err)
			}
		})
	}
}
