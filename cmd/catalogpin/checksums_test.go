package main

import (
	"slices"
	"strings"
	"testing"
)

const (
	sum256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sum1   = "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	sumMD5 = "d41d8cd98f00b204e9800998ecf8427e"
	sumB2  = "786a02f742015903c6c6fd852552d272912f4740e15847618a86e217f71f5419d25e1031afee585313896444934eb04b903a685b1448b755d56f701afe9be2ce"
)

// clonezillaList is shaped like Clonezilla's CHECKSUMS.TXT: one section per
// algorithm, the same name in each.
const clonezillaList = "### MD5SUMS:\n" + sumMD5 + "  clonezilla-live-3.3.3-37-amd64.zip\n" +
	"### SHA1SUMS:\n" + sum1 + "  clonezilla-live-3.3.3-37-amd64.zip\n" +
	"### SHA256SUMS:\n" + sum256 + "  clonezilla-live-3.3.3-37-amd64.zip\n" +
	"### B2SUMS:\n" + sumB2 + "  clonezilla-live-3.3.3-37-amd64.zip\n"

var checksumCases = []struct {
	name   string
	data   string
	algo   string
	file   string
	want   string
	wantOK bool
}{
	{"sha256 section of a mixed list", clonezillaList, "sha256", "clonezilla-live-3.3.3-37-amd64.zip", sum256, true},
	{"sha1 section of a mixed list", clonezillaList, "sha1", "clonezilla-live-3.3.3-37-amd64.zip", sum1, true},
	{"binary-mode star", sum256 + " *tool.iso\n", "sha256", "tool.iso", sum256, true},
	{"leading ./dir path", sum256 + "  ./dir/tool.iso\n", "sha256", "tool.iso", sum256, true},
	{"upper-case hex returns lower case", strings.ToUpper(sum256) + "  tool.iso\n", "sha256", "tool.iso", sum256, true},
	{"CRLF line ends", sum256 + "  tool.iso\r\n", "sha256", "tool.iso", sum256, true},
	{"the ShredOS sha1 file", sum1 + "  shredos-2025.11_32_x86-64_v0.43.1_20261010.iso\n", "sha1", "shredos-2025.11_32_x86-64_v0.43.1_20261010.iso", sum1, true},
	{"two lines, one digest", sum256 + "  tool.iso\n" + sum256 + "  tool.iso\n", "sha256", "tool.iso", sum256, true},
	{"name absent", sum256 + "  other.iso\n", "sha256", "tool.iso", "", false},
	{"name is only a suffix of another", sum256 + "  mytool.iso\n", "sha256", "tool.iso", "", false},
	{"no line of the algorithm's length", sum1 + "  tool.iso\n", "sha256", "tool.iso", "", false},
	{"same name, two digests", sum256 + "  tool.iso\n" + strings.Repeat("a", 64) + "  tool.iso\n", "sha256", "tool.iso", "", false},
	{"not hex", strings.Repeat("g", 64) + "  tool.iso\n", "sha256", "tool.iso", "", false},
	{"unknown algorithm", sumMD5 + "  tool.iso\n", "md5", "tool.iso", "", false},
	{"empty list", "", "sha256", "tool.iso", "", false},
}

func TestParseChecksums(t *testing.T) {
	for _, tt := range checksumCases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseChecksums([]byte(tt.data), tt.algo, tt.file)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseChecksums(%q, %q) = %q, %t, want %q, %t", tt.algo, tt.file, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func FuzzParseChecksums(f *testing.F) {
	for _, tt := range checksumCases {
		f.Add([]byte(tt.data), tt.algo, tt.file)
	}
	f.Fuzz(func(t *testing.T, data []byte, algo, name string) {
		got, ok := parseChecksums(data, algo, name)
		if !ok {
			return
		}
		want := map[string]int{"sha256": 64, "sha1": 40}[algo]
		if len(got) != want || strings.Trim(got, "0123456789abcdef") != "" {
			t.Errorf("parseChecksums(%q, %q) = %q: not %d lower-case hex digits", algo, name, got, want)
		}
	})
}

func TestProvenanceFingerprints(t *testing.T) {
	const (
		primary = "0FF11AF081E98345594812037091115F8320B897"
		subkey  = "62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9"
		clone   = "54C0821A48715DAFD61BFCAF667857D045599AFD"
	)
	md := "# Provenance\n\n" +
		"| entry | primary | signing subkey |\n|---|---|---|\n" +
		"| systemrescue | `" + primary + "` | " + subkey + " |\n" +
		"| clonezilla | **" + clone + "**, again " + clone + " | |\n" +
		"lower case 0ff11af081e98345594812037091115f8320b897 is not a token\n" +
		"too long " + primary + "0 and 64 hex " + strings.Repeat("A", 64) + " are not tokens\n" +
		"too short " + primary[:39] + "\n"
	got := provenanceFingerprints([]byte(md))
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{clone, primary, subkey}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("provenanceFingerprints = %v, want %v", keys, want)
	}
	if got := provenanceFingerprints(nil); len(got) != 0 {
		t.Errorf("provenanceFingerprints(nil) = %v, want none", got)
	}
}
