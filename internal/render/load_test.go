package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// baseTopf is the cluster definition of the valid tree. It lists the worker before the control plane so the sort is
// observable, and ends with the nodes so a test can append top-level keys.
const baseTopf = `clusterName: lab
clusterEndpoint: https://10.10.20.11:6443
kubernetesVersion: v1.35.10
talosVersion: v1.12.11
nodes:
  - host: wrk-01
    role: worker
    ip: 10.10.20.21
  - host: cp-01
    role: control-plane
    ip: 10.10.20.11
`

// baseAll holds a reference the secrets bundle holds too, so Refs must de-duplicate.
const baseAll = `machine:
  token: ref+op://lab/bundle/cluster.id
`

const baseTpl = `machine:
  network:
    hostname: {{ .Node.Host }}
    endpoint: {{ .Node.Endpoint }}
`

const (
	bundleDir  = "../third_party/topf/render/testdata/bundle/"
	topfName   = "topf.yaml"
	ruleEscape = "path escapes tree"
)

func file(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }

func link(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

func bundleFile(tb testing.TB, name string) []byte {
	tb.Helper()

	data, err := os.ReadFile(bundleDir + name)
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}

	return data
}

// newTree returns the valid definition tree around the given secrets bundle.
func newTree(secrets []byte) fstest.MapFS {
	return fstest.MapFS{
		topfName:                    file(baseTopf),
		"secrets.yaml":              {Data: secrets},
		"all/base.yaml":             file(baseAll),
		"control-plane/cp.yaml.tpl": file(baseTpl),
	}
}

func baseTree(tb testing.TB) fstest.MapFS {
	tb.Helper()

	return newTree(bundleFile(tb, "secrets.yaml"))
}

// edit changes a tree; it returns an error when the tree does not have the shape it expects.
type edit func(fsys fstest.MapFS) error

func apply(tb testing.TB, fsys fstest.MapFS, edits ...edit) {
	tb.Helper()

	for _, e := range edits {
		if err := e(fsys); err != nil {
			tb.Fatalf("edit tree: %v", err)
		}
	}
}

// replaceTopf replaces the first occurrence of old in topf.yaml.
func replaceTopf(oldText, newText string) edit {
	return func(fsys fstest.MapFS) error {
		f := fsys[topfName]
		if f == nil || !strings.Contains(string(f.Data), oldText) {
			return fmt.Errorf("topf.yaml does not hold %q", oldText)
		}

		f.Data = []byte(strings.Replace(string(f.Data), oldText, newText, 1))

		return nil
	}
}

func appendTopf(extra string) edit {
	return func(fsys fstest.MapFS) error {
		fsys[topfName].Data = append(fsys[topfName].Data, extra...)

		return nil
	}
}

func writeFile(name, content string) edit {
	return func(fsys fstest.MapFS) error {
		fsys[name] = file(content)

		return nil
	}
}

func linkTo(name, target string) edit {
	return func(fsys fstest.MapFS) error {
		fsys[name] = link(target)

		return nil
	}
}

func remove(name string) edit {
	return func(fsys fstest.MapFS) error {
		delete(fsys, name)

		return nil
	}
}

// moveFile writes the content of from at to, and removes from.
func moveFile(from, to string) edit {
	return func(fsys fstest.MapFS) error {
		fsys[to] = fsys[from]
		delete(fsys, from)

		return nil
	}
}

func loadRefusal(t *testing.T, fsys fs.FS) *RefusalError {
	t.Helper()

	_, err := Load(fsys)

	refusal, ok := errors.AsType[*RefusalError](err)
	if !ok {
		t.Fatalf("Load error = %v, want a *RefusalError", err)
	}

	return refusal
}

func TestLoad_Valid(t *testing.T) {
	fsys := baseTree(t)

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Name != "lab" || got.TalosVersion != "v1.12.11" || got.KubernetesVersion != "v1.35.10" {
		t.Errorf("Name, TalosVersion, KubernetesVersion = %q, %q, %q, want lab, v1.12.11, v1.35.10",
			got.Name, got.TalosVersion, got.KubernetesVersion)
	}

	wantHosts := []Host{
		{Name: "cp-01", Role: "control-plane", IP: "10.10.20.11"},
		{Name: "wrk-01", Role: "worker", IP: "10.10.20.21"},
	}
	if !slices.Equal(got.Hosts, wantHosts) {
		t.Errorf("Hosts = %+v, want %+v", got.Hosts, wantHosts)
	}

	var values map[string]string
	if err := json.Unmarshal(bundleFile(t, "values.json"), &values); err != nil {
		t.Fatalf("decode values.json: %v", err)
	}

	if want := slices.Sorted(maps.Keys(values)); !slices.Equal(got.Refs, want) {
		t.Errorf("Refs = %v, want the %d keys of values.json: %v", got.Refs, len(want), want)
	}

	if got.FS == nil {
		t.Error("FS is nil, want the tree")
	}

	if got.secretsPath != "secrets.yaml" {
		t.Errorf("secretsPath = %q, want secrets.yaml", got.secretsPath)
	}

	if want := map[string]string{"cp-01": "", "wrk-01": ""}; !maps.Equal(got.schematics, want) {
		t.Errorf("schematics = %v, want %v", got.schematics, want)
	}
}

