package render

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const refPassword = "ref+op://lab/admin/password"

func redact(t *testing.T, render string, values map[string]string) string {
	t.Helper()

	got, err := Redact([]byte(render), values)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	return string(got)
}

// indent prefixes every line of text with the given indentation.
func indent(text, prefix string) string {
	var out strings.Builder

	for line := range strings.Lines(text) {
		out.WriteString(prefix + line)
	}

	return out.String()
}

func TestRedact_MultiLineValue(t *testing.T) {
	const armourBegin, armourEnd = "-----BEGIN EC PRIVATE KEY-----", "-----END EC PRIVATE KEY-----"

	value := armourBegin + "\nMHcCAQEEIEC1WIJbm7Y3gAKSbl3x\nOhEsL9uVkYoaA2cM5lPYtu6S\nQgAEwAzkRIZG1s8ssGQ4jnEHoDxD\n" + armourEnd + "\n"
	render := "key: |\n" + indent(value, "  ")

	got := redact(t, render, map[string]string{refPassword: value})

	if !strings.Contains(got, armourBegin) || !strings.Contains(got, armourEnd) {
		t.Errorf("Redact = %q, want both armour lines kept", got)
	}

	for line := range strings.Lines(strings.ReplaceAll(got, Marker, "")) {
		line = strings.TrimSpace(line)
		if len(line) >= 8 && line != "key: |" && line != armourBegin && line != armourEnd {
			t.Errorf("line %q of 8 or more characters remains in %q", line, got)
		}
	}

	if n := strings.Count(got, Marker); n != 3 {
		t.Errorf("Marker count = %d, want 3 (one per body line) in %q", n, got)
	}
}

func TestRedact_Base64AndHex(t *testing.T) {
	// ">>>???x" encodes with "+", "/" and padding in the standard form and with "-" and "_" in the URL form.
	const value = ">>>???x"

	raw := []byte(value)
	forms := []struct {
		name, text string
	}{
		{"std", base64.StdEncoding.EncodeToString(raw)},
		{"raw std", base64.RawStdEncoding.EncodeToString(raw)},
		{"url", base64.URLEncoding.EncodeToString(raw)},
		{"raw url", base64.RawURLEncoding.EncodeToString(raw)},
		{"hex", hex.EncodeToString(raw)},
	}

	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			got := redact(t, "k: "+form.text+"\n", map[string]string{refPassword: value})

			if want := "k: " + Marker + "\n"; got != want {
				t.Errorf("Redact(%q) = %q, want %q", form.text, got, want)
			}
		})
	}
}

