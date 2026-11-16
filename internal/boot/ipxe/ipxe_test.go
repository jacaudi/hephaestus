package ipxe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	// ipxeDefaultCA is the fingerprint of iPXE's stock trust anchor, which a hephaestus build must not carry.
	ipxeDefaultCA = "9faf717b7f8ca2f93c256c79f8ac5591895d66d1ff3bee6397a70d29c65eed1a"

	dockerfilePath = "../../../build/ipxe/Dockerfile"
	embedPath      = "../../../build/ipxe/embed.ipxe"
	rootsPath      = "../../../build/ipxe/roots.txt"
)

// isrg is the oracle: the DER sha256 fingerprints of ISRG Root X1, ISRG Root X2, Root YR and Root YE, in trust order.
// It is a literal on purpose, independent of the build inputs it checks.
func isrg() []string {
	return []string{
		"96bcec06264976f37460779acf28c5a7cfe8a3c0aae11a8ffcee05c0bddf08c6",
		"69729b8e15a86efc177a57afb7171dfc64add28c2fca8cf1507e34453ccb1470",
		"e57b7e6f150c419102e8d5c055729ff967b9d1a829bf00cec89ca604ebf4a86f",
		"e14ffcad5b0025731006caa43a121a22d8e9700f4fb9cf852f02a708aa5d5666",
	}
}

// binaries maps each shipped binary name to the Files key that serves it.
func binaries() map[string]string {
	return map[string]string{
		"undionly.kpxe": TFTPBIOS,
		"snp-amd64.efi": TFTPAMD64,
		"snp-arm64.efi": TFTPARM64,
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func files(t *testing.T) map[string][]byte {
	t.Helper()
	got, err := Files()
	if err != nil {
		t.Fatalf("Files() error = %v", err)
	}
	return got
}

// buildInfoValue returns the value of the single "key: value" line in BUILDINFO.
func buildInfoValue(t *testing.T, info, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(info, "\n") {
		if v, ok := strings.CutPrefix(line, key+": "); ok {
			return v
		}
	}
	t.Fatalf("BUILDINFO has no %q line", key)
	return ""
}

func TestBinariesMatchSHA256SUMS(t *testing.T) {
	embedded := files(t)
	lines := strings.Split(strings.TrimSuffix(string(readFile(t, "bin/SHA256SUMS")), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("SHA256SUMS has %d lines, want 3: %q", len(lines), lines)
	}

	want := []string{"undionly.kpxe", "snp-amd64.efi", "snp-arm64.efi"}
	for i, line := range lines {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || name != want[i] {
			t.Errorf("SHA256SUMS line %d = %q, want \"<hex>  %s\"", i+1, line, want[i])
			continue
		}
		if got := sha256Hex(embedded[binaries()[name]]); got != sum {
			t.Errorf("%s: embedded sha256 = %s, SHA256SUMS says %s", name, got, sum)
		}
	}
}

func TestRootsTxtIsISRG(t *testing.T) {
	var got []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(readFile(t, rootsPath))), "\n") {
		cols := strings.Fields(line)
		if len(cols) != 4 {
			t.Fatalf("roots.txt line %q has %d columns, want 4", line, len(cols))
		}
		got = append(got, cols[3])
	}
	if !slices.Equal(got, isrg()) {
		t.Errorf("roots.txt fingerprints = %v, want %v", got, isrg())
	}
}

func TestEFITrustIsISRG(t *testing.T) {
	trust := decodeHex(t, strings.Join(isrg(), ""))
	stock := decodeHex(t, ipxeDefaultCA)
	embedded := files(t)

	for _, name := range []string{"snp-amd64.efi", "snp-arm64.efi"} {
		bin := embedded[binaries()[name]]
		if n := bytes.Count(bin, trust); n != 1 {
			t.Errorf("%s: ISRG fingerprints appear %d times, want 1", name, n)
		}
		if bytes.Contains(bin, stock) {
			t.Errorf("%s: contains iPXE's default CA fingerprint", name)
		}
	}
}

func TestBuildInfoTrust(t *testing.T) {
	info := string(readFile(t, "bin/BUILDINFO"))
	want := strings.Join(isrg(), "")

	for name := range binaries() {
		line := "trust " + name + " " + want
		if !strings.Contains(info, line+"\n") {
			t.Errorf("BUILDINFO has no line %q", line)
		}
	}
}

func TestBuildInfoPins(t *testing.T) {
	info := string(readFile(t, "bin/BUILDINFO"))
	dockerfile := string(readFile(t, dockerfilePath))

	for _, pin := range []struct{ buildInfoKey, arg string }{
		{"ipxe-commit", "IPXE_COMMIT"},
		{"ipxe-ancestor", "IPXE_TLS13_ANCESTOR"},
	} {
		m := regexp.MustCompile(`(?m)^ARG ` + pin.arg + `=(\S+)$`).FindStringSubmatch(dockerfile)
		if m == nil {
			t.Errorf("Dockerfile has no ARG %s", pin.arg)
			continue
		}
		if got := buildInfoValue(t, info, pin.buildInfoKey); got != m[1] {
			t.Errorf("BUILDINFO %s = %s, Dockerfile %s = %s", pin.buildInfoKey, got, pin.arg, m[1])
		}
	}

	if got, want := buildInfoValue(t, info, "embed-sha256"), sha256Hex(readFile(t, embedPath)); got != want {
		t.Errorf("BUILDINFO embed-sha256 = %s, embed.ipxe hashes to %s", got, want)
	}
}

func TestEmbeddedScriptInEFI(t *testing.T) {
	script := readFile(t, embedPath)
	embedded := files(t)

	for _, name := range []string{"snp-amd64.efi", "snp-arm64.efi"} {
		if !bytes.Contains(embedded[binaries()[name]], script) {
			t.Errorf("%s does not contain build/ipxe/embed.ipxe", name)
		}
	}
}

func TestFiles(t *testing.T) {
	got := files(t)

	wantKeys := []string{"amd64/snp.efi", "arm64/snp.efi", "snp-arm64.efi", "snp.efi", "undionly.kpxe"}
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, wantKeys) {
		t.Fatalf("Files() keys = %v, want %v", keys, wantKeys)
	}
	for name, b := range got {
		if len(b) == 0 {
			t.Errorf("Files()[%q] is empty", name)
		}
	}
	if !bytes.Equal(got["snp.efi"], got["amd64/snp.efi"]) {
		t.Error("snp.efi and amd64/snp.efi differ")
	}
	if !bytes.Equal(got["snp-arm64.efi"], got["arm64/snp.efi"]) {
		t.Error("snp-arm64.efi and arm64/snp.efi differ")
	}
	if bytes.Equal(got["snp.efi"], got["snp-arm64.efi"]) {
		t.Error("the amd64 and arm64 binaries are identical")
	}
}