func TestLoad_HostWithoutIP(t *testing.T) {
	fsys := baseTree(t)
	apply(t, fsys, replaceTopf("    ip: 10.10.20.21\n", ""))

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := (Host{Name: "wrk-01", Role: "worker", IP: ""}); got.Hosts[1] != want {
		t.Errorf("Hosts[1] = %+v, want %+v", got.Hosts[1], want)
	}
}

func TestLoad_Accepted(t *testing.T) {
	tests := []struct {
		name  string
		edits []edit
	}{
		{
			name:  "topf.yaml is a link inside the tree",
			edits: []edit{moveFile(topfName, "real/topf.yaml"), linkTo(topfName, "real/topf.yaml")},
		},
		{name: "a quoted timestamp in data", edits: []edit{appendTopf("data:\n  t: \"2026-01-01\"\n")}},
		{name: "plain data values", edits: []edit{appendTopf("data:\n  a: 1\n  b: [x, 2.5, true, null]\n  c: {d: e}\n")}},
		{name: "an op reference in data", edits: []edit{appendTopf("data:\n  token: ref+op://v/i/f\n")}},
		{
			name: "text that only looks like SOPS metadata",
			edits: []edit{
				writeFile("docs/a.yaml", "sops: plain\n"),
				writeFile("docs/b.yaml", "- sops:\n    mac: x\n"),
				writeFile("docs/c.yaml", "a: 1\n---\nsops:\n  mac: x\n"),
				writeFile("docs/d.txt", "not yaml: ["),
			},
		},
		{name: "a template with an allowed function", edits: []edit{writeFile("all/u.yaml.tpl", "x: {{ upper .Node.Host }}\n")}},
		{name: "a template that does not parse is left to the render", edits: []edit{writeFile("all/bad.yaml.tpl", "x: {{ .Node.Host \n")}},
		{name: "a patch that does not decode is left to the render", edits: []edit{writeFile("all/bad.yaml", "a: [\n")}},
		{name: "a reference as a mapping key is not scanned", edits: []edit{writeFile("all/k.yaml", "ref+vault://x: 1\n")}},
		{name: "ref+ without a scheme separator", edits: []edit{writeFile("all/n.yaml", "x: ref+op is a note\n")}},
		{
			name:  "a non-op reference outside every patch folder",
			edits: []edit{writeFile("docs/other.yaml", "x: ref+vault://a/b\n")},
		},
		{
			name:  "a patch folder that is a link inside the tree",
			edits: []edit{linkTo("worker", "control-plane")},
		},
		{
			name:  "a node talosVersion equal after normalising",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    talosVersion: 1.12.11\n")},
		},
		{name: "a missing @ schematic file is left to the render", edits: []edit{appendTopf("schematicId: '@nope.yaml'\n")}},
		{name: "an empty secrets file is left to the render", edits: []edit{writeFile("secrets.yaml", "")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, tt.edits...)

			if _, err := Load(fsys); err != nil {
				t.Errorf("Load error = %v, want nil", err)
			}
		})
	}
}

