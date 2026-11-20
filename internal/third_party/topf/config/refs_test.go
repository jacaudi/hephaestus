// hephaestus: new

package config

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

const refToken = "ref+op://v/i/t"

// decodeAll decodes every document of content, as a patch consumer would.
func decodeAll(t *testing.T, content []byte) []any {
	t.Helper()

	var docs []any

	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs
			}

			t.Fatalf("decode %q: %v", content, err)
		}

		docs = append(docs, doc)
	}
}

func TestHasReferencePrefix(t *testing.T) {
	topfPattern := regexp.MustCompile(`^ref\+[a-zA-Z0-9]+://`)

	for _, in := range []string{
		"ref+op://v/i/f",
		"ref+vault://x",
		"ref+a1://",
		"ref+://x",
		"ref+a-b://x",
		"ref+op:/x",
		"ref+op//x",
		"ref+op",
		"ref+",
		"x ref+op://v/i/f",
		" ref+op://v/i/f",
		"REF+op://v/i/f",
		"ref+\u00e9://x",
		"ref+op://\nref+op://",
		"",
	} {
		if got, want := HasReferencePrefix(in), topfPattern.MatchString(in); got != want {
			t.Errorf("HasReferencePrefix(%q) = %t, want %t", in, got, want)
		}
	}
}

func TestSubstituteReferences_NoReferenceUnchanged(t *testing.T) {
	in := []byte("a: 1.0\n# c\n")

	out, err := SubstituteReferences(in, nil)
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	if !bytes.Equal(out, in) {
		t.Errorf("SubstituteReferences = %q, want %q", out, in)
	}
}

func TestSubstituteReferences_WholeScalarOnly(t *testing.T) {
	values := map[string]string{"ref+op://v/i/f": "value", "ref+op://k": "key"}

	tests := []struct {
		name string
		in   string
		want any
	}{
		{name: "whole scalar", in: "a: ref+op://v/i/f\n", want: map[string]any{"a": "value"}},
		{name: "quoted whole scalar", in: "a: \"ref+op://v/i/f\"\n", want: map[string]any{"a": "value"}},
		{name: "list item", in: "- ref+op://v/i/f\n", want: []any{"value"}},
		{name: "nested value", in: "a:\n  b:\n    - ref+op://v/i/f\n", want: map[string]any{"a": map[string]any{"b": []any{"value"}}}},
		{name: "prefix text is not a reference", in: "a: x ref+op://v/i/f\n", want: map[string]any{"a": "x ref+op://v/i/f"}},
		{name: "mapping key is not a reference", in: "ref+op://k: 1\nb: ref+op://v/i/f\n", want: map[string]any{"ref+op://k": 1, "b": "value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := SubstituteReferences([]byte(tt.in), values)
			if err != nil {
				t.Fatalf("SubstituteReferences: %v", err)
			}

			docs := decodeAll(t, out)
			if len(docs) != 1 {
				t.Fatalf("SubstituteReferences output holds %d documents, want 1: %q", len(docs), out)
			}

			gotYAML, err := yaml.Marshal(docs[0])
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}

			wantYAML, err := yaml.Marshal(tt.want)
			if err != nil {
				t.Fatalf("marshal want: %v", err)
			}

			if !bytes.Equal(gotYAML, wantYAML) {
				t.Errorf("SubstituteReferences = %q, want %q", gotYAML, wantYAML)
			}
		})
	}
}

func TestSubstituteReferences_NoReferenceInValueKeepsFileBytes(t *testing.T) {
	in := []byte("ref+op://k: 1\nb: 2\n")

	out, err := SubstituteReferences(in, map[string]string{"ref+op://k": "key"})
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	if !bytes.Equal(out, in) {
		t.Errorf("SubstituteReferences = %q, want %q", out, in)
	}
}

func TestSubstituteReferences_ValueStaysString(t *testing.T) {
	for _, value := range []string{"true", "0123", "null", "1e3", "~", "- x", "a: b", "# c", "'q'", "\"d\"", " lead", "trail ", "multi\nline", ""} {
		t.Run(value, func(t *testing.T) {
			out, err := SubstituteReferences([]byte("a: "+refToken+"\n"), map[string]string{refToken: value})
			if err != nil {
				t.Fatalf("SubstituteReferences: %v", err)
			}

			var got map[string]any
			if err := yaml.Unmarshal(out, &got); err != nil {
				t.Fatalf("decode %q: %v", out, err)
			}

			if s, ok := got["a"].(string); !ok || s != value {
				t.Errorf("value decodes as %#v, want the string %q (output %q)", got["a"], value, out)
			}
		})
	}
}

func TestSubstituteReferences_MultiLineValue(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n"

	out, err := SubstituteReferences([]byte("key: "+refToken+"\nother: 1\n"), map[string]string{refToken: pem})
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	var got map[string]any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got["key"] != pem {
		t.Errorf("key decodes as %q, want %q", got["key"], pem)
	}
}

