// hephaestus: new

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"text/template"
	"time"

	"go.yaml.in/yaml/v4"

	"go.jacaudi.dev/hephaestus/internal/third_party/imagefactory/schematic"
	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

const defaultSchematicID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

// baseTopf is the cluster definition of every tree; extra top-level keys follow it.
const baseTopf = `clusterName: lab
clusterEndpoint: https://10.10.20.11:6443
talosVersion: v1.12.11
kubernetesVersion: v1.35.10
nodes:
  - host: cp-01
    role: control-plane
    ip: 10.10.20.11
  - host: wrk-01
    role: worker
    ip: 10.10.20.21
`

// fixture reads the shared secrets bundle: secrets.yaml holds references only, values.json maps each to its value.
func fixture(t testing.TB) (secretsYAML []byte, values map[string]string) {
	t.Helper()

	secretsYAML, err := os.ReadFile("testdata/bundle/secrets.yaml")
	if err != nil {
		t.Fatalf("read secrets.yaml: %v", err)
	}

	raw, err := os.ReadFile("testdata/bundle/values.json")
	if err != nil {
		t.Fatalf("read values.json: %v", err)
	}

	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode values.json: %v", err)
	}

	return secretsYAML, values
}

// tree returns a definition tree of baseTopf followed by extra, the bundle, and the files in more.
func tree(t *testing.T, extra string, more map[string]string) (fstest.MapFS, map[string]string) {
	t.Helper()

	secretsYAML, values := fixture(t)

	fsys := fstest.MapFS{
		"topf.yaml":    {Data: []byte(baseTopf + extra)},
		"secrets.yaml": {Data: secretsYAML},
	}

	for name, body := range more {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}

	return fsys, values
}

func input(fsys fstest.MapFS, values map[string]string) Input {
	return Input{FS: fsys, Values: values, Funcs: template.FuncMap{}}
}

func renderOK(t *testing.T, extra string, more map[string]string) []Node {
	t.Helper()

	fsys, values := tree(t, extra, more)

	nodes, err := Render(t.Context(), input(fsys, values))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	return nodes
}

func TestRender_TwoNodes(t *testing.T) {
	nodes := renderOK(t, "", nil)

	want := []struct{ host, role, ip string }{
		{"cp-01", "control-plane", "10.10.20.11"},
		{"wrk-01", "worker", "10.10.20.21"},
	}

	if len(nodes) != len(want) {
		t.Fatalf("len(nodes) = %d, want %d", len(nodes), len(want))
	}

	for i, w := range want {
		got := nodes[i]
		if got.Host != w.host || got.Role != w.role || got.IP != w.ip {
			t.Errorf("node %d = %s %s %s, want %s %s %s", i, got.Host, got.Role, got.IP, w.host, w.role, w.ip)
		}

		if got.TalosVersion != "v1.12.11" || got.KubernetesVersion != "v1.35.10" {
			t.Errorf("%s versions = %s, %s, want v1.12.11, v1.35.10", got.Host, got.TalosVersion, got.KubernetesVersion)
		}

		var doc map[string]any
		if err := yaml.Unmarshal(got.Config, &doc); err != nil {
			t.Errorf("%s Config is not YAML: %v", got.Host, err)
			continue
		}

		if _, ok := doc["machine"]; !ok || !bytes.Contains(got.Config, []byte("machine:")) {
			t.Errorf("%s Config holds no machine: section", got.Host)
		}
	}
}

func TestRender_NodeWithoutIP(t *testing.T) {
	fsys, values := tree(t, "", nil)
	fsys["topf.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(baseTopf, "    ip: 10.10.20.21\n", "", 1))}

	nodes, err := Render(t.Context(), input(fsys, values))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if nodes[1].Host != "wrk-01" || nodes[1].IP != "" {
		t.Errorf("worker = %q with IP %q, want wrk-01 with no IP", nodes[1].Host, nodes[1].IP)
	}
}

func TestRender_Deterministic(t *testing.T) {
	fsys, values := tree(t, "", nil)
	in := input(fsys, values)

	first, err := Render(t.Context(), in)
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}

	// Each Render skews its secrets clock from the time it runs; wait past a second so the two clocks differ at
	// the resolution of certificate validity.
	time.Sleep(1100 * time.Millisecond)

	second, err := Render(t.Context(), in)
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}

	for i := range first {
		if !bytes.Equal(first[i].Config, second[i].Config) {
			t.Errorf("%s: two renders of one Input differ", first[i].Host)
		}
	}
}

func TestRender_DefaultSchematic(t *testing.T) {
	nodes := renderOK(t, "", nil)

	if nodes[0].SchematicID != DefaultSchematic || DefaultSchematic != defaultSchematicID {
		t.Errorf("SchematicID = %q, DefaultSchematic = %q, want both %q", nodes[0].SchematicID, DefaultSchematic, defaultSchematicID)
	}

	if nodes[0].SchematicBody != nil {
		t.Errorf("SchematicBody = %q, want nil", nodes[0].SchematicBody)
	}

	image := "factory.talos.dev/metal-installer/" + defaultSchematicID + ":v1.12.11"
	if !bytes.Contains(nodes[0].Config, []byte(image)) {
		t.Errorf("Config does not hold the installer image %q", image)
	}
}