func TestRedact_LongestFirst(t *testing.T) {
	got := redact(t, "k: abcdefghijkl\n", map[string]string{"ref+op://a/b/short": "abcdefgh", "ref+op://a/b/long": "abcdefghijkl"})

	if want := "k: " + Marker + "\n"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedact_YAMLQuoted(t *testing.T) {
	const value = "p@dm1n'sPass"

	_, err := Redact([]byte("password: 'p@dm1n''sPass'\n"), map[string]string{refPassword: value})

	redaction, ok := errors.AsType[*RedactionError](err)
	if !ok {
		t.Fatalf("Redact error = %v, want a *RedactionError", err)
	}

	if redaction.Ref != refPassword {
		t.Errorf("Ref = %q, want %q", redaction.Ref, refPassword)
	}
}

func TestRedact_MappingKeyChecked(t *testing.T) {
	_, err := Redact([]byte("'it''s-a-secret': 1\n"), map[string]string{refPassword: "it's-a-secret"})

	if redaction, ok := errors.AsType[*RedactionError](err); !ok || redaction.Ref != refPassword {
		t.Fatalf("Redact error = %v, want a *RedactionError for %s", err, refPassword)
	}
}

func TestRedact_ShortValueRedacted(t *testing.T) {
	got := redact(t, "k: abc\n", map[string]string{refPassword: "abc"})

	if want := "k: " + Marker + "\n"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestRedact_ValueInsideMarker(t *testing.T) {
	values := map[string]string{"ref+op://a/b/one": "RED", "ref+op://a/b/two": "E", "ref+op://a/b/three": "0c9f"}

	got := redact(t, "a: RED\nb: E\nc: 0c9f\n", values)

	if want := "a: " + Marker + "\nb: " + Marker + "\nc: " + Marker + "\n"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}

	if n := strings.Count(got, Marker); n != 3 {
		t.Errorf("Marker count = %d, want 3", n)
	}
}

func TestRedact_EmptyValueIgnored(t *testing.T) {
	const render = "a: b\n"

	got := redact(t, render, map[string]string{refPassword: ""})

	if got != render {
		t.Errorf("Redact = %q, want it unchanged: %q", got, render)
	}
}

func TestRedact_UnparsableOutput(t *testing.T) {
	_, err := Redact([]byte("a: [unclosed\n"), map[string]string{refPassword: "secret-value"})

	if !errors.Is(err, ErrRedaction) {
		t.Errorf("Redact error = %v, want ErrRedaction", err)
	}
}

func TestRedact_ErrorNamesReference(t *testing.T) {
	const value = "p@dm1n'sPass"

	_, err := Redact([]byte("password: 'p@dm1n''sPass'\n"), map[string]string{refPassword: value})

	redaction, ok := errors.AsType[*RedactionError](err)
	if !ok {
		t.Fatalf("Redact error = %v, want a *RedactionError", err)
	}

	if redaction.Ref != refPassword {
		t.Errorf("Ref = %q, want %q", redaction.Ref, refPassword)
	}

	if !errors.Is(err, ErrRedaction) {
		t.Errorf("errors.Is(%v, ErrRedaction) = false, want true", err)
	}

	if strings.Contains(err.Error(), value) {
		t.Errorf("error text %q holds the value", err.Error())
	}

	if want := "render: the value of " + refPassword + " survived redaction"; err.Error() != want {
		t.Errorf("error text = %q, want %q", err.Error(), want)
	}
}

func TestRedact_ValueHoldingMarkerRefused(t *testing.T) {
	const secret = "s3cr3t-part-one"

	tests := []struct {
		name, value, render string
	}{
		{"whole value", secret + Marker + "s3cr3t-part-two", "k: " + secret + Marker + "s3cr3t-part-two\n"},
		{"line of a multi-line value", "first-line-is-long\n" + secret + Marker + "tail-of-line\n", "k: x\n"},
		{"value is the marker", Marker, "k: x\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Redact([]byte(tt.render), map[string]string{refPassword: tt.value})

			redaction, ok := errors.AsType[*RedactionError](err)
			if !ok {
				t.Fatalf("Redact = %q, %v, want a *RedactionError", got, err)
			}

			if !errors.Is(err, ErrRedaction) || redaction.Ref != refPassword {
				t.Errorf("error = %v, want ErrRedaction naming %s", err, refPassword)
			}

			if strings.Contains(err.Error(), secret) {
				t.Errorf("error text %q holds the value", err.Error())
			}
		})
	}
}

func TestRedact_SurvivorNamesFirstPatternReference(t *testing.T) {
	const render = "a: 'p@ss''one'\nb: 'p@ss''two-longer'\n"

	values := map[string]string{"ref+op://a/b/short": "p@ss'one", "ref+op://a/b/long": "p@ss'two-longer"}

	// Both values survive; the longest pattern comes first, whichever key the decoder yields first.
	for range 100 {
		_, err := Redact([]byte(render), values)

		if redaction, ok := errors.AsType[*RedactionError](err); !ok || redaction.Ref != "ref+op://a/b/long" {
			t.Fatalf("Redact error = %v, want a *RedactionError for ref+op://a/b/long", err)
		}
	}
}

// outsideMarker reports whether s holds an occurrence of pat that is not wholly inside an occurrence of Marker.
func outsideMarker(s, pat string) bool {
	for i := range len(s) - len(pat) + 1 {
		if s[i:i+len(pat)] != pat {
			continue
		}

		inside := false

		for m := max(0, i+len(pat)-len(Marker)); m <= i && !inside; m++ {
			inside = strings.HasPrefix(s[m:], Marker)
		}

		if !inside {
			return true
		}
	}

	return false
}

// leakedRef returns the reference of a value with a pattern outside a Marker in out, as text or in a decoded scalar or
// key.
func leakedRef(out []byte, values map[string]string) (string, bool) {
	texts := []string{string(out)}
	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc any

		if dec.Decode(&doc) != nil {
			break
		}

		texts = appendDecoded(texts, doc)
	}

	for ref, value := range values {
		for _, pat := range valuePatterns(value) {
			for _, s := range texts {
				if outsideMarker(s, pat) {
					return ref, true
				}
			}
		}
	}

	return "", false
}

