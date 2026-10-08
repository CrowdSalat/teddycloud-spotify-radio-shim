package toniebox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strconv"
	"time"
)

// certValidity is how long the generated identity stays usable. It only has to
// outlive a test run; teddyCloud checks neither the notBefore nor the notAfter
// of a client certificate.
const certValidity = 24 * time.Hour

// issuerCommonName is the substring teddyCloud's identity gate looks for
// (src/server.c:320-321). It is matched against the issuer *or* subject common
// name of the client certificate.
const issuerCommonName = "TeddyCloud"

// Identity is the client certificate the mock presents to teddyCloud, together
// with the overlay id teddyCloud derives from it.
type Identity struct {
	// Certificate is the client chain: leaf first, then the signing CA.
	Certificate tls.Certificate
	// Overlay is the overlay id teddyCloud assigns, derived from the CN. All
	// per-overlay state — the freshness cache, the ear-press window, the
	// firmware fields — is keyed on it, so a stable value keeps runs
	// reproducible.
	Overlay string
	// MAC is the CN that produced Overlay.
	MAC string
}

// NewIdentity generates a client certificate for the given twelve-character hex
// MAC and returns it ready to use in a tls.Config.
//
// Two certificates are produced rather than one, and the reason is teddyCloud's
// identity gate. It substring-searches the issuer or subject common name for a
// fixed list ("Boxine Factory SubCA", "Toniebox SubCA", "TeddyCloud", "Toniebox
// Root CA") before it will look at the CN at all (src/server.c:320-321). A
// self-signed leaf whose CN is the MAC fails that gate, and because the API
// listener rejects unauthenticated connections outright
// (src/server.c:516-520), every request would come back 401. So a throwaway CA
// with CN "TeddyCloud" signs a leaf whose CN is the MAC, which satisfies both
// the gate and the twelve-character CN branch that extracts the overlay id
// (src/server.c:328-330).
//
// Nothing here is verified beyond those two substring checks — teddyCloud carries
// its own "TODO: CHECK THE CERTIFICATES FOR REAL!!!!!" on the same path — so a
// self-signed chain is all the mock needs.
func NewIdentity(mac string) (*Identity, error) {
	if len(mac) != 12 {
		return nil, fmt.Errorf("toniebox: box MAC %q must be exactly 12 hex characters", mac)
	}
	if _, err := strconv.ParseUint(mac, 16, 64); err != nil {
		return nil, fmt.Errorf("toniebox: box MAC %q: %w", mac, err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("toniebox: generating CA key: %w", err)
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: issuerCommonName, Organization: []string{issuerCommonName}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("toniebox: creating CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("toniebox: parsing generated CA certificate: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("toniebox: generating box key: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: mac, Organization: []string{issuerCommonName}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("toniebox: creating box certificate: %w", err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, fmt.Errorf("toniebox: parsing generated box certificate: %w", err)
	}

	return &Identity{
		Certificate: tls.Certificate{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
			Leaf:        leafCert,
		},
		Overlay: mac,
		MAC:     mac,
	}, nil
}

// CAPEM renders the signing certificate in PEM form. It is not needed to talk to
// teddyCloud, but it is the fastest way to inspect what the gate is matching
// against when a request is unexpectedly refused.
func (i *Identity) CAPEM() []byte {
	if len(i.Certificate.Certificate) < 2 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.Certificate.Certificate[1]})
}
