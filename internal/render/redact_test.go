package render

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
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

func FuzzRedact(f *testing.F) {
	f.Add([]byte("k: abc\n"), "abc")
	f.Add([]byte("a: RED\nb: E\nc: 0c9f\n"), "E")
	f.Add([]byte("password: 'p@dm1n''sPass'\n"), "p@dm1n'sPass")
	f.Add([]byte("key: |\n  -----BEGIN KEY-----\n  MHcCAQEEIEC1WIJb\n  -----END KEY-----\n"), "-----BEGIN KEY-----\nMHcCAQEEIEC1WIJb\n-----END KEY-----\n")
	f.Add([]byte("k: Pj4+Pz8/eA==\nu: Pj4-Pz8_eA\nh: 3e3e3e3f3f3f78\n"), ">>>???x")
	f.Add([]byte("a: [unclosed\n"), "secret-value")
	f.Add([]byte(Marker+": "+Marker+"\n"), "RED")

	f.Fuzz(func(t *testing.T, render []byte, value string) {
		got, err := Redact(render, map[string]string{refPassword: value})
		if err != nil || value == "" {
			return
		}

		for _, scalar := range stringValues(got) {
			for piece := range strings.SplitSeq(scalar, Marker) {
				if strings.Contains(piece, value) {
					t.Fatalf("Redact(%q, %q) = %q: the decoded scalar %q holds the value outside a marker", render, value, got, scalar)
				}
			}
		}
	})
}
