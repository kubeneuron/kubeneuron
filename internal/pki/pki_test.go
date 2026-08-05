package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"testing"
	"time"
)

func mustAuthority(t *testing.T, now time.Time) *Authority {
	t.Helper()
	ca, err := NewAuthority("kubeneuron-agent-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// VerifyLeaf must accept only leaves that chain to this exact authority, so the
// operator can detect a leaf left behind by a replaced CA and reissue it.
func TestVerifyLeafChecksProvenance(t *testing.T) {
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	ca := mustAuthority(t, now)
	other := mustAuthority(t, now)

	leaf, err := ca.IssueServer("controller", []string{"kubeneuron-controller.kube-neuron.svc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.VerifyLeaf(leaf.CertPEM); err != nil {
		t.Fatalf("a leaf signed by this authority must verify: %v", err)
	}
	if err := other.VerifyLeaf(leaf.CertPEM); err == nil {
		t.Fatal("a leaf signed by a different authority must not verify")
	}
	if err := ca.VerifyLeaf([]byte("not pem")); err == nil {
		t.Fatal("non-PEM material must fail verification")
	}
}

// The whole design rests on this asymmetry: a long-lived signer everyone
// already trusts, and short-lived leaves that can be replaced without any
// coordination.
func TestAuthorityOutlivesTheCertificatesItSigns(t *testing.T) {
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	ca := mustAuthority(t, now)
	leaf, err := ca.IssueServer("controller", []string{"kubeneuron-controller.kube-neuron.svc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.NotAfter().After(parse(t, leaf.CertPEM).NotAfter) {
		t.Fatal("the authority must outlive its leaves")
	}
	if got := ca.NotAfter().Sub(now); got < 9*365*24*time.Hour {
		t.Fatalf("authority lifetime = %s, want the long-lived signer", got)
	}
}

func TestServerCertificateCarriesItsNamesAndUsage(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now)
	material, err := ca.IssueServer("controller", []string{"kubeneuron-controller.kube-neuron.svc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, material.CertPEM)
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "kubeneuron-controller.kube-neuron.svc" {
		t.Fatalf("dns names = %v", cert.DNSNames)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("ext key usage = %v, want serverAuth only", cert.ExtKeyUsage)
	}
}

// The identity URI is what stops material from one installation authenticating
// against another installation of the same product.
func TestClientCertificateCarriesTheInstallationIdentity(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now)
	identity, _ := url.Parse("spiffe://kubeneuron.io/installation/abc-123/agent")
	material, err := ca.IssueClient("agent", identity, now)
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, material.CertPEM)
	if len(cert.URIs) != 1 || cert.URIs[0].String() != identity.String() {
		t.Fatalf("uris = %v, want the installation identity", cert.URIs)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("ext key usage = %v, want clientAuth only", cert.ExtKeyUsage)
	}
}

// A leaf must chain to its authority, or the fleet cannot authenticate at all.
func TestIssuedLeavesVerifyAgainstTheAuthority(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("authority PEM is not usable as a root")
	}
	material, err := ca.IssueServer("controller", []string{"kubeneuron-controller.kube-neuron.svc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = parse(t, material.CertPEM).Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   "kubeneuron-controller.kube-neuron.svc",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Fatalf("leaf does not chain to its authority: %v", err)
	}
}

// A restarted operator has to keep signing with the CA the fleet already
// trusts; generating a fresh one would lock every agent out.
func TestAuthorityReloadsAndKeepsSigning(t *testing.T) {
	now := time.Now()
	original := mustAuthority(t, now)
	reloaded, err := LoadAuthority(original.Material)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(original.CertPEM)

	material, err := reloaded.IssueServer("controller", []string{"svc.local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parse(t, material.CertPEM).Verify(x509.VerifyOptions{
		Roots: roots, DNSName: "svc.local",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("a reloaded authority must keep signing for the same trust root: %v", err)
	}
}

func TestLoadAuthorityRejectsMaterialThatIsNotACA(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now)
	leaf, err := ca.IssueServer("controller", []string{"svc.local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthority(leaf); err == nil {
		t.Fatal("a leaf certificate must not be usable as an authority")
	}
	if _, err := LoadAuthority(Material{CertPEM: []byte("nonsense"), KeyPEM: []byte("nonsense")}); err == nil {
		t.Fatal("garbage must not load as an authority")
	}
}

func TestRenewalTiming(t *testing.T) {
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	ca := mustAuthority(t, now)
	material, err := ca.IssueServer("controller", []string{"svc.local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		at    time.Time
		renew bool
	}{
		{"fresh", now, false},
		{"halfway", now.Add(45 * 24 * time.Hour), false},
		// A third of the life left is 30 days of room to notice a renewal that
		// is failing, which is the number that actually matters.
		{"inside the renewal window", now.Add(65 * 24 * time.Hour), true},
		{"expired", now.Add(200 * 24 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsRenewal(material.CertPEM, tc.at); got != tc.renew {
				t.Fatalf("NeedsRenewal at %s = %v, want %v", tc.at, got, tc.renew)
			}
		})
	}
}

// Material nobody can read cannot be trusted to be valid. Treating it as fine
// would leave an installation unable to authenticate with nothing to act on.
func TestUnreadableMaterialNeedsRenewal(t *testing.T) {
	if !NeedsRenewal([]byte("not pem at all"), time.Now()) {
		t.Fatal("unparseable material must be renewed")
	}
	if !NeedsRenewal(nil, time.Now()) {
		t.Fatal("missing material must be renewed")
	}
}

// --- Fix 14: renewal lead is capped by an absolute bound ---

// The bare 1/3-life fraction demands renewal of a 10-year CA for its final ~3.3
// years; RenewalDue caps that lead so it only fires near real expiry. A
// short-lived leaf's 30-day fraction window is inside the cap, so the two agree.
func TestRenewalDueCapsTheLeadForLongLivedMaterial(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now) // 10-year CA

	// Seven years in, a third of its life is gone but it is nowhere near expiry:
	// NeedsRenewal (fraction only) says yes, RenewalDue (capped) says no.
	sevenYears := now.Add(7 * 365 * 24 * time.Hour)
	if !NeedsRenewal(ca.CertPEM, sevenYears) {
		t.Fatal("precondition: the bare fraction rule should want renewal at 7y")
	}
	if RenewalDue(ca.CertPEM, sevenYears) {
		t.Fatal("a healthy decade-long CA must not be due for renewal three years early")
	}

	// Within the absolute lead of real expiry, renewal is due.
	nearExpiry := ca.NotAfter().Add(-10 * 24 * time.Hour)
	if !RenewalDue(ca.CertPEM, nearExpiry) {
		t.Fatal("a CA within the absolute lead of expiry must be due")
	}
	// Past expiry is always due.
	if !RenewalDue(ca.CertPEM, ca.NotAfter().Add(time.Hour)) {
		t.Fatal("an expired CA must be due")
	}

	// A 90-day leaf's fraction window (30 days) is inside the cap, so RenewalDue
	// and NeedsRenewal agree there.
	leaf, err := ca.IssueServer("controller", []string{"svc.local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	twentyDaysLeft := now.Add(LeafLifetime - 20*24*time.Hour)
	if !RenewalDue(leaf.CertPEM, twentyDaysLeft) || !NeedsRenewal(leaf.CertPEM, twentyDaysLeft) {
		t.Fatal("a leaf with 20 days left must be due under both rules")
	}
	if RenewalDue(leaf.CertPEM, now.Add(24*time.Hour)) {
		t.Fatal("a fresh leaf must not be due")
	}
}

// A leaf issued near the end of its signer's life must not outlive it.
func TestLeafIsClampedToTheAuthorityExpiry(t *testing.T) {
	now := time.Now()
	ca := mustAuthority(t, now)
	late := ca.NotAfter().Add(-24 * time.Hour)
	material, err := ca.IssueServer("controller", []string{"svc.local"}, late)
	if err != nil {
		t.Fatal(err)
	}
	if parse(t, material.CertPEM).NotAfter.After(ca.NotAfter()) {
		t.Fatal("a leaf must never outlive the authority that signed it")
	}
}
