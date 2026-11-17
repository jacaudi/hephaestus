package main

import (
	"crypto/sha1" //nolint:gosec // G505: ShredOS publishes only a sha1 list; the pin records sha256 and the list check is one of two checks, not the pin's identity
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"path"
	"strings"
)

// Checksum algorithms a catalog.Signature.ChecksumAlgo names, and the length
// of the hex digest each one produces.
const (
	algoSHA256 = "sha256"
	algoSHA1   = "sha1"

	sha256HexLen = 64
	sha1HexLen   = 40

	// sectionMark opens a section header line of a checksum list.
	sectionMark = "###"
)

// hashers computes, in one pass, the digests a download is checked with: the
// sha256 every pin records and the sha1 some upstream lists carry.
type hashers struct{ sha256, sha1 hash.Hash }

func newHashers() hashers {
	return hashers{sha256: sha256.New(), sha1: sha1.New()} //nolint:gosec // G401: sha1 only matches an upstream list that offers nothing stronger; see the import
}

// writer returns the writer that feeds both digests.
func (h hashers) writer() io.Writer { return io.MultiWriter(h.sha256, h.sha1) }

// sum returns the hex digest for algo; any algorithm but sha1 gets the sha256.
func (h hashers) sum(algo string) string {
	if algo == algoSHA1 {
		return hex.EncodeToString(h.sha1.Sum(nil))
	}
	return hex.EncodeToString(h.sha256.Sum(nil))
}

// parseChecksums returns the lower-case digest the checksum list data gives
// for the file name. A line is "<hex>  <name>" or "<hex> *<name>"; the name
// may carry a leading path, of which only the last element is compared. Only
// a digest as long as algo's counts. A list with "### <ALGO>SUMS:" section
// headers (Clonezilla's and GParted's hold md5, sha1, sha256, sha512, blake2
// and blake3, and blake3 is as long as sha256) is read only in the section
// for algo; a list without headers is read whole. It returns false when no
// line names the file, when algo is unknown, and when two lines give the file
// different digests.
func parseChecksums(data []byte, algo, name string) (string, bool) {
	var want int
	switch algo {
	case algoSHA256:
		want = sha256HexLen
	case algoSHA1:
		want = sha1HexLen
	default:
		return "", false
	}
	text := string(data)
	sectioned := strings.HasPrefix(text, sectionMark) || strings.Contains(text, "\n"+sectionMark)
	inSection := !sectioned
	found := ""
	for line := range strings.Lines(text) {
		if header, ok := strings.CutPrefix(line, sectionMark); ok {
			inSection = strings.EqualFold(strings.Trim(header, " \t\r\n:"), algo+"SUMS")
			continue
		}
		if !inSection {
			continue
		}
		sum, entry, ok := parseChecksumLine(line)
		if !ok || len(sum) != want || path.Base(entry) != name {
			continue
		}
		if found != "" && found != sum {
			return "", false
		}
		found = sum
	}
	return found, found != ""
}

// parseChecksumLine splits one list line into its lower-case hex digest and
// the file name, and reports false for a line that is neither.
func parseChecksumLine(line string) (sum, name string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	i := strings.IndexAny(line, " \t")
	if i <= 0 || !isHex(line[:i]) {
		return "", "", false
	}
	name = strings.TrimPrefix(strings.TrimLeft(line[i:], " \t"), "*")
	return strings.ToLower(line[:i]), name, name != ""
}

func isHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return s != ""
}

// provenanceFingerprints returns every 40-hex-digit upper-case token of the
// PROVENANCE.md text md: the fingerprints the file names.
func provenanceFingerprints(md []byte) map[string]bool {
	out := make(map[string]bool)
	for tok := range strings.FieldsFuncSeq(string(md), func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
	}) {
		if isFingerprint(tok) {
			out[tok] = true
		}
	}
	return out
}