// requireRedacted runs Redact and fails unless it either refuses with ErrRedaction, naming no value, or returns an
// output in which no value has a pattern outside a Marker.
func requireRedacted(t *testing.T, render string, values map[string]string) []byte {
	t.Helper()

	got, err := Redact([]byte(render), values)
	if err != nil {
		if !errors.Is(err, ErrRedaction) {
			t.Fatalf("Redact error = %v, want ErrRedaction", err)
		}

		for _, value := range values {
			if strings.Contains(err.Error(), value) {
				t.Errorf("error text %q holds a value", err.Error())
			}
		}

		return nil
	}

	if ref, found := leakedRef(got, values); found {
		t.Fatalf("Redact = %q, <nil>: the value of %s remains outside a marker", got, ref)
	}

	return got
}

func TestRedact_StraddleSingleValue(t *testing.T) {
	// The value ends with the first byte of a Marker that follows it.
	requireRedacted(t, "k: abcdefgH"+Marker[1:]+"\n", map[string]string{refPassword: "abcdefgH"})
}

func TestRedact_StraddleAdjacentValues(t *testing.T) {
	// The two values share the "H" where they meet, so redacting one leaves the other next to a Marker.
	requireRedacted(t, "k: secretXHabcdefghij\n", map[string]string{"ref+op://a/b/one": "secretXH", "ref+op://a/b/two": "Habcdefghij"})
}

func TestRedact_ValueInsideMarkerAccepted(t *testing.T) {
	values := map[string]string{refPassword: "RED"}

	// "RED" also lies inside the Marker of b: that occurrence is the marker's own text.
	got := redact(t, "a: RED\nb: "+Marker+"\n", values)

	if !strings.HasPrefix(got, "a: "+Marker+"\n") {
		t.Errorf("Redact = %q, want the value of a replaced by %s", got, Marker)
	}

	if ref, found := leakedRef([]byte(got), values); found {
		t.Errorf("Redact = %q: the value of %s remains outside a marker", got, ref)
	}
}

func TestRedact_PatternFormedAtMarkerEdgeRefused(t *testing.T) {
	const refTwo = "ref+op://a/b/two"

	// Replacing "zzzz" puts the Marker's last byte "f" before "Zq": the second value appears in a comment.
	_, err := Redact([]byte("k: v # zzzzZq\n"), map[string]string{"ref+op://a/b/one": "zzzz", refTwo: "fZq"})

	if redaction, ok := errors.AsType[*RedactionError](err); !ok || redaction.Ref != refTwo {
		t.Fatalf("Redact error = %v, want a *RedactionError for %s", err, refTwo)
	}
}

func FuzzRedact(f *testing.F) {
	f.Add([]byte("k: abc\n"), "abc", "")
	f.Add([]byte("a: RED\nb: E\nc: 0c9f\n"), "E", "0c9f")
	f.Add([]byte("password: 'p@dm1n''sPass'\n"), "p@dm1n'sPass", "")
	f.Add([]byte("key: |\n  -----BEGIN KEY-----\n  MHcCAQEEIEC1WIJb\n  -----END KEY-----\n"), "-----BEGIN KEY-----\nMHcCAQEEIEC1WIJb\n-----END KEY-----\n", "")
	f.Add([]byte("k: Pj4+Pz8/eA==\nu: Pj4-Pz8_eA\nh: 3e3e3e3f3f3f78\n"), ">>>???x", "")
	f.Add([]byte("a: [unclosed\n"), "secret-value", "")
	f.Add([]byte(Marker+": "+Marker+"\n"), "RED", "")
	f.Add([]byte("k: s3cr3t-one"+Marker+"s3cr3t-two\n"), "s3cr3t-one"+Marker+"s3cr3t-two", "")
	f.Add([]byte("k: abcdefgH"+Marker[1:]+"\n"), "abcdefgH", "")
	f.Add([]byte("k: secretXHabcdefghij\n"), "secretXH", "Habcdefghij")
	f.Add([]byte("k: v # zzzzZq\n"), "zzzz", "fZq")

	f.Fuzz(func(t *testing.T, render []byte, value, other string) {
		values := map[string]string{refPassword: value, "ref+op://lab/admin/other": other}

		got, err := Redact(render, values)

		// A value that holds the marker cannot be told from a marker once replaced, so it is always refused.
		if (strings.Contains(value, Marker) || strings.Contains(other, Marker)) && err == nil {
			t.Fatalf("Redact(%q, %q) = %q, want an error: a value holds the marker", render, values, got)
		}

		if err != nil {
			return
		}

		if ref, found := leakedRef(got, values); found {
			t.Fatalf("Redact(%q, %q) = %q: the value of %s remains outside a marker", render, values, got, ref)
		}
	})
}
