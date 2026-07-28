package metrics

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func selfSigned(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// A bundle reports its earliest expiry; junk records nothing.
func TestRecordCertBundleExpiry(t *testing.T) {
	early := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	late := early.Add(48 * time.Hour)
	bundle := append(selfSigned(t, late), selfSigned(t, early)...)

	RecordCertBundleExpiry("test-bundle", bundle)
	got := testutil.ToFloat64(TLSCertificateNotAfter.WithLabelValues("test-bundle"))
	if int64(got) != early.Unix() {
		t.Fatalf("recorded expiry = %d, want earliest %d", int64(got), early.Unix())
	}

	RecordCertBundleExpiry("junk", []byte("not pem"))
	if testutil.CollectAndCount(TLSCertificateNotAfter) != 1 {
		t.Fatal("junk material must record nothing")
	}
}
