package tlsacmetest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"go.jacaudi.dev/hephaestus/internal/tlsacme"
)

// SelfSigned is a tlsacme.Issuer that issues self-signed certificates without
// any network call.
type SelfSigned struct {
	// NotBefore and NotAfter are the validity of the issued certificate.
	NotBefore, NotAfter time.Time
	// Domain is the name the certificate covers; empty means the requested domain.
	Domain string
	// Err, when set, is returned by every Obtain.
	Err error
	// Malformed makes Obtain return bytes that are not PEM.
	Malformed bool
	// Block, when non-nil, makes Obtain wait until it is closed, ignoring the
	// context as lego's Obtain does.
	Block <-chan struct{}
	// Calls counts the Obtain calls.
	Calls atomic.Int64
}

// Obtain counts the call, then returns the configured outcome: it waits on
// Block, returns Err, returns malformed bytes, or issues a self-signed ECDSA
// P-256 certificate for Domain (or domain) with PEM encoding (CERTIFICATE,
// PRIVATE KEY PKCS#8).
func (s *SelfSigned) Obtain(_ context.Context, domain string) (tlsacme.Certificate, error) {
	s.Calls.Add(1)
	if s.Block != nil {
		<-s.Block
	}
	if s.Err != nil {
		return tlsacme.Certificate{}, s.Err
	}
	if s.Malformed {
		return tlsacme.Certificate{ChainPEM: []byte("not a certificate"), KeyPEM: []byte("not a key")}, nil
	}
	name := s.Domain
	if name == "" {
		name = domain
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsacme.Certificate{}, fmt.Errorf("tlsacmetest: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tlsacme.Certificate{}, fmt.Errorf("tlsacmetest: serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    s.NotBefore,
		NotAfter:     s.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tlsacme.Certificate{}, fmt.Errorf("tlsacmetest: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tlsacme.Certificate{}, fmt.Errorf("tlsacmetest: marshal key: %w", err)
	}
	return tlsacme.Certificate{
		ChainPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}