func TestLoad_Refusals(t *testing.T) {
	const (
		unparsable   = "topf.yaml unparsable"
		refInIdent   = "reference in host, ip, schematicId, talosVersion or kubernetesVersion"
		notDNS       = "host not a DNS name"
		notID        = "schematicId not an id or @path"
		nonPlain     = "non-plain data value"
		secretsGone  = "secrets bundle missing"
		sopsMetadata = "sops metadata"
		refNotOp     = "reference not op"
		refEmbedded  = "embedded reference"
		refUnres     = "reference in an unresolved file"
		kubeMissing  = "kubernetesVersion missing"
		talosMissing = "talosVersion missing"
		talosRange   = "talosVersion out of range"
		talosDiffers = "node talosVersion differs"
		factory      = "factory not default"
		platform     = "platform not metal"
		secureboot   = "secureboot enabled"
		noPatchesDir = "template reads PatchesDir"
	)

	tests := []struct {
		name  string
		edits []edit
		file  string
		rule  string
	}{
		// 1. topf.yaml unparsable
		{name: "topf.yaml absent", edits: []edit{remove(topfName)}, file: topfName, rule: unparsable},
		{name: "topf.yaml not yaml", edits: []edit{writeFile(topfName, "a: [\n")}, file: topfName, rule: unparsable},
		{name: "topf.yaml not a mapping", edits: []edit{writeFile(topfName, "- a\n- b\n")}, file: topfName, rule: unparsable},
		{name: "invalid role", edits: []edit{replaceTopf("role: worker", "role: boss")}, file: topfName, rule: unparsable},
		{name: "invalid ip", edits: []edit{replaceTopf("ip: 10.10.20.21", "ip: not-an-ip")}, file: topfName, rule: unparsable},
		{name: "node without host", edits: []edit{replaceTopf("host: wrk-01\n    ", "")}, file: topfName, rule: unparsable},
		{name: "topf.yaml links out of the tree", edits: []edit{linkTo(topfName, "../topf.yaml")}, file: topfName, rule: ruleEscape},
		{name: "topf.yaml is a dangling link", edits: []edit{linkTo(topfName, "missing.yaml")}, file: topfName, rule: ruleEscape},
		{name: "topf.yaml is a link loop", edits: []edit{linkTo(topfName, topfName)}, file: topfName, rule: ruleEscape},

		// 2. providers and configDir
		{name: "nodesProvider", edits: []edit{appendTopf("nodesProvider: /bin/x\n")}, file: topfName, rule: "nodesProvider set"},
		{name: "secretsProvider", edits: []edit{appendTopf("secretsProvider: /bin/x\n")}, file: topfName, rule: "secretsProvider set"},
		{name: "configDir", edits: []edit{appendTopf("configDir: old\n")}, file: topfName, rule: "configDir set"},

		// 3. patchesDir and secretsPath
		{name: "patchesDir leaves the tree", edits: []edit{appendTopf("patchesDir: ../p\n")}, file: topfName, rule: ruleEscape},
		{name: "absolute patchesDir", edits: []edit{appendTopf("patchesDir: /p\n")}, file: topfName, rule: ruleEscape},
		{name: "secretsPath leaves the tree", edits: []edit{appendTopf("secretsPath: ../s.yaml\n")}, file: topfName, rule: ruleEscape},
		{name: "absolute secretsPath", edits: []edit{appendTopf("secretsPath: /s.yaml\n")}, file: topfName, rule: ruleEscape},
		{
			name:  "patchesDir is a link out of the tree",
			edits: []edit{appendTopf("patchesDir: p\n"), linkTo("p", "../elsewhere")},
			file:  topfName, rule: ruleEscape,
		},
		{
			name:  "secretsPath is a link out of the tree",
			edits: []edit{appendTopf("secretsPath: s.yaml\n"), linkTo("s.yaml", "../elsewhere.yaml")},
			file:  topfName, rule: ruleEscape,
		},

		// 4. references in identity fields, then a typed field that does not decode
		{name: "reference in host", edits: []edit{replaceTopf("host: wrk-01", "host: ref+op://v/i/f")}, file: topfName, rule: refInIdent},
		{
			name:  "reference in kubernetesVersion",
			edits: []edit{replaceTopf("kubernetesVersion: v1.35.10", "kubernetesVersion: ref+op://v/i/f")},
			file:  topfName, rule: refInIdent,
		},
		{name: "reference in ip", edits: []edit{replaceTopf("ip: 10.10.20.21", "ip: ref+op://v/i/f")}, file: topfName, rule: refInIdent},
		{name: "reference in schematicId", edits: []edit{appendTopf("schematicId: ref+op://v/i/f\n")}, file: topfName, rule: refInIdent},
		{
			name:  "reference in node schematicId",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    schematicId: ref+op://v/i/f\n")},
			file:  topfName, rule: refInIdent,
		},
		{
			name:  "reference in talosVersion",
			edits: []edit{replaceTopf("talosVersion: v1.12.11", "talosVersion: ref+op://v/i/f")},
			file:  topfName, rule: refInIdent,
		},
		{
			name:  "reference in node talosVersion",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    talosVersion: ref+vault://x\n")},
			file:  topfName, rule: refInIdent,
		},

		// 5. host names
		{name: "upper-case host", edits: []edit{replaceTopf("host: cp-01", "host: Cp-01")}, file: topfName, rule: notDNS},
		{name: "host with a slash", edits: []edit{replaceTopf("host: cp-01", "host: a/b")}, file: topfName, rule: notDNS},
		{name: "host with a trailing dot", edits: []edit{replaceTopf("host: cp-01", "host: cp-01.")}, file: topfName, rule: notDNS},
		{name: "host with an empty label", edits: []edit{replaceTopf("host: cp-01", "host: a..b")}, file: topfName, rule: notDNS},
		{name: "host with an underscore", edits: []edit{replaceTopf("host: cp-01", "host: cp_01")}, file: topfName, rule: notDNS},
		{name: "duplicate host", edits: []edit{replaceTopf("host: wrk-01", "host: cp-01")}, file: topfName, rule: "duplicate host"},

		// 6. versions
		{name: "kubernetesVersion missing", edits: []edit{replaceTopf("kubernetesVersion: v1.35.10\n", "")}, file: topfName, rule: kubeMissing},
		{name: "talosVersion missing", edits: []edit{replaceTopf("talosVersion: v1.12.11\n", "")}, file: topfName, rule: talosMissing},
		{name: "talosVersion above the range", edits: []edit{replaceTopf("v1.12.11", "v1.15.0")}, file: topfName, rule: talosRange},
		{
			name:  "node talosVersion differs",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    talosVersion: v1.13.0\n")},
			file:  topfName, rule: talosDiffers,
		},

		// 7. factory, platform, secure boot
		{name: "cluster factory", edits: []edit{appendTopf("factory: example.org\n")}, file: topfName, rule: factory},
		{
			name:  "node factory",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    factory: example.org\n")},
			file:  topfName, rule: factory,
		},
		{name: "cluster platform", edits: []edit{appendTopf("platform: aws\n")}, file: topfName, rule: platform},
		{
			name:  "node platform",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    platform: aws\n")},
			file:  topfName, rule: platform,
		},
		{name: "cluster secureboot", edits: []edit{appendTopf("secureboot: true\n")}, file: topfName, rule: secureboot},
		{
			name:  "node secureboot",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    secureboot: true\n")},
			file:  topfName, rule: secureboot,
		},

		// 8. schematic id format and @ paths
		{name: "short schematicId", edits: []edit{appendTopf("schematicId: \"3765\"\n")}, file: topfName, rule: notID},
		{
			name:  "node schematicId not an id",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    schematicId: nope\n")},
			file:  topfName, rule: notID,
		},
		{name: "empty @ path", edits: []edit{appendTopf("schematicId: '@'\n")}, file: topfName, rule: notID},
		{name: "@ path leaves the tree", edits: []edit{appendTopf("schematicId: '@../s.yaml'\n")}, file: topfName, rule: ruleEscape},
		{name: "absolute @ path", edits: []edit{appendTopf("schematicId: '@/s.yaml'\n")}, file: topfName, rule: ruleEscape},
		{
			name:  "node @ path leaves the tree",
			edits: []edit{replaceTopf("role: worker\n", "role: worker\n    schematicId: '@../s.yaml'\n")},
			file:  topfName, rule: ruleEscape,
		},
		{
			name:  "@ path is a link out of the tree",
			edits: []edit{appendTopf("schematicId: '@s.yaml'\n"), linkTo("s.yaml", "../elsewhere.yaml")},
			file:  topfName, rule: ruleEscape,
		},

		// 9. data values
		{name: "timestamp in data", edits: []edit{appendTopf("data:\n  t: 2026-01-01\n")}, file: topfName, rule: nonPlain},

		// 10. secrets bundle
		{name: "secrets bundle absent", edits: []edit{remove("secrets.yaml")}, file: "secrets.yaml", rule: secretsGone},
		{
			name:  "secrets bundle at secretsPath absent",
			edits: []edit{appendTopf("secretsPath: vault/s.yaml\n")},
			file:  "vault/s.yaml", rule: secretsGone,
		},
		{
			name:  "secrets bundle is a directory",
			edits: []edit{remove("secrets.yaml"), writeFile("secrets.yaml/inner", "x")},
			file:  "secrets.yaml", rule: secretsGone,
		},

		// 11. links in the tree and SOPS
		{
			name:  "link out of the tree",
			edits: []edit{linkTo("all/esc.yaml", "../../outside.yaml")},
			file:  "all/esc.yaml", rule: ruleEscape,
		},
		{name: "absolute link", edits: []edit{linkTo("docs/abs", "/etc/passwd")}, file: "docs/abs", rule: ruleEscape},
		{name: "dangling link", edits: []edit{linkTo("all/dangling.yaml", "nope.yaml")}, file: "all/dangling.yaml", rule: ruleEscape},
		{name: "link to itself", edits: []edit{linkTo("all/self", "self")}, file: "all/self", rule: ruleEscape},
		{name: "directory link to an ancestor", edits: []edit{linkTo("all/up", "..")}, file: "all/up", rule: ruleEscape},
		{name: "directory link to its own directory", edits: []edit{linkTo("all/here", ".")}, file: "all/here", rule: ruleEscape},
		{
			name:  "directory links in a cycle",
			edits: []edit{linkTo("a/l", "../b"), linkTo("b/l", "../a")},
			file:  "b/l", rule: ruleEscape,
		},
		{
			name:  "SOPS metadata in a patch",
			edits: []edit{writeFile("all/enc.yaml", "sops:\n  mac: x\ndata: y\n")},
			file:  "all/enc.yaml", rule: sopsMetadata,
		},
		{
			name:  "SOPS metadata in JSON outside every patch folder",
			edits: []edit{writeFile("docs/enc.json", `{"sops": {"mac": "x"}, "data": "y"}`)},
			file:  "docs/enc.json", rule: sopsMetadata,
		},
		{
			name:  "SOPS metadata in a file with another extension",
			edits: []edit{writeFile("notes.txt", "sops:\n  mac: x\n")},
			file:  "notes.txt", rule: sopsMetadata,
		},

		// 12. references in resolved files
		{
			name:  "reference that is not op",
			edits: []edit{writeFile("all/ref.yaml", "x: ref+vault://a/b\n")},
			file:  "all/ref.yaml", rule: refNotOp,
		},
		{
			name:  "op reference with two segments",
			edits: []edit{writeFile("all/ref.yaml", "x: ref+op://v/i\n")},
			file:  "all/ref.yaml", rule: refNotOp,
		},
		{
			name:  "op reference with a query",
			edits: []edit{writeFile("all/ref.yaml", "x: ref+op://v/i/f?attr=otp\n")},
			file:  "all/ref.yaml", rule: refNotOp,
		},
		{
			name:  "reference that is not op in a list",
			edits: []edit{writeFile("all/ref.yaml", "x:\n  - a\n  - ref+vault://a/b\n")},
			file:  "all/ref.yaml", rule: refNotOp,
		},
		{
			name:  "reference that is not op in topf.yaml",
			edits: []edit{appendTopf("data:\n  x: ref+vault://a/b\n")},
			file:  topfName, rule: refNotOp,
		},
		{
			name:  "reference that is not op in the secrets bundle",
			edits: []edit{writeFile("secrets.yaml", "cluster:\n  id: ref+vault://a/b\n")},
			file:  "secrets.yaml", rule: refNotOp,
		},
		{
			name:  "reference that is not op in a patch reached through a link",
			edits: []edit{linkTo("worker", "control-plane"), writeFile("control-plane/r.yaml", "x: ref+vault://a/b\n")},
			file:  "control-plane/r.yaml", rule: refNotOp,
		},
		{
			name:  "reference embedded in a string",
			edits: []edit{writeFile("all/ref.yaml", "x: \"Bearer ref+op://v/i/f\"\n")},
			file:  "all/ref.yaml", rule: refEmbedded,
		},
		{
			name:  "reference embedded in a block scalar",
			edits: []edit{writeFile("all/ref.yaml", "x: |\n  line one\n  ref+vault://a/b\n")},
			file:  "all/ref.yaml", rule: refEmbedded,
		},

		// 13. unresolved files
		{
			name:  "reference in a template patch",
			edits: []edit{writeFile("all/ref.yaml.tpl", "x: ref+op://v/i/f\n")},
			file:  "all/ref.yaml.tpl", rule: refUnres,
		},
		{
			name:  "template function outside the allow-list",
			edits: []edit{writeFile("all/keys.yaml.tpl", "x: {{ keys .Data }}\n")},
			file:  "all/keys.yaml.tpl", rule: "template function not allowed: keys",
		},
		{
			name:  "template file named like a parser message",
			edits: []edit{writeFile("all/function \"x\" not defined.yaml.tpl", "x: {{ keys .Data }}\n")},
			file:  "all/function \"x\" not defined.yaml.tpl", rule: "template function not allowed: keys",
		},
		{
			name:  "template function that does not exist",
			edits: []edit{writeFile("all/nope.yaml.tpl", "x: {{ nope 1 }}\n")},
			file:  "all/nope.yaml.tpl", rule: "template function not allowed: nope",
		},
		{
			name:  "template reads PatchesDir as a field",
			edits: []edit{writeFile("all/p.yaml.tpl", "x: {{ .PatchesDir }}\n")},
			file:  "all/p.yaml.tpl", rule: noPatchesDir,
		},
		{
			name:  "template reads PatchesDir through a variable",
			edits: []edit{writeFile("all/p.yaml.tpl", "{{ $c := . }}x: {{ $c.PatchesDir }}\n")},
			file:  "all/p.yaml.tpl", rule: noPatchesDir,
		},
		{
			name:  "template reads PatchesDir through a chain",
			edits: []edit{writeFile("all/p.yaml.tpl", "x: {{ (.).PatchesDir }}\n")},
			file:  "all/p.yaml.tpl", rule: noPatchesDir,
		},
		{
			name:  "template reads PatchesDir in a branch",
			edits: []edit{writeFile("all/p.yaml.tpl", "{{ if .Data }}a{{ else }}{{ range .Data }}{{ .PatchesDir }}{{ end }}{{ end }}\n")},
			file:  "all/p.yaml.tpl", rule: noPatchesDir,
		},
		{
			name:  "template reads PatchesDir in a defined template",
			edits: []edit{writeFile("all/p.yaml.tpl", `{{ define "x" }}{{ with .Node }}{{ .PatchesDir }}{{ end }}{{ end }}y`)},
			file:  "all/p.yaml.tpl", rule: noPatchesDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, tt.edits...)

			got := loadRefusal(t, fsys)
			if got.File != tt.file || got.Rule != tt.rule {
				t.Errorf("refusal = {File: %q, Rule: %q}, want {File: %q, Rule: %q}", got.File, got.Rule, tt.file, tt.rule)
			}

			if want := "render: " + tt.file + ": " + tt.rule; got.Error() != want {
				t.Errorf("Error() = %q, want %q", got.Error(), want)
			}
		})
	}
}

