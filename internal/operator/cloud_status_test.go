// SPDX-License-Identifier: Apache-2.0
package operator

import (
	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
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
