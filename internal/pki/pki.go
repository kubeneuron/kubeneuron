// Package pki issues and renews the mTLS material the controller and its agents
// authenticate with.
//
// The shape is deliberate. The certificate authority is long-lived and the leaf
// certificates are short-lived and renewed automatically, rather than the
// reverse. Rotating a CA means every party has to trust the new one *before*
// anything presents a certificate signed by it, which is a multi-phase rollout
// that has to survive restarts; rotating a leaf under a stable CA needs no
// coordination at all, because everyone already trusts the signer. CA rotation
// therefore stays a rare, documented, operator-driven event, and the thing that
// would otherwise expire silently every year is handled by the operator.
//
// This exists because nothing renewed anything. The installer issued 365-day
// certificates with openssl and no code anywhere watched them, so an
// installation stopped being able to authenticate roughly a year after it was
// created, with no warning and no mechanism to prevent it.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"time"
)

const (
	// CALifetime is how long an issued authority is valid. Long on purpose: see
	// the package comment. Replacing it is an operator-driven procedure.
	CALifetime = 10 * 365 * 24 * time.Hour
	// LeafLifetime is how long an issued certificate is valid.
	LeafLifetime = 90 * 24 * time.Hour
	// renewAtRemainingFraction renews a leaf once less than this fraction of its
	// life is left. A third of 90 days is 30 days of room to notice and fix a
	// renewal that is failing, which is the number that actually matters.
	renewAtRemainingFraction = 1.0 / 3.0
	// maxRenewalLead caps how early the fraction rule can demand renewal, in
	// absolute time. Without it, a third of a 10-year CA's life is 3.3 years, so
	// the operator would treat a perfectly healthy CA as "needs renewal" — and
	// for a managed CA that is a hard reconcile failure (CARotationRequiredError)
	// — more than three years before it actually expires, freezing the config
	// plane. 45 days comfortably exceeds a leaf's 30-day fraction window (so leaf
	// behavior is unchanged) while keeping the CA's renewal demand near its real
	// expiry rather than years early.
	maxRenewalLead = 45 * 24 * time.Hour
	// clockSkewAllowance backdates NotBefore so a certificate is not rejected by
	// a peer whose clock runs slightly behind the issuer's.
	clockSkewAllowance = 5 * time.Minute
)

// Material is a PEM-encoded key pair.
type Material struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Authority is a signing CA and its own PEM encoding.
type Authority struct {
	Material
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// NewAuthority creates a self-signed authority.
func NewAuthority(commonName string, now time.Time) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generating authority key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkewAllowance),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// The authority signs leaves and nothing else; there is no intermediate
		// tier to allow.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("pki: signing authority: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing authority: %w", err)
	}
	material, err := encode(der, key)
	if err != nil {
		return nil, err
	}
	return &Authority{Material: material, cert: cert, key: key}, nil
}

// LoadAuthority reconstructs an authority from its PEM material so an operator
// restart keeps signing with the CA the fleet already trusts.
func LoadAuthority(material Material) (*Authority, error) {
	certBlock, _ := pem.Decode(material.CertPEM)
	keyBlock, _ := pem.Decode(material.KeyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("pki: authority material is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing authority certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("pki: authority certificate is not a CA")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parsing authority key: %w", err)
	}
	return &Authority{Material: material, cert: cert, key: key}, nil
}

// NotAfter is when this authority stops being usable.
func (a *Authority) NotAfter() time.Time { return a.cert.NotAfter }

// VerifyLeaf reports whether certPEM is a leaf that chains to THIS authority:
// the issuer matches and the signature verifies against the CA's public key. It
// deliberately checks provenance only, not expiry (NeedsRenewal owns that), so a
// caller can detect a leaf that was signed by a now-replaced CA — for example a
// leaf written just before a crash that then recreated the CA Secret — and
// reissue it before the fleet, which trusts only the current CA, loses mTLS.
func (a *Authority) VerifyLeaf(certPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("pki: leaf is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("pki: parsing leaf: %w", err)
	}
	if err := leaf.CheckSignatureFrom(a.cert); err != nil {
		return fmt.Errorf("pki: leaf does not chain to the current authority: %w", err)
	}
	return nil
}

// IssueServer signs a serving certificate for the given DNS names.
func (a *Authority) IssueServer(commonName string, dnsNames []string, now time.Time) (Material, error) {
	return a.issue(commonName, now, func(template *x509.Certificate) {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.DNSNames = dnsNames
	})
}

// IssueClient signs a client certificate carrying a SPIFFE-style identity URI.
// The URI binds the certificate to one installation, so material from another
// installation of the same product cannot authenticate against this one.
func (a *Authority) IssueClient(commonName string, identity *url.URL, now time.Time) (Material, error) {
	return a.issue(commonName, now, func(template *x509.Certificate) {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		if identity != nil {
			template.URIs = []*url.URL{identity}
		}
	})
}

func (a *Authority) issue(commonName string, now time.Time, shape func(*x509.Certificate)) (Material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, fmt.Errorf("pki: generating leaf key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return Material{}, err
	}
	notAfter := now.Add(LeafLifetime)
	if notAfter.After(a.cert.NotAfter) {
		// A leaf that outlives its signer is unusable for the difference.
		notAfter = a.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkewAllowance),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	shape(template)
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return Material{}, fmt.Errorf("pki: signing %s: %w", commonName, err)
	}
	return encode(der, key)
}

// NotAfter reports when a PEM-encoded certificate expires.
func NotAfter(certPEM []byte) (time.Time, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}, fmt.Errorf("pki: certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("pki: parsing certificate: %w", err)
	}
	return cert.NotAfter, nil
}

// NeedsRenewal reports whether a certificate is close enough to expiry to be
// replaced, and is deliberately generous about what counts.
//
// Unparseable or already-expired material needs renewal: the alternative is an
// installation that cannot authenticate and an operator with nothing to act on.
func NeedsRenewal(certPEM []byte, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return needsRenewal(cert.NotBefore, cert.NotAfter, now)
}

func needsRenewal(notBefore, notAfter, now time.Time) bool {
	if !now.Before(notAfter) {
		return true
	}
	life := notAfter.Sub(notBefore)
	if life <= 0 {
		return true
	}
	remaining := notAfter.Sub(now)
	return float64(remaining)/float64(life) < renewAtRemainingFraction
}

// RenewalDue is the fraction rule bounded by an absolute lead time: renewal is
// due only when the certificate is within maxRenewalLead of expiry AND past the
// 1/3-life fraction. It exists for long-lived material — a 10-year CA — where
// the fraction alone would demand renewal 3.3 years early. Applied to a managed
// CA whose renewal is a hard, operator-driven event, the uncapped rule froze the
// whole config plane years before expiry; the cap keeps that demand near the CA's
// real end of life. For short-lived leaves the fraction window (30 days of 90)
// is well inside the cap, so RenewalDue and NeedsRenewal agree there.
//
// Unparseable or already-expired material is due, for the same reason
// NeedsRenewal treats it so: the alternative is silent failure.
func RenewalDue(certPEM []byte, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	if !now.Before(cert.NotAfter) {
		return true
	}
	if cert.NotAfter.Sub(now) > maxRenewalLead {
		return false
	}
	return needsRenewal(cert.NotBefore, cert.NotAfter, now)
}

func encode(der []byte, key *ecdsa.PrivateKey) (Material, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Material{}, fmt.Errorf("pki: encoding key: %w", err)
	}
	return Material{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("pki: generating serial: %w", err)
	}
	return serial, nil
}