// TestLoad_RuleOrder pairs every rule with each later one: both violations present, the earlier rule's refusal returned.
func TestLoad_RuleOrder(t *testing.T) {
	steps := []struct {
		rule  string
		edits []edit
	}{
		{"nodesProvider set", []edit{appendTopf("nodesProvider: /bin/x\n")}},
		{ruleEscape, []edit{appendTopf("patchesDir: ../p\n")}},
		{"reference in host, ip, schematicId, talosVersion or kubernetesVersion", []edit{replaceTopf("host: wrk-01", "host: ref+op://v/i/f")}},
		{"topf.yaml unparsable", []edit{replaceTopf("role: worker", "role: boss")}},
		{"host not a DNS name", []edit{replaceTopf("host: cp-01", "host: Cp-01")}},
		{"kubernetesVersion missing", []edit{replaceTopf("kubernetesVersion: v1.35.10\n", "")}},
		{"factory not default", []edit{appendTopf("factory: example.org\n")}},
		{"schematicId not an id or @path", []edit{appendTopf("schematicId: abc\n")}},
		{"non-plain data value", []edit{appendTopf("data:\n  t: 2026-01-01\n")}},
		{"secrets bundle missing", []edit{remove("secrets.yaml")}},
		{"sops metadata", []edit{writeFile("all/enc.yaml", "sops:\n  mac: x\n")}},
		{"reference not op", []edit{writeFile("all/ref.yaml", "x: ref+vault://a/b\n")}},
		{"reference in an unresolved file", []edit{writeFile("all/ref.yaml.tpl", "x: ref+op://v/i/f\n")}},
		{"template function not allowed: keys", []edit{writeFile("all/keys.yaml.tpl", "x: {{ keys .Data }}\n")}},
	}

	for i, first := range steps {
		for _, second := range steps[i+1:] {
			t.Run(first.rule+" before "+second.rule, func(t *testing.T) {
				fsys := baseTree(t)
				apply(t, fsys, first.edits...)
				apply(t, fsys, second.edits...)

				if got := loadRefusal(t, fsys); got.Rule != first.rule {
					t.Errorf("refusal rule = %q, want %q", got.Rule, first.rule)
				}
			})
		}
	}
}