// TestTrustedRoots pins that bin/roots.pem holds exactly the roots the binaries
// were built with: the ISRG oracle, in trust order (so also roots.txt and the
// BUILDINFO trust lines), each a CA.
func TestTrustedRoots(t *testing.T) {
	roots, err := TrustedRoots()
	if err != nil {
		t.Fatalf("TrustedRoots() error = %v", err)
	}

	var got []string
	for i, c := range roots {
		got = append(got, sha256Hex(c.Raw))
		if !c.IsCA {
			t.Errorf("root %d (%s) is not a CA", i, c.Subject)
		}
	}
	if !slices.Equal(got, isrg()) {
		t.Errorf("TrustedRoots() fingerprints = %v, want %v", got, isrg())
	}
}

func TestParseRoots(t *testing.T) {
	good := string(readFile(t, "bin/roots.pem"))
	block := func(typ string) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte("x")}))
	}

	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty", "", "no certificates"},
		{"text only", "not pem\n", "no certificates"},
		{"private key block", block("PRIVATE KEY"), `block type "PRIVATE KEY"`},
		{"certificate then key", good + block("PRIVATE KEY"), `block type "PRIVATE KEY"`},
		{"trailing garbage", good + "garbage\n", "trailing data"},
		{"unparsable certificate", block("CERTIFICATE"), "parse root 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roots, err := parseRoots([]byte(tt.in))

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseRoots() = %d roots, error %v, want an error containing %q", len(roots), err, tt.wantErr)
			}
		})
	}
}
