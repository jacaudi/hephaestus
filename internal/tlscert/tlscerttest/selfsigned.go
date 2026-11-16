package tlscerttest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WriteSelfSigned writes an ECDSA P-256 self-signed leaf for fqdn, valid from
// notBefore to notAfter, and its key as PEM files (CERTIFICATE and PRIVATE KEY
// PKCS#8) named tls.crt and tls.key in dir, replacing any already there. Every
// call has a unique serial number. It returns the two paths and fails the test
// on any error.
func WriteSelfSigned(tb testing.TB, dir, fqdn string, notBefore, notAfter time.Time) (certFile, keyFile string) {
	tb.Helper()

	key := newKey(tb)
	leaf := sign(tb, leafTemplate(tb, fqdn, notBefore, notAfter), nil, &key.PublicKey, key)
	return writePair(tb, dir, key, leaf.Raw)
}

// WriteChain is WriteSelfSigned for a leaf issued by a fresh root CA, or, when
// withIntermediate is set, by a fresh intermediate CA that the root issued. All
// certificates are valid from notBefore to notAfter. The certificate file holds
// the leaf, followed by the intermediate when includeIntermediate is also set,
// as a fullchain.pem does; the root is never written to it. It returns the two
// paths and the root, which a verifier must trust to accept the leaf.
//
//nolint:revive // argument-limit: a wide test helper, and the signature is the contract the tests share; the repo exempts _test.go files from this rule for the same reason
func WriteChain(tb testing.TB, dir, fqdn string, notBefore, notAfter time.Time, withIntermediate, includeIntermediate bool) (certFile, keyFile string, root *x509.Certificate) {
	tb.Helper()

	rootKey := newKey(tb)
	root = sign(tb, caTemplate(tb, "tlscerttest root", notBefore, notAfter), nil, &rootKey.PublicKey, rootKey)
	issuer, issuerKey := root, rootKey
	if withIntermediate {
		intKey := newKey(tb)
		issuer = sign(tb, caTemplate(tb, "tlscerttest intermediate", notBefore, notAfter), root, &intKey.PublicKey, rootKey)
		issuerKey = intKey
	}

	key := newKey(tb)
	leaf := sign(tb, leafTemplate(tb, fqdn, notBefore, notAfter), issuer, &key.PublicKey, issuerKey)
	chain := [][]byte{leaf.Raw}
	if withIntermediate && includeIntermediate {
		chain = append(chain, issuer.Raw)
	}
	certFile, keyFile = writePair(tb, dir, key, chain...)
	return certFile, keyFile, root
}

func newKey(tb testing.TB) *ecdsa.PrivateKey {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("tlscerttest: generate key: %v", err)
	}
	return key
}

func newSerial(tb testing.TB) *big.Int {
	tb.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		tb.Fatalf("tlscerttest: serial number: %v", err)
	}
	return serial
}

func leafTemplate(tb testing.TB, fqdn string, notBefore, notAfter time.Time) *x509.Certificate {
	tb.Helper()
	return &x509.Certificate{
		SerialNumber: newSerial(tb),
		Subject:      pkix.Name{CommonName: fqdn},
		DNSNames:     []string{fqdn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

func caTemplate(tb testing.TB, name string, notBefore, notAfter time.Time) *x509.Certificate {
	tb.Helper()
	return &x509.Certificate{
		SerialNumber:          newSerial(tb),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
}

// sign issues tmpl for pub, signed by signer as parent; a nil parent makes the
// certificate self-signed.
func sign(tb testing.TB, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
	tb.Helper()
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		tb.Fatalf("tlscerttest: create certificate %q: %v", tmpl.Subject.CommonName, err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("tlscerttest: parse certificate %q: %v", tmpl.Subject.CommonName, err)
	}
	return c
}

// writePair writes the certificates ders, in order, and key as tls.crt and tls.key
// in dir.
func writePair(tb testing.TB, dir string, key *ecdsa.PrivateKey, ders ...[]byte) (certFile, keyFile string) {
	tb.Helper()
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		tb.Fatalf("tlscerttest: marshal key: %v", err)
	}
	var certPEM bytes.Buffer
	for _, der := range ders {
		if err := pem.Encode(&certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			tb.Fatalf("tlscerttest: encode certificate: %v", err)
		}
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write(tb, certFile, certPEM.Bytes())
	write(tb, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func write(tb testing.TB, path string, data []byte) {
	tb.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatalf("tlscerttest: write %s: %v", path, err)
	}
}