func TestLoad_FQDNHostAccepted(t *testing.T) {
	label63 := strings.Repeat("a", 63)

	tests := []struct {
		name string
		host string
		want string // the refusal rule, "" when accepted
	}{
		{name: "FQDN", host: "cp-01.lab.example.test"},
		{name: "63-character label", host: label63},
		{name: "253 characters", host: label63 + "." + label63 + "." + label63 + "." + strings.Repeat("a", 61)},
		{name: "upper case", host: "Cp-01", want: "host not a DNS name"},
		{name: "slash", host: "a/b", want: "host not a DNS name"},
		{name: "64-character label", host: strings.Repeat("a", 64), want: "host not a DNS name"},
		{name: "254 characters", host: label63 + "." + label63 + "." + label63 + "." + strings.Repeat("a", 62), want: "host not a DNS name"},
		{name: "leading dot", host: ".a", want: "host not a DNS name"},
		{name: "non-ASCII", host: "é", want: "host not a DNS name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, replaceTopf("host: cp-01", "host: "+tt.host))

			got, err := Load(fsys)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Load error = %v, want nil", err)
				}

				if got.Hosts[0].Name != tt.host {
					t.Errorf("Hosts[0].Name = %q, want %q", got.Hosts[0].Name, tt.host)
				}

				return
			}

			if refusal, ok := errors.AsType[*RefusalError](err); !ok || refusal.Rule != tt.want {
				t.Errorf("Load error = %v, want a refusal %q", err, tt.want)
			}
		})
	}
}