func TestSubstituteReferences_KeepsCommentsAndAnchors(t *testing.T) {
	in := "# head\na: &x " + refToken + " # line\nb: *x\n"

	out, err := SubstituteReferences([]byte(in), map[string]string{refToken: "v"})
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	for _, want := range []string{"# head", "# line", "&x", "*x"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}

	var got map[string]any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}

	if got["a"] != "v" || got["b"] != "v" {
		t.Errorf("decoded %v, want a and b both \"v\"", got)
	}
}

func TestSubstituteReferences_MissingReference(t *testing.T) {
	_, err := SubstituteReferences([]byte("a: ref+op://v/i/x\n"), map[string]string{refToken: "unused"})

	got, ok := errors.AsType[*MissingReferenceError](err)
	if !ok {
		t.Fatalf("SubstituteReferences error = %v, want a *MissingReferenceError", err)
	}

	if got.Ref != "ref+op://v/i/x" {
		t.Errorf("Ref = %q, want %q", got.Ref, "ref+op://v/i/x")
	}

	if want := "topf: no value for reference ref+op://v/i/x"; got.Error() != want {
		t.Errorf("Error() = %q, want %q", got.Error(), want)
	}
}

func TestSubstituteReferences_MultiDocument(t *testing.T) {
	in := "a: ref+op://v/i/1\n---\nb: ref+op://v/i/2\n"
	values := map[string]string{"ref+op://v/i/1": "one", "ref+op://v/i/2": "two"}

	out, err := SubstituteReferences([]byte(in), values)
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	docs := decodeAll(t, out)
	if len(docs) != 2 {
		t.Fatalf("output holds %d documents, want 2: %q", len(docs), out)
	}

	first, _ := docs[0].(map[string]any)
	second, _ := docs[1].(map[string]any)

	if first["a"] != "one" || second["b"] != "two" {
		t.Errorf("documents decode as %v and %v, want a=one and b=two", first, second)
	}
}

func TestSubstituteReferences_DecodeErrorFirstHoldsNone(t *testing.T) {
	in := []byte("a: [\n---\nb: ref+op://v/i/f\n")

	out, err := SubstituteReferences(in, map[string]string{"ref+op://v/i/f": "x"})
	if err != nil {
		t.Fatalf("SubstituteReferences: %v", err)
	}

	if !bytes.Equal(out, in) {
		t.Errorf("SubstituteReferences = %q, want %q", out, in)
	}
}

func TestReadResolved(t *testing.T) {
	fsys := fstest.MapFS{
		"plain.yaml": file("a: 1\n"),
		"ref.yaml":   file("a: " + refToken + "\n"),
	}
	values := map[string]string{refToken: "v"}

	got, err := ReadResolved(fsys, "plain.yaml", values)
	if err != nil || string(got) != "a: 1\n" {
		t.Errorf("ReadResolved(plain.yaml) = %q, %v, want %q", got, err, "a: 1\n")
	}

	got, err = ReadResolved(fsys, "ref.yaml", values)
	if err != nil || string(got) != "a: v\n" {
		t.Errorf("ReadResolved(ref.yaml) = %q, %v, want %q", got, err, "a: v\n")
	}

	if _, err = ReadResolved(fsys, "ref.yaml", nil); !isMissingRef(err) {
		t.Errorf("ReadResolved(ref.yaml, nil) error = %v, want a *MissingReferenceError", err)
	}

	if _, err = ReadResolved(fsys, "absent.yaml", values); err == nil {
		t.Error("ReadResolved(absent.yaml) succeeded, want an error")
	}
}

func FuzzSubstituteReferences(f *testing.F) {
	for _, seed := range []string{
		"a: 1.0\n# c\n",
		"a: ref+op://v/i/f\n",
		"a: x ref+op://v/i/f\n",
		"ref+op://k: 1\nb: ref+op://v/i/f\n",
		"- ref+op://v/i/f\n- 2\n",
		"a: ref+op://v/i/1\n---\nb: ref+op://v/i/2\n",
		"a: [\n---\nb: ref+op://v/i/f\n",
		"a: ref+op://v/i/x\n",
		"# head\na: &x ref+op://v/i/t # line\nb: *x\n",
		"key: |\n  ref+op://v/i/f\n",
		"ref+op://v/i/f\n",
		"",
		"---\n---\n",
		"a: \"ref+op://v/i/t\"\n",
	} {
		f.Add([]byte(seed))
	}

	values := map[string]string{
		"ref+op://v/i/f": "multi\nline: true\n",
		"ref+op://v/i/t": "0123",
		"ref+op://v/i/1": "one",
		"ref+op://v/i/2": "two",
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := SubstituteReferences(in, values)

		// Without the literal prefix no scalar can be a reference.
		if !bytes.Contains(in, []byte("ref+")) {
			if err != nil {
				t.Fatalf("SubstituteReferences(%q) error = %v, want nil", in, err)
			}

			if !bytes.Equal(out, in) {
				t.Fatalf("SubstituteReferences(%q) = %q, want the input unchanged", in, out)
			}
		}
	})
}

func isMissingRef(err error) bool {
	_, ok := errors.AsType[*MissingReferenceError](err)
	return ok
}
