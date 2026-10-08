package toniebox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

// gatePasses reproduces teddyCloud's identity gate exactly as written, so that
// the assertion is about the code under test rather than about a paraphrase of
// it (src/server.c:320-321).
func gatePasses(issuerCN, subjectCN string) bool {
	return strings.Contains(issuerCN, "Boxine Factory SubCA") ||
		strings.Contains(issuerCN, "Toniebox SubCA") ||
		strings.Contains(issuerCN, "TeddyCloud") ||
		strings.Contains(subjectCN, "TeddyCloud") ||
		strings.Contains(issuerCN, "Toniebox Root CA")
}

// cnExtraction reproduces src/server.c:326-330.
func cnExtraction(subject string) (string, bool) {
	if len(subject) == 15 && strings.HasPrefix(subject, "b'") && strings.HasSuffix(subject, "'") {
		return subject[2:14], true
	}
	if len(subject) == 12 {
		return subject, true
	}
	return "", false
}

// TestIdentityPassesTheGate is the assertion the whole certificate strategy
// hangs on. A self-signed leaf with the MAC as its CN — which reads as the
// obvious design — fails, because only common names ever reach the gate and a
// self-signed issuer equals its own subject.
func TestIdentityPassesTheGate(t *testing.T) {
	const mac = "00aabbccdd00"

	id, err := NewIdentity(mac)
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if len(id.Certificate.Certificate) != 2 {
		t.Fatalf("chain has %d certificates, want leaf + CA", len(id.Certificate.Certificate))
	}

	leaf, err := x509.ParseCertificate(id.Certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parsing leaf: %v", err)
	}
	ca, err := x509.ParseCertificate(id.Certificate.Certificate[1])
	if err != nil {
		t.Fatalf("parsing CA: %v", err)
	}

	if leaf.Subject.CommonName != mac {
		t.Errorf("leaf CN = %q, want %q", leaf.Subject.CommonName, mac)
	}
	if ca.Subject.CommonName != "TeddyCloud" {
		t.Errorf("CA CN = %q, want TeddyCloud", ca.Subject.CommonName)
	}
	if leaf.Issuer.CommonName != ca.Subject.CommonName {
		t.Errorf("leaf issuer CN = %q, want %q", leaf.Issuer.CommonName, ca.Subject.CommonName)
	}
	if !ca.IsCA {
		t.Error("signing certificate is not marked as a CA")
	}

	if !gatePasses(leaf.Issuer.CommonName, leaf.Subject.CommonName) {
		t.Error("generated chain does not pass the identity gate")
	}

	overlay, ok := cnExtraction(leaf.Subject.CommonName)
	if !ok {
		t.Fatalf("leaf CN %q is not extractable", leaf.Subject.CommonName)
	}
	if overlay != mac {
		t.Errorf("overlay = %q, want %q", overlay, mac)
	}
	if id.Overlay != mac {
		t.Errorf("Identity.Overlay = %q, want %q", id.Overlay, mac)
	}
}

// TestSelfSignedLeafFailsTheGate pins the design's corrected claim: putting
// TeddyCloud in the Organization field does nothing, because tls_adapter copies
// only the common name (src/tls_adapter.c:626-627).
func TestSelfSignedLeafFailsTheGate(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "00aabbccdd00",
			Organization: []string{"TeddyCloud"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	if cert.Subject.CommonName != cert.Issuer.CommonName {
		t.Fatal("expected a self-signed certificate, issuer differs from subject")
	}
	if gatePasses(cert.Issuer.CommonName, cert.Subject.CommonName) {
		t.Error("self-signed leaf passed the gate; the mock's CA would be unnecessary")
	}
}

// TestIdentityRejectsBadMAC covers the failure that would otherwise surface as
// a certificate teddyCloud happily accepts under the wrong overlay id.
func TestIdentityRejectsBadMAC(t *testing.T) {
	for _, mac := range []string{"", "00aabbccdd", "00aabbccdd000", "00aabbccddzz"} {
		if _, err := NewIdentity(mac); err == nil {
			t.Errorf("NewIdentity(%q) succeeded, want an error", mac)
		}
	}
}

// TestIdentityCAPEMParses keeps the debugging aid usable: it is the fastest way
// to see what the gate is matching against when a request is refused.
func TestIdentityCAPEMParses(t *testing.T) {
	id, err := NewIdentity("00aabbccdd00")
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if len(id.CAPEM()) == 0 {
		t.Fatal("CAPEM returned nothing")
	}
}