func TestLoad_PreRelease(t *testing.T) {
	tests := []struct {
		version string
		want    string // the normalised version; "" when refused
	}{
		{version: "v1.12.11", want: "v1.12.11"},
		{version: "v1.14.6", want: "v1.14.6"},
		{version: "v1.14.99", want: "v1.14.99"},
		{version: "1.13.12", want: "v1.13.12"},
		{version: "v1.15.0-beta.1"},
		{version: "v1.14.7-rc.0"},
		{version: "v1.13.0+build"},
		{version: "v1.12.10"},
		{version: "v1.11.9"},
		{version: "v1.15.0"},
		{version: "v2.0.0"},
		{version: "v0.13.0"},
		{version: "v1.13"},
		{version: "v1.13.0.1"},
		{version: "v1.13.x"},
		{version: "v1.13.012"},
		{version: "v1.13.-5"},
		{version: "vv1.13.0"},
		{version: "V1.13.0"},
		{version: "v1.13.99999999999999999999"},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, replaceTopf("talosVersion: v1.12.11", "talosVersion: "+tt.version))

			got, err := Load(fsys)
			if tt.want == "" {
				if refusal, ok := errors.AsType[*RefusalError](err); !ok || refusal.Rule != "talosVersion out of range" {
					t.Errorf("Load error = %v, want a refusal %q", err, "talosVersion out of range")
				}

				return
			}

			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if got.TalosVersion != tt.want {
				t.Errorf("TalosVersion = %q, want %q", got.TalosVersion, tt.want)
			}
		})
	}
}

func TestLoad_KubernetesVersionNormalised(t *testing.T) {
	fsys := baseTree(t)
	apply(t, fsys, replaceTopf("kubernetesVersion: v1.35.10", "kubernetesVersion: 1.35.10"))

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.KubernetesVersion != "v1.35.10" {
		t.Errorf("KubernetesVersion = %q, want v1.35.10", got.KubernetesVersion)
	}
}

func TestLoad_TimestampInData(t *testing.T) {
	tests := []struct {
		name    string
		edits   []edit
		refused bool
	}{
		{name: "unquoted timestamp", edits: []edit{appendTopf("data:\n  t: 2026-01-01\n")}, refused: true},
		{name: "nested timestamp", edits: []edit{appendTopf("data:\n  a:\n    - b: 2026-01-01T10:00:00Z\n")}, refused: true},
		{name: "non-string key", edits: []edit{appendTopf("data:\n  a:\n    1: x\n")}, refused: true},
		{
			name:    "node timestamp tagged",
			edits:   []edit{replaceTopf("role: worker\n", "role: worker\n    data:\n      t: !!timestamp \"2026-01-01\"\n")},
			refused: true,
		},
		{name: "quoted", edits: []edit{appendTopf("data:\n  t: \"2026-01-01\"\n")}},
		{name: "node quoted", edits: []edit{replaceTopf("role: worker\n", "role: worker\n    data:\n      t: \"2026-01-01\"\n")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, tt.edits...)

			_, err := Load(fsys)
			if !tt.refused {
				if err != nil {
					t.Errorf("Load error = %v, want nil", err)
				}

				return
			}

			if refusal, ok := errors.AsType[*RefusalError](err); !ok || refusal.Rule != "non-plain data value" {
				t.Errorf("Load error = %v, want a refusal %q", err, "non-plain data value")
			}
		})
	}
}