func TestRender_LiteralSchematicHasNoBody(t *testing.T) {
	const id = "0000000000000000000000000000000000000000000000000000000000000000"

	nodes := renderOK(t, "schematicId: "+id+"\n", nil)

	if nodes[0].SchematicID != id || nodes[0].SchematicBody != nil {
		t.Errorf("SchematicID = %q, SchematicBody = %q, want %q and nil", nodes[0].SchematicID, nodes[0].SchematicBody, id)
	}

	if !bytes.Contains(nodes[0].Config, []byte("/metal-installer/"+id+":v1.12.11")) {
		t.Errorf("Config does not hold the installer image of schematic %q", id)
	}
}

func TestRender_FileSchematicBodyAndID(t *testing.T) {
	const body = "customization:\n  extraKernelArgs:\n    - console=ttyS0\n"

	nodes := renderOK(t, "schematicId: \"@schematic.yaml\"\n", map[string]string{"schematic.yaml": body})

	wantBody, err := schematic.Canonical([]byte(body))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	wantID, err := schematic.ID([]byte(body))
	if err != nil {
		t.Fatalf("ID: %v", err)
	}

	for _, n := range nodes {
		if !bytes.Equal(n.SchematicBody, wantBody) {
			t.Errorf("%s SchematicBody = %q, want %q", n.Host, n.SchematicBody, wantBody)
		}

		if n.SchematicID != wantID {
			t.Errorf("%s SchematicID = %q, want %q", n.Host, n.SchematicID, wantID)
		}

		if !bytes.Contains(n.Config, []byte("/metal-installer/"+wantID+":v1.12.11")) {
			t.Errorf("%s Config does not hold the installer image of schematic %q", n.Host, wantID)
		}
	}
}

func TestRender_TemplateSchematicGetsData(t *testing.T) {
	const body = "customization:\n  extraKernelArgs:\n    - \"{{ .Data.arg }}\"\n"

	extra := "schematicId: \"@schematic.yaml.tpl\"\ndata:\n  arg: net.ifnames=0\n"

	nodes := renderOK(t, extra, map[string]string{"schematic.yaml.tpl": body})

	if !bytes.Contains(nodes[0].SchematicBody, []byte("net.ifnames=0")) {
		t.Errorf("SchematicBody = %q, want it to hold net.ifnames=0", nodes[0].SchematicBody)
	}
}

func TestRender_SchematicPathEscapes(t *testing.T) {
	fsys, values := tree(t, "schematicId: \"@../x.yaml\"\n", nil)

	_, err := Render(t.Context(), input(fsys, values))
	if !errors.Is(err, config.ErrEscapes) {
		t.Errorf("Render error = %v, want config.ErrEscapes", err)
	}
}

func TestRender_MissingBundle(t *testing.T) {
	fsys, values := tree(t, "", nil)
	delete(fsys, "secrets.yaml")

	_, err := Render(t.Context(), input(fsys, values))
	if !errors.Is(err, ErrSecretsBundleMissing) {
		t.Errorf("Render error = %v, want ErrSecretsBundleMissing", err)
	}
}

func TestRender_EmptyBundle(t *testing.T) {
	fsys, values := tree(t, "", map[string]string{"secrets.yaml": ""})

	_, err := Render(t.Context(), input(fsys, values))
	if !errors.Is(err, ErrSecretsBundleMissing) {
		t.Errorf("Render error = %v, want ErrSecretsBundleMissing", err)
	}
}

// unparsableBundles are secrets files that are not empty and are not a valid bundle.
func unparsableBundles() []struct{ name, body string } {
	return []struct{ name, body string }{
		{"not yaml", "certs: ["},
		{"fails validation", "cluster:\n  id: x\n"},
		{"null", "null"},
		{"tilde", "~"},
		{"document marker", "---\n"},
		{"comment only", "# nothing\n"},
		{"empty list", "[]"},
		{"reference in malformed yaml", " 00000000A: ref+op://lab/bundle/certs.k8s.crt\n0"},
	}
}

func TestRender_UnparsableBundle(t *testing.T) {
	for _, tt := range unparsableBundles() {
		t.Run(tt.name, func(t *testing.T) {
			fsys, values := tree(t, "", map[string]string{"secrets.yaml": tt.body})

			_, err := Render(t.Context(), input(fsys, values))
			if !errors.Is(err, ErrSecretsBundleUnparsable) {
				t.Errorf("Render error = %v, want ErrSecretsBundleUnparsable", err)
			}
		})
	}
}

