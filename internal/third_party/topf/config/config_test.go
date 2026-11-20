// hephaestus: new

package config

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
)

// baseTopf lists the worker before the control plane so the load sort is observable.
const baseTopf = `clusterName: lab
clusterEndpoint: https://10.10.20.11:6443
kubernetesVersion: v1.35.10
talosVersion: v1.12.11
nodes:
  - host: wrk-01
    ip: 10.10.20.21
    role: worker
  - host: cp-01
    ip: 10.10.20.11
    role: control-plane
`

// topfTree returns a definition tree whose topf.yaml is baseTopf followed by extra.
func topfTree(extra string) fstest.MapFS {
	return fstest.MapFS{"topf.yaml": file(baseTopf + extra)}
}

func TestLoad_DefaultsAndSort(t *testing.T) {
	cfg, err := Load(topfTree(""), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PatchesDir != "." {
		t.Errorf("PatchesDir = %q, want %q", cfg.PatchesDir, ".")
	}

	if cfg.SecretsPath != "secrets.yaml" {
		t.Errorf("SecretsPath = %q, want %q", cfg.SecretsPath, "secrets.yaml")
	}

	if len(cfg.Nodes) != 2 || cfg.Nodes[0].Host != "cp-01" || cfg.Nodes[1].Host != "wrk-01" {
		t.Errorf("Nodes = %+v, want cp-01 then wrk-01", cfg.Nodes)
	}

	if cfg.ClusterName != "lab" || cfg.ClusterEndpoint.String() != "https://10.10.20.11:6443" {
		t.Errorf("cluster = %q at %s, want lab at https://10.10.20.11:6443", cfg.ClusterName, cfg.ClusterEndpoint.String())
	}
}

func TestLoad_PathsMustStayInTree(t *testing.T) {
	tests := []struct {
		name            string
		extra           string
		wantErr         error
		wantPatchesDir  string
		wantSecretsPath string
	}{
		{name: "absolute patchesDir", extra: "patchesDir: /abs\n", wantErr: ErrEscapes},
		{name: "patchesDir leaves the tree", extra: "patchesDir: ../x\n", wantErr: ErrEscapes},
		{name: "secretsPath leaves the tree", extra: "secretsPath: ../s.yaml\n", wantErr: ErrEscapes},
		{name: "absolute secretsPath", extra: "secretsPath: /s.yaml\n", wantErr: ErrEscapes},
		{name: "relative patchesDir", extra: "patchesDir: patches\n", wantPatchesDir: "patches", wantSecretsPath: "secrets.yaml"},
		{name: "cleaned secretsPath", extra: "secretsPath: ./a/../s.yaml\n", wantPatchesDir: ".", wantSecretsPath: "s.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(topfTree(tt.extra), nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Load error = %v, want %v", err, tt.wantErr)
			}

			if err != nil {
				return
			}

			if cfg.PatchesDir != tt.wantPatchesDir || cfg.SecretsPath != tt.wantSecretsPath {
				t.Errorf("PatchesDir, SecretsPath = %q, %q, want %q, %q", cfg.PatchesDir, cfg.SecretsPath, tt.wantPatchesDir, tt.wantSecretsPath)
			}
		})
	}
}