func TestLoad_SchematicIDFormat(t *testing.T) {
	const (
		plainID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
		upperID = "376567988AD370138AD8B2698212367B8EDCB69B5FD68C80BE1F2EC7D603B4BA"
	)

	tests := []struct {
		name       string
		schematic  string
		wantRefuse bool
	}{
		{name: "64 lower-case hex", schematic: plainID},
		{name: "empty", schematic: ""},
		{name: "@ path", schematic: "'@schematic.yaml'"},
		{name: "@ path below a folder", schematic: "'@schematics/lab.yaml.tpl'"},
		{name: "short", schematic: "\"3765\"", wantRefuse: true},
		{name: "upper-case hex", schematic: upperID, wantRefuse: true},
		{name: "63 hex", schematic: plainID[1:], wantRefuse: true},
		{name: "65 hex", schematic: plainID + "0", wantRefuse: true},
		{name: "non-hex", schematic: strings.Repeat("g", 64), wantRefuse: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys,
				appendTopf("schematicId: "+tt.schematic+"\n"),
				writeFile("schematic.yaml", "customization: {}\n"),
				writeFile("schematics/lab.yaml.tpl", "customization: {}\n"),
			)

			got, err := Load(fsys)
			if tt.wantRefuse {
				if refusal, ok := errors.AsType[*RefusalError](err); !ok || refusal.Rule != "schematicId not an id or @path" {
					t.Errorf("Load error = %v, want a refusal %q", err, "schematicId not an id or @path")
				}

				return
			}

			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			want := strings.Trim(tt.schematic, "'")
			if got.schematics["cp-01"] != want || got.schematics["wrk-01"] != want {
				t.Errorf("schematics = %v, want %q for both hosts", got.schematics, want)
			}
		})
	}
}

func TestLoad_NodeSchematicOverridesCluster(t *testing.T) {
	const nodeID = "1111111111111111111111111111111111111111111111111111111111111111"

	fsys := baseTree(t)
	apply(t, fsys,
		appendTopf("schematicId: '@schematic.yaml'\n"),
		writeFile("schematic.yaml", "customization: {}\n"),
		replaceTopf("role: worker\n", "role: worker\n    schematicId: "+nodeID+"\n"),
	)

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string]string{"cp-01": "@schematic.yaml", "wrk-01": nodeID}
	if !maps.Equal(got.schematics, want) {
		t.Errorf("schematics = %v, want %v", got.schematics, want)
	}
}

func TestLoad_ReferenceInUnresolvedFile(t *testing.T) {
	tests := []struct {
		name  string
		edits []edit
		file  string
	}{
		{
			name:  "template patch",
			edits: []edit{writeFile("all/ref.yaml.tpl", "x: ref+op://v/i/f\n")},
			file:  "all/ref.yaml.tpl",
		},
		{
			name:  "template patch with a reference in a comment",
			edits: []edit{writeFile("all/ref.yaml.tpl", "# ref+vault://a/b\nx: 1\n")},
			file:  "all/ref.yaml.tpl",
		},
		{
			name: "schematic file",
			edits: []edit{
				appendTopf("schematicId: '@schematic.yaml'\n"),
				writeFile("schematic.yaml", "customization:\n  extraKernelArgs:\n    - ref+op://v/i/f\n"),
			},
			file: "schematic.yaml",
		},
		{
			name: "node schematic template",
			edits: []edit{
				replaceTopf("role: worker\n", "role: worker\n    schematicId: '@s/w.yaml.tpl'\n"),
				writeFile("s/w.yaml.tpl", "x: ref+op://v/i/f\n"),
			},
			file: "s/w.yaml.tpl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, tt.edits...)

			want := RefusalError{File: tt.file, Rule: "reference in an unresolved file"}
			if got := loadRefusal(t, fsys); *got != want {
				t.Errorf("refusal = %+v, want %+v", *got, want)
			}
		})
	}
}

