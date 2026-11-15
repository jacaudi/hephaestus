package ipxe

import (
	_ "embed" // the binaries are compiled into the Go binary
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