func TestLoad_ConfigDirRefused(t *testing.T) {
	_, err := Load(topfTree("configDir: x\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "configDir") {
		t.Errorf("Load error = %v, want one naming configDir", err)
	}
}

func TestLoad_NodesProviderRefused(t *testing.T) {
	_, err := Load(topfTree("nodesProvider: /bin/x\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "nodesProvider") {
		t.Errorf("Load error = %v, want one naming nodesProvider", err)
	}
}

func TestLoad_KeepsSecretsProviderForTheCaller(t *testing.T) {
	cfg, err := Load(topfTree("secretsProvider: /bin/x\n"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.SecretsProvider != "/bin/x" {
		t.Errorf("SecretsProvider = %q, want %q", cfg.SecretsProvider, "/bin/x")
	}
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{name: "no topf.yaml", fsys: fstest.MapFS{}, want: "topf.yaml"},
		{name: "invalid yaml", fsys: fstest.MapFS{"topf.yaml": file("clusterName: [\n")}, want: "decode"},
		{name: "node without host", fsys: fstest.MapFS{"topf.yaml": file("nodes:\n  - role: worker\n")}, want: "host"},
		{name: "invalid role", fsys: fstest.MapFS{"topf.yaml": file("nodes:\n  - host: a\n    role: boss\n")}, want: "role"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.fsys, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLoad_ResolvesReferences(t *testing.T) {
	cfg, err := Load(topfTree("data:\n  token: ref+op://v/i/t\n"), map[string]string{"ref+op://v/i/t": "s3cr3t"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Data["token"]; got != "s3cr3t" {
		t.Errorf("Data[token] = %v, want %q", got, "s3cr3t")
	}
}

func TestLoad_MissingReference(t *testing.T) {
	_, err := Load(topfTree("data:\n  token: ref+op://v/i/t\n"), nil)
	if !isMissingRef(err) {
		t.Errorf("Load error = %v, want a *MissingReferenceError", err)
	}
}

func TestRenderTemplate_FuncsAndMissingKey(t *testing.T) {
	fsys := fstest.MapFS{
		"ok.tpl":      file("{{ up .ClusterName }}"),
		"missing.tpl": file("{{ .Data.nope }}"),
		"nofunc.tpl":  file("{{ nofunc }}"),
	}
	funcs := map[string]any{"up": strings.ToUpper}
	data := TemplateData{ClusterName: "lab", Data: map[string]any{"a": 1}}

	got, err := RenderTemplate(fsys, "ok.tpl", funcs, data)
	if err != nil || string(got) != "LAB" {
		t.Errorf("RenderTemplate(ok.tpl) = %q, %v, want %q", got, err, "LAB")
	}

	for _, name := range []string{"missing.tpl", "nofunc.tpl", "absent.tpl"} {
		if _, err := RenderTemplate(fsys, name, funcs, data); err == nil {
			t.Errorf("RenderTemplate(%s) succeeded, want an error", name)
		}
	}
}

func TestTemplateData_Fields(t *testing.T) {
	ip := netip.MustParseAddr("10.10.20.11")
	ctx := &PatchContext{
		ClusterName:       "lab",
		ClusterEndpoint:   "https://10.10.20.11:6443",
		KubernetesVersion: "v1.35.10",
		TalosVersion:      "v1.12.11",
		SchematicID:       "abc",
		Data:              map[string]any{"k": "v"},
		PatchesDir:        "patches",
		Node: &Node{
			Host:         "cp-01",
			IP:           &ip,
			Role:         RoleControlPlane,
			Data:         map[string]any{"n": 1},
			TalosVersion: "v1.12.10",
			SchematicID:  "def",
			Factory:      "factory.example.test",
			Platform:     "metal",
			SecureBoot:   true,
		},
	}

	got := ctx.TemplateData()

	if got.ClusterName != "lab" || got.ClusterEndpoint != "https://10.10.20.11:6443" || got.KubernetesVersion != "v1.35.10" ||
		got.TalosVersion != "v1.12.11" || got.SchematicID != "abc" || got.Data["k"] != "v" {
		t.Errorf("cluster fields = %+v, want the context's values", got)
	}

	node := got.Node
	if node.Host != "cp-01" || node.Role != "control-plane" || node.Data["n"] != 1 || node.TalosVersion != "v1.12.10" ||
		node.SchematicID != "def" || node.Factory != "factory.example.test" || node.Platform != "metal" || !node.SecureBoot {
		t.Errorf("node fields = %+v, want the node's values", node)
	}

	if node.RuntimeData != (RuntimeData{}) {
		t.Errorf("RuntimeData = %+v, want the zero value", node.RuntimeData)
	}

	if got := node.Endpoint(); got != "10.10.20.11" {
		t.Errorf("Endpoint() = %q, want %q", got, "10.10.20.11")
	}

	*got.Node.IP = netip.MustParseAddr("10.99.99.99")
	if ctx.Node.IP.String() != "10.10.20.11" {
		t.Errorf("changing the template copy changed the context's IP to %s", ctx.Node.IP)
	}

	ctx.Node.IP = nil

	none := ctx.TemplateData().Node
	if none.IP != nil {
		t.Errorf("IP = %v, want nil", none.IP)
	}

	if got := none.Endpoint(); got != "cp-01" {
		t.Errorf("Endpoint() = %q, want %q", got, "cp-01")
	}
}

func TestTemplateData_RendersLikeTopf(t *testing.T) {
	ip := netip.MustParseAddr("10.10.20.11")
	ctx := &PatchContext{
		ClusterName: "lab",
		Data:        map[string]any{"vip": "10.10.20.5"},
		Node:        &Node{Host: "cp-01", IP: &ip, Role: RoleControlPlane},
	}
	fsys := fstest.MapFS{"t.tpl": file("{{.ClusterName}} {{.Node.Host}} {{.Node.Endpoint}} {{.Node.Role}} {{.Data.vip}} {{.Node.IP}}")}

	got, err := RenderTemplate(fsys, "t.tpl", nil, ctx.TemplateData())
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	if want := "lab cp-01 10.10.20.11 control-plane 10.10.20.5 10.10.20.11"; string(got) != want {
		t.Errorf("RenderTemplate = %q, want %q", got, want)
	}

	ctx.Node.IP = nil

	got, err = RenderTemplate(fsys, "t.tpl", nil, ctx.TemplateData())
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	if want := "lab cp-01 cp-01 control-plane 10.10.20.5 <nil>"; string(got) != want {
		t.Errorf("RenderTemplate without an IP = %q, want %q", got, want)
	}
}

// patchText returns the YAML text of a loaded patch.
func patchText(t *testing.T, p configpatcher.Patch) string {
	t.Helper()

	b, ok := p.(interface{ Bytes() ([]byte, error) })
	if !ok {
		t.Fatalf("patch %T has no Bytes method", p)
	}

	out, err := b.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	return string(out)
}

func patchTree() fstest.MapFS {
	return fstest.MapFS{
		"patches/node/cp-01/30-node.yaml":        file("machine:\n  network:\n    hostname: ref+op://v/i/t\n"),
		"patches/node/wrk-01/30-node.yaml":       file("machine:\n  network:\n    hostname: other\n"),
		"patches/control-plane/20-role.yaml.tpl": file("machine:\n  network:\n    hostname: {{ .ClusterName }}-{{ .Node.Role }}\n"),
		"patches/worker/20-role.yaml":            file("machine:\n  network:\n    hostname: worker\n"),
		"patches/all/10-base.yaml":               file("machine:\n  network:\n    hostname: all\n"),
		"patches/all/README.md":                  file("not a patch"),
	}
}

func patchContext(fsys fstest.MapFS) *PatchContext {
	return &PatchContext{
		ClusterName: "lab",
		Node:        &Node{Host: "cp-01", Role: RoleControlPlane},
		PatchesDir:  "patches",
		FS:          fsys,
		Values:      map[string]string{"ref+op://v/i/t": "from-ref"},
	}
}

func TestPatchContext_Load_OrderTemplatesAndReferences(t *testing.T) {
	patches, err := patchContext(patchTree()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"hostname: all", "hostname: lab-control-plane", "hostname: from-ref"}
	if len(patches) != len(want) {
		t.Fatalf("Load returned %d patches, want %d", len(patches), len(want))
	}

	for i, w := range want {
		if got := patchText(t, patches[i]); !strings.Contains(got, w) {
			t.Errorf("patch %d = %q, want it to contain %q", i, got, w)
		}
	}
}

func TestPatchContext_Load_MissingFoldersAreEmpty(t *testing.T) {
	ctx := patchContext(fstest.MapFS{"patches/all/10-base.yaml": file("machine:\n  network:\n    hostname: all\n")})

	patches, err := ctx.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(patches) != 1 {
		t.Errorf("Load returned %d patches, want 1", len(patches))
	}
}

func TestPatchContext_Load_Errors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		check func(error) bool
	}{
		{
			name:  "reference without a value",
			files: map[string]string{"patches/all/a.yaml": "machine:\n  network:\n    hostname: ref+op://v/i/none\n"},
			check: isMissingRef,
		},
		{
			name:  "template without its key",
			files: map[string]string{"patches/all/a.yaml.tpl": "machine:\n  network:\n    hostname: {{ .Data.none }}\n"},
			check: func(err error) bool { return err != nil },
		},
		{
			name:  "array patch refused",
			files: map[string]string{"patches/all/a.yaml": "- op: add\n"},
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "JSON patch") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, content := range tt.files {
				fsys[name] = file(content)
			}

			ctx := patchContext(fsys)
			ctx.Data = map[string]any{}

			if _, err := ctx.Load(); !tt.check(err) {
				t.Errorf("Load error = %v, want the expected failure", err)
			}
		})
	}
}

func TestPatchContext_Load_TemplateOutputIsNotResolved(t *testing.T) {
	ctx := patchContext(fstest.MapFS{
		"patches/all/a.yaml.tpl": file("machine:\n  network:\n    hostname: ref+op://v/i/t\n"),
	})

	patches, err := ctx.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(patches) != 1 || !strings.Contains(patchText(t, patches[0]), "ref+op://v/i/t") {
		t.Errorf("a template's output was resolved; want the reference text kept, got %d patches", len(patches))
	}
}
