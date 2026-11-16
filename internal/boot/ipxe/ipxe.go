package ipxe

import (
	"bytes"
	"crypto/x509"
	_ "embed" // the binaries are compiled into the Go binary
	"encoding/pem"
	"errors"
	"fmt"
)

// Names the vendored proxy hands to clients. The HTTP names are relative to /tftp/.
const (
	// TFTPBIOS is the BIOS (x86 PCBIOS) iPXE binary.
	TFTPBIOS = "undionly.kpxe"
	// TFTPAMD64 is the amd64 EFI iPXE binary.
	TFTPAMD64 = "snp.efi"
	// TFTPARM64 is the arm64 EFI iPXE binary.
	TFTPARM64 = "snp-arm64.efi"
	// HTTPAMD64 is the amd64 EFI iPXE binary as requested over HTTP.
	HTTPAMD64 = "amd64/snp.efi"
	// HTTPARM64 is the arm64 EFI iPXE binary as requested over HTTP.
	HTTPARM64 = "arm64/snp.efi"
)

//go:embed bin/undionly.kpxe
var undionlyKPXE []byte

//go:embed bin/snp-amd64.efi
var snpAMD64 []byte

//go:embed bin/snp-arm64.efi
var snpARM64 []byte

// rootsPEM holds the certificates the binaries were built to trust, in the order
// of build/ipxe/roots.txt.
//
//go:embed bin/roots.pem
var rootsPEM []byte

// Files returns the embedded binaries keyed by the names clients request. The
// amd64 and arm64 EFI binaries are each served under a TFTP and an HTTP name.
func Files() (map[string][]byte, error) {
	return map[string][]byte{
		TFTPBIOS:  undionlyKPXE,
		TFTPAMD64: snpAMD64,
		HTTPAMD64: snpAMD64,
		TFTPARM64: snpARM64,
		HTTPARM64: snpARM64,
	}, nil
}

// TrustedRoots returns the certificates the embedded binaries trust: the only
// roots an HTTPS certificate served to them can chain to.
func TrustedRoots() ([]*x509.Certificate, error) { return parseRoots(rootsPEM) }

// parseRoots parses a PEM bundle of root certificates. It fails on a block that
// is not a CERTIFICATE, on a certificate that does not parse, on data after the
// last block, and on a bundle with no certificate.
func parseRoots(b []byte) ([]*x509.Certificate, error) {
	var roots []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("ipxe: roots.pem has a block type %q, want CERTIFICATE", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("ipxe: parse root %d: %w", len(roots)+1, err)
		}
		roots = append(roots, c)
	}
	if len(roots) == 0 {
		return nil, errors.New("ipxe: roots.pem has no certificates")
	}
	if len(bytes.TrimSpace(b)) > 0 {
		return nil, errors.New("ipxe: roots.pem has trailing data after the last certificate")
	}
	return roots, nil
}
