package operator

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
)

// TestCARotationIsItsOwnCondition covers the one reconcile failure a human must
// act on, and whose action is a documented procedure rather than a bug report.
//
// A CA inside its renewal window blocks EVERYTHING — no ConfigMap, Deployment
// or DaemonSet converges while it stands — for up to maxRenewalLead before the
// certificate actually expires. That is deliberate: replacing a trust root in
// place leaves a fleet that cannot mutually authenticate. But it used to
// surface only as Ready=False with reason ReconciliationFailed, which is
// indistinguishable from a transient apiserver blip, so an operator could watch
// configuration changes silently fail to land for weeks with nothing to alert
// on.
func TestCARotationIsItsOwnCondition(t *testing.T) {
	inst := &kubeneuronv1alpha1.KubeNeuron{}
	inst.Generation = 7

	setTLSMaterialCondition(inst, fmt.Errorf("reconcile TLS material: %w",
		&CARotationRequiredError{
			Secret: "kubeneuron-agent-client-ca",
			Reason: "authority is expiring or expired",
		}))

	cond := meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid")
	if cond == nil {
		t.Fatal("a CA that needs rotating produced no distinguishable condition; nothing else in " +
			"the installation converges until it is fixed, and an operator has nothing to alert on")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "CARotationRequired" {
		t.Fatalf("condition = %s/%s, want False/CARotationRequired", cond.Status, cond.Reason)
	}
	if !strings.Contains(cond.Message, "kubeneuron-agent-client-ca") {
		t.Fatalf("the message does not name the Secret to rotate: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "expand/activate/retire") {
		t.Fatalf("the message does not point at the procedure: %q", cond.Message)
	}
	if cond.ObservedGeneration != 7 {
		t.Fatalf("observedGeneration = %d, want the installation's 7", cond.ObservedGeneration)
	}
}

// TestUnrelatedFailuresDoNotRaiseIt: the condition is only useful if it means
// one thing. An apiserver blip must not light it.
func TestUnrelatedFailuresDoNotRaiseIt(t *testing.T) {
	inst := &kubeneuronv1alpha1.KubeNeuron{}
	setTLSMaterialCondition(inst, fmt.Errorf("apiserver said no"))
	if meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid") != nil {
		t.Fatal("an unrelated reconcile failure raised the CA-rotation condition")
	}
}

// TestACompletedRotationClearsIt: a red condition that outlives the problem it
// described trains people to ignore the next one.
func TestACompletedRotationClearsIt(t *testing.T) {
	inst := &kubeneuronv1alpha1.KubeNeuron{}
	setTLSMaterialCondition(inst, &CARotationRequiredError{Secret: "s", Reason: "r"})
	if meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid") == nil {
		t.Fatal("setup failed: the condition was never raised")
	}

	// The operator performs the rotation; the next pass succeeds.
	setTLSMaterialCondition(inst, nil)
	if meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid") != nil {
		t.Fatal("the condition survived a successful pass; it would stay red forever after a " +
			"rotation somebody had already completed")
	}
}

// TestTwoTLSRolesCannotShareOneSecret covers a plausible simplification —
// "one CA for everything" — that the API used to accept and the PKI could not
// honour.
//
// The PKI keys its material by ROLE, not by Secret name, so two roles pointing
// at one Secret with issuer: Operator mint two authorities and write both under
// the same name. The second wins, and the controller's serving leaf is then
// signed by an authority the agents' mounted CA does not contain — the fleet
// stops authenticating.
//
// With default keys it self-heals on the next pass, because the leaf provenance
// check reissues on a mismatch. With distinct keys it wedges permanently: the
// authority load fails, which raises CARotationRequired, and that deliberately
// blocks everything in the installation until a human runs the rotation
// procedure. Refusing the configuration is kinder than either.
func TestTwoTLSRolesCannotShareOneSecret(t *testing.T) {
	inst := validTLSInstallation()
	inst.Spec.TLS.ServerCASecretRef.Name = inst.Spec.TLS.ClientCASecretRef.Name

	err := validateTLS(inst)
	if err == nil {
		t.Fatal("two TLS roles naming one Secret was accepted; the operator would issue two " +
			"authorities under that name and the fleet would stop mutually authenticating")
	}
	if !strings.Contains(err.Error(), inst.Spec.TLS.ClientCASecretRef.Name) {
		t.Fatalf("the error does not name the shared Secret: %v", err)
	}

	// And the ordinary configuration is still accepted.
	if err := validateTLS(validTLSInstallation()); err != nil {
		t.Fatalf("a well-formed TLS block was rejected: %v", err)
	}
}

func validTLSInstallation() *kubeneuronv1alpha1.KubeNeuron {
	ref := func(name string) *kubeneuronv1alpha1.SecretReference {
		return &kubeneuronv1alpha1.SecretReference{Name: name}
	}
	inst := &kubeneuronv1alpha1.KubeNeuron{}
	inst.Spec.TLS.ServerSecretRef = ref("kn-controller-tls")
	inst.Spec.TLS.ClientCASecretRef = ref("kn-agent-client-ca")
	inst.Spec.TLS.ClientSecretRef = ref("kn-agent-tls")
	inst.Spec.TLS.ServerCASecretRef = ref("kn-controller-server-ca")
	return inst
}

// TestAnUnrelatedFailureDoesNotClearIt: the condition exists to be alerted on,
// and an alert that blinks off on every apiserver blip is one people mute.
//
// Two reconcile paths renew TLS best-effort and then report an unrelated error
// — a failure to list child configuration, an invalid compile. Neither learned
// anything about the CA, so neither may clear its verdict.
func TestAnUnrelatedFailureDoesNotClearIt(t *testing.T) {
	inst := &kubeneuronv1alpha1.KubeNeuron{}
	setTLSMaterialCondition(inst, &CARotationRequiredError{Secret: "s", Reason: "expiring"})
	if meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid") == nil {
		t.Fatal("setup failed: the condition was never raised")
	}

	setTLSMaterialCondition(inst, fmt.Errorf("list GPUPlaybooks: apiserver unavailable"))
	if meta.FindStatusCondition(inst.Status.Conditions, "TLSMaterialValid") == nil {
		t.Fatal("an unrelated reconcile failure cleared the CA-rotation condition; the signal " +
			"would flap off and on with every transient apiserver error")
	}
}
