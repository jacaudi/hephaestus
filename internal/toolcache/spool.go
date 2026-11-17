package toolcache

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SpoolName is the file name of a partial download of object n of the pin:
// pin, a dash and the object's index.
func SpoolName(pin string, n int) string {
	return pin + "-" + strconv.Itoa(n)
}

// ETagName is the name of the sidecar that holds the strong ETag a spool file
// was started under.
func ETagName(pin string, n int) string {
	return SpoolName(pin, n) + ".etag"
}

// strongETag reports whether v is a strong entity tag: a quoted string that
// is not weak (W/"..."). Only a strong tag proves a resumed range comes from
// the same bytes.
func strongETag(v string) bool {
	return len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`)
}

// errContentRange is the cause behind every parseContentRange failure.
var errContentRange = errors.New("not a satisfied bytes Content-Range with a known total")

// parseContentRange parses "bytes <start>-<end>/<total>" and returns
// 0 <= start <= end < total. The "bytes */<total>" and "bytes a-b/*" forms
// are refused: a resume needs the range and the total to check them.
func parseContentRange(h string) (start, end, total int64, err error) {
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("content-range %q: %w", h, errContentRange)
	}
	span, totalText, ok1 := strings.Cut(rest, "/")
	startText, endText, ok2 := strings.Cut(span, "-")
	if !ok1 || !ok2 {
		return 0, 0, 0, fmt.Errorf("content-range %q: %w", h, errContentRange)
	}
	start, ok1 = parseDigits(startText)
	end, ok2 = parseDigits(endText)
	total, ok3 := parseDigits(totalText)
	if !ok1 || !ok2 || !ok3 || start > end || end >= total {
		return 0, 0, 0, fmt.Errorf("content-range %q: %w", h, errContentRange)
	}
	return start, end, total, nil
}

// parseDigits parses a non-empty run of decimal digits. ParseInt alone would
// also take a sign.
func parseDigits(s string) (int64, bool) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}
