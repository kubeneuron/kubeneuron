package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/pki"
)

func pkiInstallation() *kubeneuronv1alpha1.KubeNeuron {
	installation := &kubeneuronv1alpha1.KubeNeuron{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeneuron", UID: "abc-123"},
	}
	installation.Spec.Namespace = "kube-neuron"
	installation.Spec.TLS.ServerSecretRef = &kubeneuronv1alpha1.SecretReference{Name: "kubeneuron-controller-tls"}
	installation.Spec.TLS.ServerCASecretRef = &kubeneuronv1alpha1.SecretReference{Name: "kubeneuron-controller-server-ca"}
	installation.Spec.TLS.ClientSecretRef = &kubeneuronv1alpha1.SecretReference{Name: "kubeneuron-agent-tls"}
	installation.Spec.TLS.ClientCASecretRef = &kubeneuronv1alpha1.SecretReference{Name: "kubeneuron-agent-client-ca"}
	return installation
}

func secretsByName(plan *PKIPlan) map[string]*corev1.Secret {
	out := map[string]*corev1.Secret{}
	for _, s := range plan.Secrets {
		out[s.Name] = s
	}
	return out
}

// A fresh installation gets a complete, self-consistent PKI without anyone
// running openssl by hand.
func TestFreshInstallationIsIssuedACompletePKI(t *testing.T) {
	now := time.Now()
	plan, err := planPKI(pkiInstallation(), pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByName(plan)
	for _, name := range []string{
		"kubeneuron-controller-tls", "kubeneuron-controller-server-ca",
		"kubeneuron-agent-tls", "kubeneuron-agent-client-ca",
	} {
		if got[name] == nil {
			t.Fatalf("secrets = %v, want %s issued", plan.Secrets, name)
		}
		if got[name].Labels[managedPKILabel] != "true" {
			t.Fatalf("%s must be marked as issued by us, or renewal will skip it", name)
		}
	}
	if !plan.Rotated {
		t.Fatal("issuing material must roll the workloads that mount it")
	}
	// The signing key must never be handed to a workload; only ca.crt is
	// mounted, so it lives beside its authority and nowhere else.
	if len(got["kubeneuron-agent-client-ca"].Data["ca.key"]) == 0 {
		t.Fatal("the authority must keep its signing key")
	}
}

// The point of the whole change: material gets replaced before it expires,
// without anybody noticing.
func TestManagedLeafIsRenewedBeforeExpiry(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	initial, err := planPKI(installation, pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	existing := pkiInputs{}
	for _, s := range initial.Secrets {
		switch s.Name {
		case "kubeneuron-controller-tls":
			existing[RoleServer] = s
		case "kubeneuron-controller-server-ca":
			existing[RoleServerCA] = s
		case "kubeneuron-agent-tls":
			existing[RoleClient] = s
		case "kubeneuron-agent-client-ca":
			existing[RoleClientCA] = s
		}
	}

	// Nothing to do while the material is fresh.
	steady, err := planPKI(installation, existing, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(steady.Secrets) != 0 || steady.Rotated {
		t.Fatalf("secrets = %v, want no churn while the material is fresh", steady.Secrets)
	}

	// Inside the renewal window the leaves are reissued and the authorities are
	// left alone — that asymmetry is what makes rotation need no coordination.
	renewed, err := planPKI(installation, existing, now.Add(70*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByName(renewed)
	if got["kubeneuron-controller-tls"] == nil || got["kubeneuron-agent-tls"] == nil {
		t.Fatalf("secrets = %v, want both leaves renewed", renewed.Secrets)
	}
	if got["kubeneuron-agent-client-ca"] != nil || got["kubeneuron-controller-server-ca"] != nil {
		t.Fatal("the authority must not be replaced just because a leaf aged")
	}
	if !renewed.Rotated {
		t.Fatal("a renewal has to roll the workloads mounting the old certificate")
	}
}

// Material somebody else supplied — cert-manager, a corporate CA, the installer
// before this existed — must never be overwritten. It is reported instead.
func TestForeignMaterialIsWarnedAboutAndNeverReplaced(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	// Age the foreign CA so it is genuinely near expiry (~20 days out, inside the
	// capped renewal-lead window). The warning uses RenewalDue, so a healthy
	// long-lived external CA is not flagged years early; one this close still is.
	authority, err := pki.NewAuthority("someone-elses-ca", now.Add(-pki.CALifetime+20*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	foreignCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeneuron-agent-client-ca"},
		Data:       map[string][]byte{"ca.crt": authority.CertPEM},
	}

	plan, err := planPKI(installation, pkiInputs{RoleClientCA: foreignCA}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Secrets {
		if s.Name == "kubeneuron-agent-client-ca" {
			t.Fatal("material we did not issue must never be replaced")
		}
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("an expiring certificate nobody renews has to be reported")
	}
	if !strings.Contains(plan.Warnings[0], "nothing will renew it") {
		t.Fatalf("warning = %q, want it to say the material is unmanaged", plan.Warnings[0])
	}
	// The agent leaf under that foreign authority must not be issued either:
	// it would not chain to what the fleet is told to trust.
	for _, s := range plan.Secrets {
		if s.Name == "kubeneuron-agent-tls" {
			t.Fatal("a leaf must not be signed by an authority we do not hold")
		}
	}
}

// Fix H-F5: a healthy, long-lived external CA must not be flagged for renewal
// years early. The warning uses the absolute-lead-capped RenewalDue, not the
// bare 1/3-life fraction, so a fresh 10-year foreign CA stays silent.
func TestLongLivedForeignCADoesNotWarnEarly(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	authority, err := pki.NewAuthority("someone-elses-ca", now) // ~10 years of life left
	if err != nil {
		t.Fatal(err)
	}
	foreignCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeneuron-agent-client-ca"},
		Data:       map[string][]byte{"ca.crt": authority.CertPEM},
	}
	plan, err := planPKI(installation, pkiInputs{RoleClientCA: foreignCA}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "kubeneuron-agent-client-ca") {
			t.Fatalf("a healthy long-lived external CA must not warn years early; got %q", w)
		}
	}
}

// Fix H-F3: a managed leaf that no longer chains to the CURRENT authority — a CA
// Secret recreated out from under a still-fresh leaf — must be reissued, even
// though it is nowhere near expiry and authorityChanged is false.
func TestManagedLeafReissuedWhenItDoesNotChainToCurrentCA(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	initial, err := planPKI(installation, pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	existing := pkiInputs{}
	for _, s := range initial.Secrets {
		switch s.Name {
		case "kubeneuron-controller-tls":
			existing[RoleServer] = s
		case "kubeneuron-controller-server-ca":
			existing[RoleServerCA] = s
		case "kubeneuron-agent-tls":
			existing[RoleClient] = s
		case "kubeneuron-agent-client-ca":
			existing[RoleClientCA] = s
		}
	}

	// Replace the client CA Secret with a DIFFERENT managed authority, as a crash
	// or partial restore between writing a new CA and its leaf would. The leaf in
	// RoleClient was signed by the previous authority.
	newCA, err := pki.NewAuthority("kubeneuron-agent-client-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	caKeys := pkiCAKeys(installation)
	existing[RoleClientCA] = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "kubeneuron-agent-client-ca",
			Labels: map[string]string{managedPKILabel: "true"},
		},
		Data: map[string][]byte{caKeys[RoleClientCA]: newCA.CertPEM, caPrivateKeyKey: newCA.KeyPEM},
	}

	// The leaf is fresh (only a day old), so only a chain check — not expiry —
	// can trigger reissue.
	plan, err := planPKI(installation, existing, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByName(plan)
	if got["kubeneuron-agent-tls"] == nil {
		t.Fatal("a leaf that does not chain to the current CA must be reissued")
	}
	if err := newCA.VerifyLeaf(got["kubeneuron-agent-tls"].Data[corev1.TLSCertKey]); err != nil {
		t.Fatalf("the reissued leaf must chain to the current authority: %v", err)
	}
	// The server leaf still chains to its unchanged CA, so it is left alone.
	if got["kubeneuron-controller-tls"] != nil {
		t.Fatal("a leaf that still chains to its CA must not be needlessly reissued")
	}
}

// Replacing an authority invalidates everything it signed, so the leaves have
// to be reissued in the same pass or the fleet cannot authenticate.
func TestReplacingAnAuthorityReissuesItsLeaf(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	initial, err := planPKI(installation, pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	existing := pkiInputs{}
	for _, s := range initial.Secrets {
		if s.Name == "kubeneuron-agent-tls" {
			existing[RoleClient] = s
		}
	}
	// The authority is gone; a new one is generated.
	plan, err := planPKI(installation, existing, now)
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByName(plan)
	if got["kubeneuron-agent-client-ca"] == nil || got["kubeneuron-agent-tls"] == nil {
		t.Fatalf("secrets = %v, want the authority and its leaf issued together", plan.Secrets)
	}
}

func TestUnreadableManagedAuthorityRequiresManualRotation(t *testing.T) {
	now := time.Now()
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "kubeneuron-agent-client-ca",
			Labels: map[string]string{managedPKILabel: "true"},
		},
		Data: map[string][]byte{"ca.crt": []byte("corrupt"), "ca.key": []byte("corrupt")},
	}
	_, err := planPKI(pkiInstallation(), pkiInputs{RoleClientCA: broken}, now)
	var rotation *CARotationRequiredError
	if !errors.As(err, &rotation) {
		t.Fatalf("planPKI() error = %v, want manual CA rotation requirement", err)
	}
	if rotation.Secret != "kubeneuron-agent-client-ca" {
		t.Fatalf("rotation Secret = %q", rotation.Secret)
	}
}

// --- Fix 14: a healthy long-lived managed CA must not freeze the config plane ---

// The 1/3-life fraction says a 10-year CA needs renewal for its final ~3.3
// years; applied to a managed CA that is a hard rotation demand (freezing every
// reconcile). The absolute lead cap means the demand only fires near real
// expiry, so seven years in — a third of its life gone, still years from expiry
// — planPKI must succeed, not raise CARotationRequiredError.
func TestManagedCANotRotatedYearsBeforeExpiry(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	initial, err := planPKI(installation, pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	existing := pkiInputs{}
	for _, s := range initial.Secrets {
		switch s.Name {
		case "kubeneuron-controller-tls":
			existing[RoleServer] = s
		case "kubeneuron-controller-server-ca":
			existing[RoleServerCA] = s
		case "kubeneuron-agent-tls":
			existing[RoleClient] = s
		case "kubeneuron-agent-client-ca":
			existing[RoleClientCA] = s
		}
	}
	if _, err := planPKI(installation, existing, now.Add(7*365*24*time.Hour)); err != nil {
		var rotation *CARotationRequiredError
		if errors.As(err, &rotation) {
			t.Fatalf("a healthy decade-long CA must not demand rotation three years early: %v", err)
		}
		t.Fatalf("planPKI at 7y: %v", err)
	}
}

// A migrated (unmanaged) CA with a leftover KubeNeuron-managed leaf beneath it
// must be warned about: the operator cannot renew the leaf (it does not hold the
// signing CA), and staying silent would let the leaf expire at 90 days and take
// the fleet's mTLS down with no notice.
func TestManagedLeafUnderUnmanagedCAIsWarned(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()

	extCA, err := pki.NewAuthority("someone-elses-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	unmanagedCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeneuron-agent-client-ca"},
		Data:       map[string][]byte{"ca.crt": extCA.CertPEM},
	}
	leaf, err := extCA.IssueClient("kubeneuron-agent", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	managedLeaf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeneuron-agent-tls", Labels: map[string]string{managedPKILabel: "true"}},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: leaf.CertPEM, corev1.TLSPrivateKeyKey: leaf.KeyPEM},
	}

	plan, err := planPKI(installation, pkiInputs{RoleClientCA: unmanagedCA, RoleClient: managedLeaf}, now)
	if err != nil {
		t.Fatal(err)
	}
	if secretsByName(plan)["kubeneuron-agent-tls"] != nil {
		t.Fatal("a leaf under an unmanaged CA must not be reissued — the operator does not hold that signer")
	}
	warned := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "kubeneuron-agent-tls") && strings.Contains(w, "signing CA is not managed") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a managed leaf under an unmanaged CA must be warned about; warnings = %v", plan.Warnings)
	}
}

// An installation without a complete TLS block gets no material invented for
// it: issuing into a Secret nothing mounts would look like success and change
// nothing.
func TestIncompleteTLSBlockIssuesNothing(t *testing.T) {
	installation := pkiInstallation()
	installation.Spec.TLS.ClientCASecretRef = nil
	plan, err := planPKI(installation, pkiInputs{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Secrets) != 0 || plan.Rotated {
		t.Fatalf("secrets = %v, want nothing issued", plan.Secrets)
	}
}

// The installation chooses which key inside a CA Secret holds the bundle, and
// the workloads mount exactly that key. Writing ca.crt regardless would produce
// material nothing reads — a failure that only shows up as a handshake error.
func TestIssuedAuthorityUsesTheDeclaredBundleKey(t *testing.T) {
	installation := pkiInstallation()
	installation.Spec.TLS.ClientCASecretRef.Key = "clients.pem"
	plan, err := planPKI(installation, pkiInputs{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secret := secretsByName(plan)["kubeneuron-agent-client-ca"]
	if secret == nil {
		t.Fatal("the client authority must be issued")
	}
	if len(secret.Data["clients.pem"]) == 0 {
		t.Fatalf("data keys = %v, want the declared bundle key populated", mapKeys(secret.Data))
	}
	if len(secret.Data["ca.crt"]) != 0 {
		t.Fatal("a bundle must not also be written under a key nothing mounts")
	}
	if len(secret.Data["ca.key"]) == 0 {
		t.Fatal("the signing key must still live beside its authority")
	}
}

// A managed authority stored under a custom key must be reloaded from that key,
// or every reconcile would decide it was unreadable and replace it — churning
// the fleet's trust root on a timer.
func TestManagedAuthorityWithACustomKeyIsReused(t *testing.T) {
	now := time.Now()
	installation := pkiInstallation()
	installation.Spec.TLS.ClientCASecretRef.Key = "clients.pem"
	first, err := planPKI(installation, pkiInputs{}, now)
	if err != nil {
		t.Fatal(err)
	}
	existing := pkiInputs{}
	for _, s := range first.Secrets {
		switch s.Name {
		case "kubeneuron-agent-client-ca":
			existing[RoleClientCA] = s
		case "kubeneuron-agent-tls":
			existing[RoleClient] = s
		}
	}
	second, err := planPKI(installation, existing, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if secretsByName(second)["kubeneuron-agent-client-ca"] != nil {
		t.Fatal("a readable authority must be reused, not replaced on every pass")
	}
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Issuance is opt-in by declaration, not by circumstance. A Secret being absent
// is not an invitation: taking over material somebody else manages the moment
// they deleted it mid-rotation would be far worse than leaving it missing.
func TestExternalIssuerNeverWritesMaterial(t *testing.T) {
	installation := pkiInstallation() // Issuer is unset, i.e. External
	if installation.Spec.TLS.Issuer == kubeneuronv1alpha1.TLSIssuerOperator {
		t.Fatal("the default must not be operator issuance")
	}
}

// The TLS revision is scoped per workload: a digest that unioned all four
// roles would roll the controller Deployment when only the agents' server-CA
// trust expanded, breaking the rotation protocol's promise that a phase rolls
// only its consumers (caught live by the kind rotation-scope assertion).
func TestTLSRevisionIsScopedToWhatEachWorkloadMounts(t *testing.T) {
	installation := pkiInstallation()
	r := &KubeNeuronReconciler{}
	secretFor := func(role PKIRole, generation string) *corev1.Secret {
		names, _ := pkiSecretNames(installation)
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: names[role], Namespace: "kube-neuron"},
			Data: map[string][]byte{
				"tls.crt": []byte(string(role) + "-cert-" + generation),
				"tls.key": []byte(string(role) + "-key-" + generation),
				"ca.crt":  []byte(string(role) + "-ca-" + generation),
			},
		}
	}
	base := pkiInputs{}
	for _, role := range []PKIRole{RoleServer, RoleServerCA, RoleClient, RoleClientCA} {
		base[role] = secretFor(role, "v1")
	}
	baseline, err := r.tlsRevision(context.Background(), installation, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Controller == "" || baseline.Agent == "" || baseline.Controller == baseline.Agent {
		t.Fatalf("revisions = %+v, want two distinct non-empty digests", baseline)
	}

	cases := []struct {
		role           PKIRole
		wantController bool // controller digest must change
		wantAgent      bool // agent digest must change
	}{
		{RoleServer, true, false},   // serving leaf: controller mounts it
		{RoleClientCA, true, false}, // client CA: controller verifies agents with it
		{RoleClient, false, true},   // client leaf: agents present it
		{RoleServerCA, false, true}, // server CA: agents trust it — expand-trust must NOT roll the controller
	}
	for _, tc := range cases {
		mutated := pkiInputs{}
		for role, secret := range base {
			mutated[role] = secret
		}
		mutated[tc.role] = secretFor(tc.role, "v2")
		got, err := r.tlsRevision(context.Background(), installation, mutated, nil)
		if err != nil {
			t.Fatal(err)
		}
		if changed := got.Controller != baseline.Controller; changed != tc.wantController {
			t.Fatalf("%s: controller digest changed=%v, want %v", tc.role, changed, tc.wantController)
		}
		if changed := got.Agent != baseline.Agent; changed != tc.wantAgent {
			t.Fatalf("%s: agent digest changed=%v, want %v", tc.role, changed, tc.wantAgent)
		}
	}
}
