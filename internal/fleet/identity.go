package fleet

import (
	"errors"
	"net"
	"strings"
	"unicode"
)

// uuidLen is the length of a canonical 8-4-4-4-12 UUID.
const uuidLen = 36

// NormalizeUUID trims and lowercases raw and returns it when it is an
// 8-4-4-4-12 hex UUID, or "" when it is not. Validation is ASCII-only and runs
// before lowercasing, so no Unicode case folding can produce a valid UUID.
func NormalizeUUID(raw string) string {
	u := strings.TrimSpace(raw)
	if len(u) != uuidLen {
		return ""
	}
	for i := range len(u) {
		switch i {
		case 8, 13, 18, 23:
			if u[i] != '-' {
				return ""
			}
		default:
			if !isHex(u[i]) {
				return ""
			}
		}
	}
	return strings.ToLower(u)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// JunkPattern reports whether a NormalizeUUID result is useless as a machine
// key: empty, all zeros or all f. A UUID reported with two different
// serial/product pairs is also junk, but that needs stored data and is decided
// elsewhere.
func JunkPattern(uuid string) bool {
	digits := strings.ReplaceAll(uuid, "-", "")
	return strings.Trim(digits, "0") == "" || strings.Trim(digits, "f") == ""
}

// NormalizeMAC parses raw (colon, dash or dotted form) and returns the
// lowercase colon form. It rejects anything that is not a 48-bit MAC; the
// input is left out of the error because it comes from the network.
func NormalizeMAC(raw string) (string, error) {
	hw, err := net.ParseMAC(raw)
	if err != nil {
		return "", errors.New("invalid MAC address")
	}
	if len(hw) != 6 {
		return "", errors.New("must be a 48-bit MAC")
	}
	return hw.String(), nil
}

// CleanText removes NUL bytes and invalid UTF-8 from s. Neither database driver
// stores a string holding NUL, so text that came from the network goes through
// it before it is stored.
func CleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
}

// PrintableText returns s without invalid UTF-8 and without any rune that
// unicode.IsGraphic rejects: control, format and separator runes, tab, CR and
// LF included. The ASCII space stays. Text that came from the network goes
// through it before it reaches a log, a database row or a terminal.
func PrintableText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsGraphic(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, ""))
}