func TestRender_SecretsProviderRefused(t *testing.T) {
	fsys, values := tree(t, "secretsProvider: ./provider\n", nil)

	_, err := Render(t.Context(), input(fsys, values))
	if err == nil || !strings.Contains(err.Error(), "secretsProvider") {
		t.Errorf("Render error = %v, want one naming secretsProvider", err)
	}
}

func TestRender_PatchApplied(t *testing.T) {
	const patch = `machine: {nodeLabels: {"hephaestus.test/host": "{{ .Node.Host }}"}}` + "\n"

	nodes := renderOK(t, "", map[string]string{"all/labels.yaml.tpl": patch})

	for _, n := range nodes {
		if !bytes.Contains(n.Config, []byte("hephaestus.test/host: "+n.Host)) {
			t.Errorf("%s Config does not hold its host label", n.Host)
		}
	}
}

func TestRender_UnknownReference(t *testing.T) {
	const patch = "machine:\n  nodeLabels:\n    secret: ref+op://lab/missing/field\n"

	fsys, values := tree(t, "", map[string]string{"all/ref.yaml": patch})

	_, err := Render(t.Context(), input(fsys, values))

	missing, ok := errors.AsType[*config.MissingReferenceError](err)
	if !ok {
		t.Fatalf("Render error = %v, want *config.MissingReferenceError", err)
	}

	if missing.Ref != "ref+op://lab/missing/field" {
		t.Errorf("Ref = %q, want ref+op://lab/missing/field", missing.Ref)
	}
}

func TestRender_NodeErrorNamesHost(t *testing.T) {
	fsys, values := tree(t, "", map[string]string{"node/wrk-01/bad.yaml": "machine: ["})

	_, err := Render(t.Context(), input(fsys, values))
	if err == nil || !strings.Contains(err.Error(), "wrk-01") {
		t.Errorf("Render error = %v, want one naming wrk-01", err)
	}
}

func TestRender_CanceledContext(t *testing.T) {
	fsys, values := tree(t, "", nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Render(ctx, input(fsys, values))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Render error = %v, want context.Canceled", err)
	}
}

func TestSchematic_ReadsNoPatch(t *testing.T) {
	fsys, values := tree(t, "", map[string]string{"all/bad.yaml": "machine: ["})

	// Schematic reads neither the patch nor the bundle: it resolves with the bundle gone.
	withoutBundle, _ := tree(t, "", map[string]string{"all/bad.yaml": "machine: ["})
	delete(withoutBundle, "secrets.yaml")

	id, body, err := Schematic(t.Context(), input(withoutBundle, values), "cp-01")
	if err != nil {
		t.Fatalf("Schematic: %v", err)
	}

	if id != DefaultSchematic || body != nil {
		t.Errorf("Schematic = %q, %q, want %q and nil", id, body, DefaultSchematic)
	}

	// Render, with the bundle present, reads the patch and fails on it.
	_, err = Render(t.Context(), input(fsys, values))
	if err == nil || errors.Is(err, ErrSecretsBundleMissing) {
		t.Errorf("Render error = %v, want the patch error", err)
	}
}

func TestSchematic_FileBody(t *testing.T) {
	const body = "customization:\n  extraKernelArgs:\n    - console=ttyS0\n"

	fsys, values := tree(t, "schematicId: \"@schematic.yaml\"\n", map[string]string{"schematic.yaml": body})

	id, got, err := Schematic(t.Context(), input(fsys, values), "wrk-01")
	if err != nil {
		t.Fatalf("Schematic: %v", err)
	}

	wantBody, err := schematic.Canonical([]byte(body))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	wantID, err := schematic.ID([]byte(body))
	if err != nil {
		t.Fatalf("ID: %v", err)
	}

	if id != wantID || !bytes.Equal(got, wantBody) {
		t.Errorf("Schematic = %q, %q, want %q, %q", id, got, wantID, wantBody)
	}
}

func TestSchematic_UnknownHost(t *testing.T) {
	fsys, values := tree(t, "", nil)

	if _, _, err := Schematic(t.Context(), input(fsys, values), "nope"); err == nil {
		t.Error("Schematic of an unknown host succeeded, want an error")
	}
}

func FuzzLoadSecretsBundle(f *testing.F) {
	secretsYAML, values := fixture(f)

	f.Add(secretsYAML)
	f.Add([]byte(""))

	for _, tt := range unparsableBundles() {
		f.Add([]byte(tt.body))
	}

	cfg := &config.TopfConfig{SecretsPath: "secrets.yaml"}

	f.Fuzz(func(t *testing.T, content []byte) {
		fsys := fstest.MapFS{"secrets.yaml": {Data: content}}

		_, err := loadSecretsBundle(input(fsys, values), cfg)
		if err == nil || errors.Is(err, ErrSecretsBundleMissing) || errors.Is(err, ErrSecretsBundleUnparsable) {
			return
		}

		if _, ok := errors.AsType[*config.MissingReferenceError](err); !ok {
			t.Errorf("loadSecretsBundle error = %v, want nil, Missing, Unparsable or a missing reference", err)
		}
	})
}
