// hephaestus: new

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MissingReferenceError reports a secret reference in a file that has no value in the map given to SubstituteReferences.
// It names the reference, never a value.
type MissingReferenceError struct {
	// Ref is the reference as written in the file, for example "ref+op://vault/item/field".
	Ref string
}

func (e *MissingReferenceError) Error() string {
	return "topf: no value for reference " + e.Ref
}

// HasReferencePrefix reports whether s starts like a vals reference: Topf's pattern `^ref\+[a-zA-Z0-9]+://`
// (internal/vals/vals.go). The scheme is ASCII letters and digits only.
func HasReferencePrefix(s string) bool {
	rest, ok := strings.CutPrefix(s, "ref+")
	if !ok {
		return false
	}

	scheme, _, ok := strings.Cut(rest, "://")
	if !ok || scheme == "" {
		return false
	}

	for i := range len(scheme) {
		c := scheme[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}

	return true
}

// ReadResolved reads name from fsys and replaces its secret references with values (see SubstituteReferences).
func ReadResolved(fsys fs.FS, name string, values map[string]string) ([]byte, error) {
	content, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}

	resolved, err := SubstituteReferences(content, values)
	if err != nil {
		return nil, fmt.Errorf("resolve references in %s: %w", name, err)
	}

	return resolved, nil
}

// SubstituteReferences replaces each secret reference in the YAML content with its value from values.
// It stands in for Topf's `vals eval` pipe (internal/decryption and internal/vals), which hephaestus does not run.
//
// A reference is a whole scalar, never a mapping key, that starts like one (HasReferencePrefix); text around a
// reference is not one. Content without a reference is returned unchanged, byte for byte. Otherwise every
// document is decoded, each reference scalar becomes the value as a string (a value such as "true" or "0123"
// stays a string, a multi-line value stays one value), and the documents are encoded again with two-space
// indent: formatting that carries no meaning (spacing, quoting style) may change, comments and anchors stay.
// A reference with no entry in values is a *MissingReferenceError.
func SubstituteReferences(content []byte, values map[string]string) ([]byte, error) {
	if !hasReferences(content) {
		return content, nil
	}

	var (
		buf bytes.Buffer
		enc = yaml.NewEncoder(&buf)
		dec = yaml.NewDecoder(bytes.NewReader(content))
	)

	enc.SetIndent(2)

	for {
		var doc yaml.Node

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("decode yaml: %w", err)
		}

		if err := substituteNode(&doc, values); err != nil {
			return nil, err
		}

		if err := enc.Encode(&doc); err != nil {
			return nil, fmt.Errorf("encode yaml: %w", err)
		}
	}

	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}

	return buf.Bytes(), nil
}

// substituteNode replaces, in place, the value of every reference scalar below n. Mapping keys are left alone.
func substituteNode(n *yaml.Node, values map[string]string) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if !HasReferencePrefix(n.Value) {
			return nil
		}

		value, ok := values[n.Value]
		if !ok {
			return &MissingReferenceError{Ref: n.Value}
		}

		n.Value = value
		n.Tag = "!!str"
		n.Style = 0
	case yaml.MappingNode:
		// Content holds key, value, key, value, ...
		for i := 1; i < len(n.Content); i += 2 {
			if err := substituteNode(n.Content[i], values); err != nil {
				return err
			}
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range n.Content {
			if err := substituteNode(child, values); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		// The anchored node is replaced where it is defined.
	}

	return nil
}

// hasReferences reports whether content holds a reference, as Topf's hasValsRefs does (internal/vals/vals.go):
// the documents are decoded in order, and a decode error ends the scan with false, whatever later documents hold.
func hasReferences(content []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(content))

	for {
		var doc any

		if err := dec.Decode(&doc); err != nil {
			return false
		}

		if doc != nil && containsReference(doc) {
			return true
		}
	}
}

// containsReference reports whether a decoded YAML value holds a reference in a mapping value or list item.
func containsReference(data any) bool {
	switch v := data.(type) {
	case map[string]any:
		for _, val := range v {
			if containsReference(val) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, containsReference)
	case string:
		return HasReferencePrefix(v)
	}

	return false
}
