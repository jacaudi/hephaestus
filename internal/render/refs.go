package render

import (
	"bytes"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v4"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// opRefPrefix starts every reference hephaestus resolves: a 1Password reference, in vals' notation.
const opRefPrefix = "ref+op://"

// IsReference reports whether s is a whole-scalar reference hephaestus resolves: "ref+op://VAULT/ITEM/[SECTION/]FIELD",
// three or four non-empty segments, with no "?" query, no "#" fragment, no white space and no "+" after the prefix.
func IsReference(s string) bool {
	rest, ok := strings.CutPrefix(s, opRefPrefix)
	if !ok || strings.ContainsAny(rest, "?#+") || strings.IndexFunc(rest, unicode.IsSpace) >= 0 {
		return false
	}

	segments := strings.Split(rest, "/")

	return (len(segments) == 3 || len(segments) == 4) && !slices.Contains(segments, "")
}

// HasEmbeddedReference reports whether s holds, anywhere, text that vals reads as a reference: the unanchored pattern
// `ref\+[A-Za-z0-9]+://`.
func HasEmbeddedReference(s string) bool {
	for {
		_, after, ok := strings.Cut(s, "ref+")
		if !ok {
			return false
		}

		scheme := 0
		for scheme < len(after) && isAlphanumeric(after[scheme]) {
			scheme++
		}

		if scheme > 0 && strings.HasPrefix(after[scheme:], "://") {
			return true
		}

		s = after
	}
}

func isAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// classifyReference decides what a string value of a resolved file is: it returns the load rule the value breaks, or
// whether it is a reference to resolve. A string that starts like a reference and is not a plain op one is
// ruleRefNotOp; one that holds a reference somewhere inside is ruleRefEmbedded.
func classifyReference(s string) (rule string, isRef bool) {
	switch {
	case config.HasReferencePrefix(s):
		if IsReference(s) {
			return "", true
		}

		return ruleRefNotOp, false
	case HasEmbeddedReference(s):
		return ruleRefEmbedded, false
	default:
		return "", false
	}
}

// stringValues returns the distinct string values and list items of every YAML document of raw, sorted, so the
// first refusal of a file is the same on every run. A mapping key is not a value: Topf substitutes values only. The
// documents are read in order and a document that does not decode ends the scan: the render fails the tree then.
func stringValues(raw []byte) []string {
	var values []string

	dec := yaml.NewDecoder(bytes.NewReader(raw))

	for {
		var doc any

		if dec.Decode(&doc) != nil {
			break
		}

		values = appendStrings(values, doc)
	}

	slices.Sort(values)

	return slices.Compact(values)
}

// appendStrings appends the string values and list items below v, through both kinds of mapping YAML decodes to.
func appendStrings(out []string, v any) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, item := range v {
			out = appendStrings(out, item)
		}
	case map[string]any:
		for _, item := range v {
			out = appendStrings(out, item)
		}
	case map[any]any:
		for _, item := range v {
			out = appendStrings(out, item)
		}
	}

	return out
}
