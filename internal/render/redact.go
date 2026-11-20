package render

import (
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// Marker replaces every secret value in a display render. A value's text that lies wholly inside a Marker, such as
// "RED", is the marker's own text and not the value in clear.
const Marker = "HEPHAESTUS-REDACTED-0c9f"

// minLinePattern is the shortest trimmed line of a multi-line value that is redacted on its own.
const minLinePattern = 8

// ErrRedaction reports a display render that cannot be shown: a secret value survived the redaction, or the output
// does not decode as YAML so the check cannot run.
var ErrRedaction = errors.New("render: a secret value survived redaction")

// RedactionError reports a secret value found in a decoded scalar of a redacted render. It names the reference of the
// value and never the value.
type RedactionError struct {
	// Ref is the reference of the value that survived.
	Ref string
}

// Error returns "render: the value of <ref> survived redaction".
func (e *RedactionError) Error() string {
	return "render: the value of " + e.Ref + " survived redaction"
}

// Unwrap returns ErrRedaction.
func (e *RedactionError) Unwrap() error { return ErrRedaction }

// pattern is one text to redact and the reference of the value it stands for.
type pattern struct {
	text, ref string
}

// Redact replaces every secret value in render by Marker and then checks the result. values maps each reference to its
// value; an empty value is ignored.
//
// The patterns of a value are the value, its four base64 forms, its lower-case hex and, for a value of several lines,
// each trimmed line of 8 or more characters that is not a PEM armour line. Every occurrence of every pattern is found in
// render, overlapping ones included, and each run of overlapping or touching occurrences is replaced by one Marker.
//
// The output is then decoded as multi-document YAML: a decode failure is ErrRedaction. A pattern occurrence that is not
// wholly inside a Marker, in the output text or in a decoded string scalar or mapping key, is a *RedactionError naming
// the reference of the first such pattern, longest first.
func Redact(render []byte, values map[string]string) ([]byte, error) {
	patterns := patternsOf(values)

	// A value that holds Marker cannot be told from a Marker once redacted, so it is refused whole.
	for _, p := range patterns {
		if strings.Contains(p.text, Marker) {
			return nil, &RedactionError{Ref: p.ref}
		}
	}

	text := redactSpans(string(render), patterns)
	texts := []string{text}
	dec := yaml.NewDecoder(strings.NewReader(text))

	for {
		var doc any

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		// The text of a decode error can quote the render, so it is dropped.
		if err != nil {
			return nil, fmt.Errorf("%w: the output does not decode as YAML", ErrRedaction)
		}

		texts = appendDecoded(texts, doc)
	}

	if ref, found := survivor(texts, patterns); found {
		return nil, &RedactionError{Ref: ref}
	}

	return []byte(text), nil
}

// patternsOf returns the patterns of every non-empty value: longest first, equal lengths in byte order, each text once
// and credited to the first reference in sorted order that yields it.
func patternsOf(values map[string]string) []pattern {
	var patterns []pattern

	for _, ref := range slices.Sorted(maps.Keys(values)) {
		for _, text := range valuePatterns(values[ref]) {
			patterns = append(patterns, pattern{text: text, ref: ref})
		}
	}

	slices.SortStableFunc(patterns, func(a, b pattern) int {
		return cmp.Or(cmp.Compare(len(b.text), len(a.text)), strings.Compare(a.text, b.text))
	})

	return slices.CompactFunc(patterns, func(a, b pattern) bool { return a.text == b.text })
}

// valuePatterns returns the texts under which value can appear in a render. It returns nothing for an empty value.
func valuePatterns(value string) []string {
	if value == "" {
		return nil
	}

	raw := []byte(value)
	texts := []string{
		value,
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
		hex.EncodeToString(raw),
	}

	if !strings.Contains(value, "\n") {
		return texts
	}

	// A multi-line value lands in a render with its lines indented, folded or re-wrapped, so each line stands alone.
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimSpace(line)

		if utf8.RuneCountInString(line) >= minLinePattern && !isArmour(line) {
			texts = append(texts, line)
		}
	}

	return texts
}

func isArmour(line string) bool {
	return strings.HasPrefix(line, "-----BEGIN ") || strings.HasPrefix(line, "-----END ")
}

// redactSpans replaces every occurrence of every pattern in text by Marker, one Marker for each run of overlapping or
// touching occurrences.
func redactSpans(text string, patterns []pattern) string {
	covered := make([]bool, len(text))

	for _, p := range patterns {
		for i := range occurrences(text, p.text) {
			for j := range len(p.text) {
				covered[i+j] = true
			}
		}
	}

	var out strings.Builder

	for i := 0; i < len(text); {
		end := i
		for end < len(text) && covered[end] == covered[i] {
			end++
		}

		if covered[i] {
			out.WriteString(Marker)
		} else {
			out.WriteString(text[i:end])
		}

		i = end
	}

	return out.String()
}

// occurrences yields the start of every occurrence of pat in s, overlapping ones included. pat is not empty.
func occurrences(s, pat string) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := 0; ; i++ {
			at := strings.Index(s[i:], pat)
			if at < 0 {
				return
			}

			i += at
			if !yield(i) {
				return
			}
		}
	}
}

// holdsOutsideMarker reports whether s holds an occurrence of pat that is not wholly inside an occurrence of Marker.
func holdsOutsideMarker(s, pat string) bool {
	markers := slices.Collect(occurrences(s, Marker))

	for i := range occurrences(s, pat) {
		inside := func(m int) bool { return m <= i && i+len(pat) <= m+len(Marker) }
		if !slices.ContainsFunc(markers, inside) {
			return true
		}
	}

	return false
}

// survivor returns the reference of the first pattern, in the order of patterns, that one of texts holds outside a
// Marker. Going pattern by pattern, not string by string, makes the answer the same on every run.
func survivor(texts []string, patterns []pattern) (ref string, found bool) {
	for _, p := range patterns {
		for _, s := range texts {
			if holdsOutsideMarker(s, p.text) {
				return p.ref, true
			}
		}
	}

	return "", false
}

// appendDecoded appends every string scalar and mapping key below v, through both kinds of mapping YAML decodes to.
func appendDecoded(out []string, v any) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, item := range v {
			out = appendDecoded(out, item)
		}
	case map[string]any:
		out = appendMap(out, v)
	case map[any]any:
		out = appendMap(out, v)
	}

	return out
}

func appendMap[K comparable](out []string, m map[K]any) []string {
	for key, item := range m {
		out = appendDecoded(appendDecoded(out, key), item)
	}

	return out
}
