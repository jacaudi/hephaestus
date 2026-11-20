// hephaestus: new

//go:build ignore

// Command genbundle writes the secrets-bundle fixture of the render tests.
//
// It generates a Talos secrets bundle (machinery's secrets.NewBundle at a fixed clock) and writes it two ways into
// the directory named by -out: secrets.yaml, the bundle's YAML with every leaf string replaced by the reference
// ref+op://lab/bundle/<dotted.path>, and values.json, the value of each reference. The outputs are committed;
// the keys are random, so a run replaces both files.
//
//	go run internal/third_party/topf/render/testdata/genbundle/main.go -out internal/third_party/topf/render/testdata/bundle
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"go.yaml.in/yaml/v4"
)

const refPrefix = "ref+op://lab/bundle/"

func main() {
	out := flag.String("out", "", "directory to write secrets.yaml and values.json into")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "genbundle: -out is required")
		os.Exit(2)
	}

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "genbundle:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	bundle, err := secrets.NewBundle(secrets.NewFixedClock(t0), nil)
	if err != nil {
		return fmt.Errorf("generate bundle: %w", err)
	}

	raw, err := yaml.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode bundle: %w", err)
	}

	values := map[string]string{}
	replaceLeaves(&doc, nil, values)

	refs, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode references: %w", err)
	}

	valuesJSON, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("encode values: %w", err)
	}

	if err := os.WriteFile(filepath.Join(out, "secrets.yaml"), refs, 0o644); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(out, "values.json"), append(valuesJSON, '\n'), 0o644)
}

// replaceLeaves turns every scalar below n into a reference named by its dotted key path, recording its value.
func replaceLeaves(n *yaml.Node, path []string, values map[string]string) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			replaceLeaves(c, path, values)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			replaceLeaves(n.Content[i+1], append(path[:len(path):len(path)], n.Content[i].Value), values)
		}
	case yaml.ScalarNode:
		ref := refPrefix + strings.Join(path, ".")
		values[ref] = n.Value
		n.Value, n.Tag, n.Style = ref, "!!str", 0
	}
}