func TestLoad_RefsFromResolvedFiles(t *testing.T) {
	fsys := baseTree(t)
	apply(t, fsys,
		appendTopf("data:\n  list:\n    - ref+op://z/top/f\n    - ref+op://z/top/f\n"),
		writeFile("control-plane/c.yaml", "x: ref+op://z/cp/s/f\n"),
		linkTo("worker", "control-plane"),
		writeFile("node/wrk-01/n.yml", "x: ref+op://z/node/f\n---\ny: ref+op://z/node/f\n"),
		// Not scanned for references of their own: a plain "ref" word and a mapping key.
		writeFile("all/plain.yaml", "ref+op://a/b/c: ref\n"),
	)

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, want := range []string{"ref+op://z/top/f", "ref+op://z/cp/s/f", "ref+op://z/node/f", "ref+op://lab/bundle/cluster.id"} {
		if !slices.Contains(got.Refs, want) {
			t.Errorf("Refs lacks %q: %v", want, got.Refs)
		}
	}

	if slices.Contains(got.Refs, "ref+op://a/b/c") {
		t.Error("Refs holds a mapping key")
	}

	if !slices.IsSorted(got.Refs) || len(slices.Compact(slices.Clone(got.Refs))) != len(got.Refs) {
		t.Errorf("Refs is not sorted and de-duplicated: %v", got.Refs)
	}
}

func TestLoad_PatchesDirAndSecretsPath(t *testing.T) {
	fsys := baseTree(t)
	apply(t, fsys,
		appendTopf("patchesDir: defs/./patches\nsecretsPath: ./vault/../vault/s.yaml\n"),
		moveFile("secrets.yaml", "vault/s.yaml"),
		writeFile("defs/patches/all/p.yaml", "x: ref+op://z/patches/f\n"),
		// Outside patchesDir, so never loaded: its non-op reference is not refused.
		writeFile("all/other.yaml", "x: ref+vault://a/b\n"),
	)

	got, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.secretsPath != "vault/s.yaml" {
		t.Errorf("secretsPath = %q, want vault/s.yaml", got.secretsPath)
	}

	if !slices.Contains(got.Refs, "ref+op://z/patches/f") || !slices.Contains(got.Refs, "ref+op://lab/bundle/cluster.id") {
		t.Errorf("Refs = %v, want the patch under patchesDir and the bundle at secretsPath", got.Refs)
	}
}

func TestLoad_ErrorsNeverQuoteContent(t *testing.T) {
	const secret = "SECRETVALUE"

	tests := []struct {
		name  string
		edits []edit
	}{
		{name: "yaml that does not decode", edits: []edit{writeFile(topfName, "clusterName: ["+secret+"\n")}},
		{name: "typed field that does not decode", edits: []edit{replaceTopf("role: worker", "role: "+secret)}},
		{name: "reference in host", edits: []edit{replaceTopf("host: wrk-01", "host: ref+op://"+secret+"/i/f")}},
		{name: "host", edits: []edit{replaceTopf("host: wrk-01", "host: "+secret)}},
		{name: "talosVersion", edits: []edit{replaceTopf("talosVersion: v1.12.11", "talosVersion: "+secret)}},
		{name: "schematicId", edits: []edit{appendTopf("schematicId: " + secret + "\n")}},
		{name: "data", edits: []edit{appendTopf("data:\n  k: 2026-01-01\n  " + secret + ": 2026-01-01\n")}},
		{name: "reference that is not op", edits: []edit{writeFile("all/r.yaml", "x: ref+vault://"+secret+"/a\n")}},
		{name: "embedded reference", edits: []edit{writeFile("all/r.yaml", "x: a ref+op://"+secret+"/i/f\n")}},
		{name: "reference in an unresolved file", edits: []edit{writeFile("all/r.yaml.tpl", "x: ref+op://"+secret+"/i/f\n")}},
		{name: "SOPS metadata", edits: []edit{writeFile("all/s.yaml", "sops:\n  mac: "+secret+"\n")}},
		{name: "template function outside the allow-list", edits: []edit{writeFile("all/t.yaml.tpl", "{{ keys "+secret+" }}\n")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := baseTree(t)
			apply(t, fsys, tt.edits...)

			_, err := Load(fsys)
			if err == nil {
				t.Fatal("Load error = nil, want a refusal")
			}

			if strings.Contains(err.Error(), secret) {
				t.Errorf("Load error %q quotes file content", err.Error())
			}
		})
	}
}

func FuzzLoad(f *testing.F) {
	secrets := bundleFile(f, "secrets.yaml")

	for _, seed := range []string{
		baseTopf,
		"",
		"- a\n",
		"a: [\n",
		baseTopf + "nodesProvider: /bin/x\n",
		baseTopf + "patchesDir: ../p\n",
		baseTopf + "secretsPath: all\n",
		baseTopf + "schematicId: '@all'\n",
		baseTopf + "schematicId: '@../s.yaml'\n",
		baseTopf + "data:\n  t: 2026-01-01\n",
		baseTopf + "factory: example.org\nplatform: aws\nsecureboot: true\n",
		strings.Replace(baseTopf, "host: cp-01", "host: Cp-01", 1),
		strings.Replace(baseTopf, "ip: 10.10.20.11", "ip: ref+op://v/i/f", 1),
		strings.Replace(baseTopf, "v1.12.11", "v1.15.0-beta.1", 1),
		strings.Replace(baseTopf, "kubernetesVersion: v1.35.10\n", "", 1),
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, topf []byte) {
		fsys := newTree(secrets)
		fsys[topfName] = &fstest.MapFile{Data: topf}

		_, err := Load(fsys)
		if err == nil {
			return
		}

		if _, ok := errors.AsType[*RefusalError](err); !ok {
			t.Errorf("Load error = %v, want nil or a *RefusalError", err)
		}
	})
}
