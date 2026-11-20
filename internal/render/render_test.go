package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	schematicTpl  = "schematic.yaml.tpl"
	hashedArgBody = "customization:\n  extraKernelArgs:\n    - \"{{ .Data.x | sha256sum }}\"\n"
	refClusterID  = "ref+op://lab/bundle/cluster.id"
)

func bundleValues(t *testing.T) map[string]string {
	t.Helper()

	var values map[string]string
	if err := json.Unmarshal(bundleFile(t, "values.json"), &values); err != nil {
		t.Fatalf("decode values.json: %v", err)
	}

	return values
}

// renderTree is the base tree with a control-plane patch that Talos accepts: the Load fixtures hold a patch that only Load
// reads.
func renderTree(t *testing.T) fstest.MapFS {
	t.Helper()

	fsys := baseTree(t)
	apply(t, fsys, writeFile("control-plane/cp.yaml.tpl", "machine:\n  network:\n    hostname: {{ .Node.Host }}\n"))

	return fsys
}

func loadAndRender(t *testing.T, fsys fs.FS, values map[string]string) ([]HostRender, error) {
	t.Helper()

	cluster, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return cluster.Render(t.Context(), values)
}

func wantSHA256(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestRender_Hashes(t *testing.T) {
	values := bundleValues(t)

	cluster, err := Load(renderTree(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	first, err := cluster.Render(t.Context(), values)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	second, err := cluster.Render(t.Context(), values)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}

	if len(first) != 2 || first[0].Host != "cp-01" || first[1].Host != "wrk-01" {
		t.Fatalf("hosts = %+v, want cp-01 then wrk-01", first)
	}

	for i, host := range first {
		if got, want := host.ConfigHash, wantSHA256(host.Config); got != want {
			t.Errorf("%s ConfigHash = %s, want %s", host.Host, got, want)
		}

		if got, want := host.DisplayHash, wantSHA256(host.Redacted); got != want {
			t.Errorf("%s DisplayHash = %s, want %s", host.Host, got, want)
		}

		if host.ConfigHash != second[i].ConfigHash || host.DisplayHash != second[i].DisplayHash {
			t.Errorf("%s hashes differ between two renders of the same values", host.Host)
		}

		if host.TalosVersion != "v1.12.11" {
			t.Errorf("%s TalosVersion = %q, want v1.12.11", host.Host, host.TalosVersion)
		}
	}

	if first[0].Role != "control-plane" || first[0].IP != "10.10.20.11" || first[1].Role != "worker" || first[1].IP != "10.10.20.21" {
		t.Errorf("role and ip = %s %s, %s %s", first[0].Role, first[0].IP, first[1].Role, first[1].IP)
	}
}

func TestRender_RedactedHasNoValue(t *testing.T) {
	values := bundleValues(t)

	hosts, err := loadAndRender(t, renderTree(t), values)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, host := range hosts {
		if bytes.Equal(host.Config, host.Redacted) {
			t.Errorf("%s Redacted equals Config, want the values replaced", host.Host)
		}

		if !bytes.Contains(host.Redacted, []byte(Marker)) {
			t.Errorf("%s Redacted holds no Marker", host.Host)
		}

		for _, ref := range slices.Sorted(maps.Keys(values)) {
			if len(values[ref]) >= 8 && bytes.Contains(host.Redacted, []byte(values[ref])) {
				t.Errorf("%s Redacted holds the value of %s", host.Host, ref)
			}
		}
	}
}

func TestRender_UnparsableBundleRefused(t *testing.T) {
	values := bundleValues(t)
	values["ref+op://lab/bundle/certs.etcd.key"] = "x"

	_, err := loadAndRender(t, renderTree(t), values)

	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("Render error = %v, want a *RefusalError", err)
	}

	if refusal.Rule != "secrets bundle unparsable" || refusal.File != "secrets.yaml" {
		t.Errorf("refusal = %q in %q, want rule \"secrets bundle unparsable\" in secrets.yaml", refusal.Rule, refusal.File)
	}
}

// schematicTree is the base tree with the schematic template as the cluster's schematic, and data.<key> as given.
func schematicTree(t *testing.T, body, key, value string) fs.FS {
	t.Helper()

	fsys := renderTree(t)
	apply(t, fsys,
		appendTopf("schematicId: \"@"+schematicTpl+"\"\ndata:\n  "+key+": "+value+"\n"),
		writeFile(schematicTpl, body),
	)

	return fsys
}

func TestSchematicBody_TransformedSecretRefused(t *testing.T) {
	_, err := loadAndRender(t, schematicTree(t, hashedArgBody, "x", refClusterID), bundleValues(t))

	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("Render error = %v, want a *RefusalError", err)
	}

	if refusal.Rule != "secret reaches schematic" || refusal.File != schematicTpl {
		t.Errorf("refusal = %q in %q, want rule \"secret reaches schematic\" in %s", refusal.Rule, refusal.File, schematicTpl)
	}
}

func TestSchematicBody_PlainDataPasses(t *testing.T) {
	const arg = "net.ifnames=0"

	hosts, err := loadAndRender(t, schematicTree(t, hashedArgBody, "x", arg), bundleValues(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	want := wantSHA256([]byte(arg))

	for _, host := range hosts {
		if !strings.Contains(string(host.SchematicBody), want) {
			t.Errorf("%s SchematicBody = %q, want it to hold %s", host.Host, host.SchematicBody, want)
		}

		if len(host.SchematicID) != 64 {
			t.Errorf("%s SchematicID = %q, want 64 hex digits", host.Host, host.SchematicID)
		}
	}
}

func TestSchematicBody_DummyRenderErrorRefused(t *testing.T) {
	const body = "{{ if ne .Data.mode \"secret-mode\" }}{{ fail \"x\" }}{{ end }}customization:\n  extraKernelArgs:\n    - a=b\n"

	const ref = "ref+op://lab/bundle/mode"

	values := bundleValues(t)
	values[ref] = "secret-mode"

	_, err := loadAndRender(t, schematicTree(t, body, "mode", ref), values)

	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("Render error = %v, want a *RefusalError", err)
	}

	if refusal.Rule != "secret reaches schematic" || refusal.File != schematicTpl {
		t.Errorf("refusal = %q in %q, want rule \"secret reaches schematic\" in %s", refusal.Rule, refusal.File, schematicTpl)
	}
}

func TestSchematicBody_RefusalNamesCleanedPath(t *testing.T) {
	fsys := renderTree(t)
	apply(t, fsys,
		appendTopf("schematicId: \"@./"+schematicTpl+"\"\ndata:\n  x: "+refClusterID+"\n"),
		writeFile(schematicTpl, hashedArgBody),
	)

	_, err := loadAndRender(t, fsys, bundleValues(t))

	if refusal, ok := errors.AsType[*RefusalError](err); !ok || refusal.File != schematicTpl {
		t.Fatalf("Render error = %v, want a *RefusalError naming %s", err, schematicTpl)
	}
}
