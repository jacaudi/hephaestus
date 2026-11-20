package render

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// Marker replaces every secret value in a display render. Matching skips its occurrences, so a value that is a part of
// the marker's own text (such as "RED") is never matched inside one.
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
// each trimmed line of 8 or more characters that is not a PEM armour line. Longest patterns are replaced first. A
// replacement pass, like the check, searches only the text between occurrences of Marker.
//
// The output is then decoded as multi-document YAML: a decode failure is ErrRedaction, and a decoded string scalar or
// mapping key that still holds a pattern outside a Marker is a *RedactionError.
func Redact(render []byte, values map[string]string) ([]byte, error) {
	patterns := patternsOf(values)
	text := string(render)

	for _, p := range patterns {
		text = replaceOutsideMarker(text, p.text)
	}

	out := []byte(text)
	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc any

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}

		// The text of a decode error can quote the render, so it is dropped.
		if err != nil {
			return nil, fmt.Errorf("%w: the output does not decode as YAML", ErrRedaction)
		}

		if ref, found := survivor(doc, patterns); found {
			return nil, &RedactionError{Ref: ref}
		}
	}
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

// replaceOutsideMarker replaces every occurrence of pat in text by Marker, except inside, or across the edge of, an
// occurrence of Marker.
func replaceOutsideMarker(text, pat string) string {
	pieces := strings.Split(text, Marker)

	for i, piece := range pieces {
		pieces[i] = strings.ReplaceAll(piece, pat, Marker)
	}

	return strings.Join(pieces, Marker)
}

// holdsOutsideMarker reports whether s holds pat anywhere but inside an occurrence of Marker.
func holdsOutsideMarker(s, pat string) bool {
	for piece := range strings.SplitSeq(s, Marker) {
		if strings.Contains(piece, pat) {
			return true
		}
	}

	return false
}

// survivor returns the reference of the first pattern that a string scalar or mapping key below v still holds outside
// a Marker, through both kinds of mapping YAML decodes to.
func survivor(v any, patterns []pattern) (ref string, found bool) {
	switch v := v.(type) {
	case string:
		for _, p := range patterns {
			if holdsOutsideMarker(v, p.text) {
				return p.ref, true
			}
		}
	case []any:
		for _, item := range v {
			if ref, found := survivor(item, patterns); found {
				return ref, true
			}
		}
	case map[string]any:
		for key, item := range v {
			if ref, found := survivor(key, patterns); found {
				return ref, true
			}

			if ref, found := survivor(item, patterns); found {
				return ref, true
			}
		}
	case map[any]any:
		for key, item := range v {
			if ref, found := survivor(key, patterns); found {
				return ref, true
			}

			if ref, found := survivor(item, patterns); found {
				return ref, true
			}
		}
	}

	return "", false
}
