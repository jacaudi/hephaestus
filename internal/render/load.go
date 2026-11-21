package render

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// The load rules, as a *RefusalError carries them. They are what the service stores and shows, so each is fixed text,
// and none ever holds a value of the definition.
const (
	ruleTopfUnparsable     = "topf.yaml unparsable"
	ruleNodesProvider      = "nodesProvider set"
	ruleSecretsProvider    = "secretsProvider set"
	ruleConfigDir          = "configDir set"
	rulePathEscapes        = "path escapes tree"
	ruleRefInIdentity      = "reference in host, ip, schematicId, talosVersion or kubernetesVersion"
	ruleHostNotDNS         = "host not a DNS name"
	ruleHostDuplicate      = "duplicate host"
	ruleKubernetesMissing  = "kubernetesVersion missing"
	ruleTalosMissing       = "talosVersion missing"
	ruleTalosRange         = "talosVersion out of range"
	ruleTalosDiffers       = "node talosVersion differs"
	ruleFactory            = "factory not default"
	rulePlatform           = "platform not metal"
	ruleSecureBoot         = "secureboot enabled"
	ruleSchematicFormat    = "schematicId not an id or @path"
	ruleDataNotPlain       = "non-plain data value"
	ruleSecretsMissing     = "secrets bundle missing"
	ruleSOPS               = "sops metadata"
	ruleRefNotOp           = "reference not op"
	ruleRefEmbedded        = "embedded reference"
	ruleRefUnresolved      = "reference in an unresolved file"
	ruleTemplateFunc       = "template function not allowed: "
	ruleTemplatePatchesDir = "template reads PatchesDir"
)

const (
	topfFile           = "topf.yaml"
	defaultSecretsPath = "secrets.yaml"
	defaultFactory     = "factory.talos.dev"
	defaultPlatform    = "metal"
	templateSuffix     = ".tpl"
)

// RefusalError reports a definition that breaks a load rule. It names the file and the rule and nothing else: never a
// value of the file, and never the text of a YAML or template error, which can quote one.
type RefusalError struct {
	// File is the tree-relative path of the file that breaks the rule.
	File string
	// Rule is the rule's fixed text, for example "duplicate host".
	Rule string
}

// Error returns "render: <file>: <rule>".
func (e *RefusalError) Error() string {
	return "render: " + e.File + ": " + e.Rule
}

func refuse(file, rule string) error {
	return &RefusalError{File: file, Rule: rule}
}

// Cluster is a cluster definition that passed Load.
type Cluster struct {
	// FS is the tree the definition was loaded from.
	FS fs.FS
	// Name is clusterName.
	Name string
	// TalosVersion is the Talos version with a leading "v", for example "v1.12.11".
	TalosVersion string
	// KubernetesVersion is the Kubernetes version with a leading "v", for example "v1.35.10".
	KubernetesVersion string
	// Hosts are the nodes in Topf's order: control planes first, then by host name.
	Hosts []Host
	// Refs holds every whole-scalar reference of the resolved files, sorted and de-duplicated.
	Refs []string

	// schematics maps each host name to its schematicId, the node's own when set and the cluster's otherwise: "", 64
	// lower-case hex digits, or "@" and a path inside the tree.
	schematics map[string]string
	// secretsPath is the cleaned, tree-relative path of the secrets bundle.
	secretsPath string
}

// Host is a node of a Cluster.
type Host struct {
	// Name is nodes[].host.
	Name string
	// Role is "control-plane" or "worker".
	Role string
	// IP is nodes[].ip, or "" when it is unset.
	IP string
}

// definition is topf.yaml as decoded, with the two paths it names, cleaned.
type definition struct {
	cfg         config.TopfConfig
	patchesDir  string
	secretsPath string
}

// Load reads the cluster definition at the root of fsys and applies the load rules in order: the first refusal is
// returned as a *RefusalError. Files are read as Topf reads them (topf.yaml, the secrets bundle, the patch files of
// patchesDir and each @ schematic file); a tree that fails to read for a reason no rule names is an ordinary error.
func Load(fsys fs.FS) (*Cluster, error) {
	def, err := readTopf(fsys)
	if err != nil {
		return nil, err
	}

	cfg := &def.cfg

	for _, check := range []func() error{
		func() error { return checkHosts(cfg.Nodes) },
		func() error { return checkVersions(cfg) },
		func() error { return checkPolicy(cfg) },
		func() error { return checkSchematics(fsys, cfg) },
		func() error { return checkData(cfg) },
		func() error { return checkSecretsExist(fsys, def.secretsPath) },
		func() error { return checkSecretsParse(fsys, def.secretsPath) },
	} {
		if err := check(); err != nil {
			return nil, err
		}
	}

	files, err := walkTree(fsys)
	if err != nil {
		return nil, err
	}

	if err := checkSOPS(fsys, files); err != nil {
		return nil, err
	}

	patches, err := patchFiles(fsys, def)
	if err != nil {
		return nil, err
	}

	plain, templates := splitPatches(patches)

	refs, err := scanResolved(fsys, slices.Concat([]string{topfFile, def.secretsPath}, plain))
	if err != nil {
		return nil, err
	}

	if err := scanUnresolved(fsys, slices.Concat(templates, schematicFiles(cfg))); err != nil {
		return nil, err
	}

	return newCluster(fsys, def, refs), nil
}

func newCluster(fsys fs.FS, def *definition, refs []string) *Cluster {
	cfg := &def.cfg
	cluster := &Cluster{
		FS:                fsys,
		Name:              cfg.ClusterName,
		TalosVersion:      normaliseVersion(cfg.TalosVersion),
		KubernetesVersion: normaliseVersion(cfg.KubernetesVersion),
		Refs:              refs,
		schematics:        make(map[string]string, len(cfg.Nodes)),
		secretsPath:       def.secretsPath,
	}

	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]
		host := Host{Name: node.Host, Role: string(node.Role)}

		if node.IP != nil {
			host.IP = node.IP.String()
		}

		cluster.Hosts = append(cluster.Hosts, host)
		cluster.schematics[node.Host] = cmp.Or(node.SchematicID, cfg.SchematicID)
	}

	return cluster
}

// readTopf applies load rules 1 to 4: topf.yaml reads and decodes, sets no provider, names paths inside the tree,
// holds no reference in an identity field, and decodes into a config.TopfConfig.
func readTopf(fsys fs.FS) (*definition, error) {
	raw, err := readTopfFile(fsys)
	if err != nil {
		return nil, err
	}

	// The text of a decode error can quote the file, so it is dropped.
	var generic any
	if yaml.Unmarshal(raw, &generic) != nil {
		return nil, refuse(topfFile, ruleTopfUnparsable)
	}

	if err := checkGeneric(fsys, generic); err != nil {
		return nil, err
	}

	def := &definition{}
	if yaml.Unmarshal(raw, &def.cfg) != nil {
		return nil, refuse(topfFile, ruleTopfUnparsable)
	}

	// Rule 3 checked string values only. A path that decodes from another scalar (patchesDir: 5) cannot leave the tree by
	// name, and a link it names that leaves the tree is refused by rule 11.
	if def.patchesDir, err = config.CleanRel(cmp.Or(def.cfg.PatchesDir, ".")); err != nil {
		return nil, refuse(topfFile, rulePathEscapes)
	}

	if def.secretsPath, err = config.CleanRel(cmp.Or(def.cfg.SecretsPath, defaultSecretsPath)); err != nil {
		return nil, refuse(topfFile, rulePathEscapes)
	}

	sortNodes(def.cfg.Nodes)

	return def, nil
}

// checkGeneric applies rules 2 to 4 to the generic decode of topf.yaml: no provider and no configDir, paths inside the
// tree, and no reference in an identity field. It runs before any typed field decodes, so the first refusal does not
// depend on which typed field fails.
func checkGeneric(fsys fs.FS, generic any) error {
	for _, key := range []struct{ name, rule string }{
		{"nodesProvider", ruleNodesProvider},
		{"secretsProvider", ruleSecretsProvider},
		{"configDir", ruleConfigDir},
	} {
		if isSet(field(generic, key.name)) {
			return refuse(topfFile, key.rule)
		}
	}

	for _, key := range []string{"patchesDir", "secretsPath"} {
		if p, ok := field(generic, key).(string); ok && p != "" {
			if err := checkTreePath(fsys, p); err != nil {
				return err
			}
		}
	}

	if hasIdentityReference(generic) {
		return refuse(topfFile, ruleRefInIdentity)
	}

	return nil
}

// sortNodes puts the nodes in Topf's order (config.Load): control planes first, then by host name.
func sortNodes(nodes []config.Node) {
	slices.SortStableFunc(nodes, func(a, b config.Node) int {
		switch {
		case a.Role == config.RoleControlPlane && b.Role == config.RoleWorker:
			return -1
		case a.Role == config.RoleWorker && b.Role == config.RoleControlPlane:
			return 1
		default:
			return strings.Compare(a.Host, b.Host)
		}
	})
}

// readTopfFile reads topf.yaml. config.Load reads it without resolving links, so a link that leaves the tree, loops or
// dangles is refused here.
func readTopfFile(fsys fs.FS) ([]byte, error) {
	if _, err := fs.Lstat(fsys, topfFile); err != nil {
		return nil, refuse(topfFile, ruleTopfUnparsable)
	}

	if _, err := config.EvalSymlinks(fsys, topfFile); err != nil {
		return nil, refuse(topfFile, rulePathEscapes)
	}

	raw, err := fs.ReadFile(fsys, topfFile)
	if err != nil {
		return nil, refuse(topfFile, ruleTopfUnparsable)
	}

	return raw, nil
}

// field returns the value of key of a decoded YAML mapping, nil when v is not one. YAML decodes a mapping with a key
// that is not a string to map[any]any.
func field(v any, key string) any {
	switch m := v.(type) {
	case map[string]any:
		return m[key]
	case map[any]any:
		return m[key]
	default:
		return nil
	}
}

// isSet reports whether a decoded value is set as Topf reads a string field: present, and neither null nor "".
func isSet(v any) bool {
	return v != nil && v != ""
}

// checkTreePath refuses a path of topf.yaml that leaves the tree, by name or through a link or a link loop. A path
// that does not exist is not refused here: an absent patch folder is empty and an absent bundle is its own rule.
func checkTreePath(fsys fs.FS, p string) error {
	cleaned, err := config.CleanRel(p)
	if err == nil {
		_, err = config.EvalSymlinks(fsys, cleaned)
	}

	if errors.Is(err, config.ErrEscapes) || errors.Is(err, config.ErrSymlinkLoop) {
		return refuse(topfFile, rulePathEscapes)
	}

	return nil
}

// hasIdentityReference reports whether host, ip, schematicId, talosVersion or kubernetesVersion, of the cluster or of a
// node (config.Node has no kubernetesVersion), starts like a reference. It reads the generic decode, so no typed field
// has failed first.
func hasIdentityReference(generic any) bool {
	isReference := func(v any) bool {
		s, ok := v.(string)

		return ok && config.HasReferencePrefix(s)
	}

	for _, key := range []string{"schematicId", "talosVersion", "kubernetesVersion"} {
		if isReference(field(generic, key)) {
			return true
		}
	}

	nodes, _ := field(generic, "nodes").([]any)

	return slices.ContainsFunc(nodes, func(node any) bool {
		return slices.ContainsFunc([]string{"host", "ip", "schematicId", "talosVersion"}, func(key string) bool {
			return isReference(field(node, key))
		})
	})
}

// checkHosts applies rule 5: every host is a DNS name, and no two are the same name.
func checkHosts(nodes []config.Node) error {
	for i := range nodes {
		if !isDNSName(nodes[i].Host) {
			return refuse(topfFile, ruleHostNotDNS)
		}
	}

	seen := make(map[string]bool, len(nodes))

	for i := range nodes {
		host := strings.ToLower(nodes[i].Host)
		if seen[host] {
			return refuse(topfFile, ruleHostDuplicate)
		}

		seen[host] = true
	}

	return nil
}

// isDNSName reports whether s is labels of lower-case letters, digits and "-", each 1 to 63 characters, joined by
// dots, 253 characters at most, with no trailing dot.
func isDNSName(s string) bool {
	if len(s) > 253 {
		return false
	}

	for label := range strings.SplitSeq(s, ".") {
		if len(label) < 1 || len(label) > 63 {
			return false
		}

		for i := range len(label) {
			if c := label[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}

	return true
}

// checkVersions applies rule 6.
func checkVersions(cfg *config.TopfConfig) error {
	if cfg.KubernetesVersion == "" {
		return refuse(topfFile, ruleKubernetesMissing)
	}

	if cfg.TalosVersion == "" {
		return refuse(topfFile, ruleTalosMissing)
	}

	talos := normaliseVersion(cfg.TalosVersion)
	if !talosInRange(talos) {
		return refuse(topfFile, ruleTalosRange)
	}

	for i := range cfg.Nodes {
		if v := cfg.Nodes[i].TalosVersion; v != "" && normaliseVersion(v) != talos {
			return refuse(topfFile, ruleTalosDiffers)
		}
	}

	return nil
}

// normaliseVersion adds a missing leading "v" to a version.
func normaliseVersion(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}

	return "v" + v
}

// talosInRange reports whether v is vMAJOR.MINOR.PATCH, digits only, from v1.12.11 up to v1.14.x. A pre-release or build
// suffix, a part with a leading zero or too many digits, and any other shape is out of range.
func talosInRange(v string) bool {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return false
	}

	var number [3]int

	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return false
		}

		number[i] = n
	}

	major, minor, patch := number[0], number[1], number[2]

	return major == 1 && minor <= 14 && (minor > 12 || minor == 12 && patch >= 11)
}

// checkPolicy applies rule 7: the default image factory, the metal platform, and no secure boot, for the cluster and
// for every node.
func checkPolicy(cfg *config.TopfConfig) error {
	if differs(cfg.Factory, defaultFactory) ||
		slices.ContainsFunc(cfg.Nodes, func(n config.Node) bool { return differs(n.Factory, defaultFactory) }) {
		return refuse(topfFile, ruleFactory)
	}

	if differs(cfg.Platform, defaultPlatform) ||
		slices.ContainsFunc(cfg.Nodes, func(n config.Node) bool { return differs(n.Platform, defaultPlatform) }) {
		return refuse(topfFile, rulePlatform)
	}

	if cfg.SecureBoot || slices.ContainsFunc(cfg.Nodes, func(n config.Node) bool { return n.SecureBoot }) {
		return refuse(topfFile, ruleSecureBoot)
	}

	return nil
}

// differs reports whether v is set to something other than def: "" is the default too.
func differs(v, def string) bool {
	return v != "" && v != def
}

// schematicValues returns the schematicId of the cluster and then of each node, as written.
func schematicValues(cfg *config.TopfConfig) []string {
	values := make([]string, 0, 1+len(cfg.Nodes))
	values = append(values, cfg.SchematicID)

	for i := range cfg.Nodes {
		values = append(values, cfg.Nodes[i].SchematicID)
	}

	return values
}

// checkSchematics applies rule 8: each schematicId is "", an id or an @ path, and an @ path stays inside the tree.
func checkSchematics(fsys fs.FS, cfg *config.TopfConfig) error {
	values := schematicValues(cfg)

	for _, v := range values {
		if p, isPath := strings.CutPrefix(v, "@"); !isID(v) && (!isPath || p == "") {
			return refuse(topfFile, ruleSchematicFormat)
		}
	}

	for _, v := range values {
		if p, isPath := strings.CutPrefix(v, "@"); isPath {
			if err := checkTreePath(fsys, p); err != nil {
				return err
			}
		}
	}

	return nil
}

// isID reports whether v is a schematic id: empty (the default schematic), or 64 lower-case hex digits.
func isID(v string) bool {
	if v == "" {
		return true
	}

	if len(v) != 64 {
		return false
	}

	for i := range len(v) {
		if c := v[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// schematicFiles returns the cleaned path of each @ schematic file the definition names, once each.
func schematicFiles(cfg *config.TopfConfig) []string {
	var files []string

	for _, v := range schematicValues(cfg) {
		if p, isPath := strings.CutPrefix(v, "@"); isPath {
			if cleaned, err := config.CleanRel(p); err == nil && !slices.Contains(files, cleaned) {
				files = append(files, cleaned)
			}
		}
	}

	return files
}

// checkData applies rule 9: the data of the cluster and of each node holds plain values only.
func checkData(cfg *config.TopfConfig) error {
	if !isPlain(cfg.Data) {
		return refuse(topfFile, ruleDataNotPlain)
	}

	for i := range cfg.Nodes {
		if !isPlain(cfg.Nodes[i].Data) {
			return refuse(topfFile, ruleDataNotPlain)
		}
	}

	return nil
}

// isPlain reports whether v is a string, number, bool, null, list or map, all the way down. YAML decodes a timestamp
// to a time.Time, whose time-zone methods a template could reach.
func isPlain(v any) bool {
	switch v := v.(type) {
	case nil, string, int, int64, uint64, float64, bool:
		return true
	case []any:
		return !slices.ContainsFunc(v, func(item any) bool { return !isPlain(item) })
	case map[string]any:
		for _, item := range v {
			if !isPlain(item) {
				return false
			}
		}

		return true
	default:
		return false
	}
}

// checkSecretsExist applies rule 10: the secrets bundle is a file of the tree. A link that dangles, or a path that is
// not a regular file, is as absent as a missing path.
func checkSecretsExist(fsys fs.FS, name string) error {
	info, err := fs.Stat(fsys, name)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(name, ruleSecretsMissing)
	case err != nil:
		return fmt.Errorf("render: stat %s: %w", name, err)
	case !info.Mode().IsRegular():
		return refuse(name, ruleSecretsMissing)
	default:
		return nil
	}
}

// checkSecretsParse applies rule 10's second half: every document of the secrets bundle is YAML, so a bundle that
// cannot decode is refused before any of its references is resolved. An empty file has no document and passes;
// whether the values make a valid bundle is for the render, which has them.
func checkSecretsParse(fsys fs.FS, name string) error {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("render: read %s: %w", name, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))

	for {
		var doc any

		switch err := dec.Decode(&doc); {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return refuse(name, ruleSecretsUnparsable)
		}
	}
}

// treeWalk lists the regular files of a tree, following links that stay inside it.
type treeWalk struct {
	fsys  fs.FS
	files []string
	// dirs holds the real directories met: false while one is being walked, true once done.
	dirs map[string]bool
}

// walkTree applies the link half of rule 11. It returns the real path of every regular file of the tree, sorted, and
// refuses a link that leaves the tree, dangles or loops, and a link to a directory that is being walked: a symlinked
// directory cycle. Each real directory is walked once, so a tree of many links is read in linear time.
func walkTree(fsys fs.FS) ([]string, error) {
	w := &treeWalk{fsys: fsys, dirs: map[string]bool{}}

	if err := w.walk("."); err != nil {
		return nil, err
	}

	slices.Sort(w.files)

	return slices.Compact(w.files), nil
}

// walk walks the real directory dir.
func (w *treeWalk) walk(dir string) error {
	w.dirs[dir] = false

	entries, err := fs.ReadDir(w.fsys, dir)
	if err != nil {
		return fmt.Errorf("render: read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		name := path.Join(dir, entry.Name())

		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			err = w.link(name)
		case entry.IsDir():
			err = w.enter(name, name)
		case entry.Type().IsRegular():
			w.files = append(w.files, name)
		}

		if err != nil {
			return err
		}
	}

	w.dirs[dir] = true

	return nil
}

// link handles a symlink met during the walk.
func (w *treeWalk) link(name string) error {
	target, err := config.EvalSymlinks(w.fsys, name)
	if err != nil {
		return refuse(name, rulePathEscapes)
	}

	info, err := fs.Stat(w.fsys, target)
	if err != nil {
		return refuse(name, rulePathEscapes)
	}

	switch {
	case info.IsDir():
		return w.enter(target, name)
	case info.Mode().IsRegular():
		w.files = append(w.files, target)
	}

	return nil
}

// enter walks the real directory dir, reached as via, unless it was walked already. A directory that is still being
// walked is a cycle of links.
func (w *treeWalk) enter(dir, via string) error {
	done, seen := w.dirs[dir]

	switch {
	case !seen:
		return w.walk(dir)
	case !done:
		return refuse(via, rulePathEscapes)
	default:
		return nil
	}
}

// checkSOPS applies the SOPS half of rule 11: no file of the tree, whatever its name, starts with a mapping that has a
// top-level "sops" mapping.
func checkSOPS(fsys fs.FS, files []string) error {
	for _, name := range files {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("render: read %s: %w", name, err)
		}

		// Only the first document counts, and a file that does not decode is not a SOPS file.
		var first any
		if yaml.Unmarshal(raw, &first) != nil {
			continue
		}

		if isMapping(field(first, "sops")) {
			return refuse(name, ruleSOPS)
		}
	}

	return nil
}

func isMapping(v any) bool {
	switch v.(type) {
	case map[string]any, map[any]any:
		return true
	default:
		return false
	}
}

// patchFiles returns the patch files Topf loads for the definition, in load order: the files of <patchesDir>/all, of
// <patchesDir>/<role> for each role present, and of <patchesDir>/node/<host> for each host.
func patchFiles(fsys fs.FS, def *definition) ([]string, error) {
	folders := []string{path.Join(def.patchesDir, "all")}

	for i := range def.cfg.Nodes {
		if folder := path.Join(def.patchesDir, string(def.cfg.Nodes[i].Role)); !slices.Contains(folders, folder) {
			folders = append(folders, folder)
		}
	}

	for i := range def.cfg.Nodes {
		folders = append(folders, path.Join(def.patchesDir, "node", def.cfg.Nodes[i].Host))
	}

	var (
		files []string
		seen  = map[string]bool{}
	)

	for _, folder := range folders {
		found, err := collectPatchFiles(fsys, folder)
		if err != nil {
			return nil, err
		}

		for _, name := range found {
			if !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
	}

	return files, nil
}

// collectPatchFiles returns the patch files below folder, selected as the vendored collectPatchFiles of package config
// does: a file matches by its own name, a link to a file by its target's, and a link to a directory is walked under the
// link's name. A folder that does not exist has none. It differs from the vendored walk in two ways: it has no loop check
// (the tree passed rule 11 already, so no link leaves the tree, dangles or loops), and only a missing folder is empty,
// where any other error, a missing entry below the folder included, is returned.
func collectPatchFiles(fsys fs.FS, folder string) ([]string, error) {
	if _, err := fs.Stat(fsys, folder); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	var files []string

	err := fs.WalkDir(fsys, folder, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.Type()&fs.ModeSymlink == 0 {
			if !entry.IsDir() && isPatchName(entry.Name()) {
				files = append(files, name)
			}

			return nil
		}

		target, err := config.EvalSymlinks(fsys, name)
		if err != nil {
			return err
		}

		info, err := fs.Stat(fsys, target)
		if err != nil {
			return err
		}

		if info.IsDir() {
			linked, err := collectPatchFiles(fsys, name)
			files = append(files, linked...)

			return err
		}

		if isPatchName(path.Base(target)) {
			files = append(files, name)
		}

		return nil
	})

	return files, err
}

// isPatchName reports whether a file name is one Topf loads: it matches the unanchored pattern `.*\.ya?ml(\.tpl)?`,
// that is, it holds ".yaml" or ".yml" anywhere.
func isPatchName(name string) bool {
	return strings.Contains(name, ".yaml") || strings.Contains(name, ".yml")
}

// splitPatches separates the patch files Topf substitutes references in from the templates it renders, which it does
// not substitute.
func splitPatches(patches []string) (plain, templates []string) {
	for _, name := range patches {
		if strings.HasSuffix(name, templateSuffix) {
			templates = append(templates, name)
		} else {
			plain = append(plain, name)
		}
	}

	return plain, templates
}

// readRegular reads name when it is a regular file. A file that does not exist, or is not a regular file, is not read:
// the render, which reads the same path, fails the tree.
func readRegular(fsys fs.FS, name string) (content []byte, ok bool, err error) {
	info, err := fs.Stat(fsys, name)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("render: stat %s: %w", name, err)
	case !info.Mode().IsRegular():
		return nil, false, nil
	}

	content, err = fs.ReadFile(fsys, name)
	if err != nil {
		return nil, false, fmt.Errorf("render: read %s: %w", name, err)
	}

	return content, true, nil
}

// scanResolved applies rule 12 to the files whose references are substituted: every string value and list item is a
// plain op reference, holds none, or is refused. It returns the references, sorted and de-duplicated.
func scanResolved(fsys fs.FS, names []string) ([]string, error) {
	refs := map[string]struct{}{}

	for _, name := range names {
		raw, ok, err := readRegular(fsys, name)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		for _, value := range stringValues(raw) {
			rule, isRef := classifyReference(value)
			if rule != "" {
				return nil, refuse(name, rule)
			}

			if isRef {
				refs[value] = struct{}{}
			}
		}
	}

	return slices.Sorted(maps.Keys(refs)), nil
}

// scanUnresolved applies rule 13 to the files whose references are not substituted: none holds a reference, and each
// template follows the template rules.
func scanUnresolved(fsys fs.FS, names []string) error {
	type text struct{ name, content string }

	var texts []text

	for _, name := range names {
		raw, ok, err := readRegular(fsys, name)
		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		if HasEmbeddedReference(string(raw)) {
			return refuse(name, ruleRefUnresolved)
		}

		texts = append(texts, text{name, string(raw)})
	}

	funcs := FuncMap()

	for _, t := range texts {
		if strings.HasSuffix(t.name, templateSuffix) {
			if rule := templateRule(t.name, t.content, funcs); rule != "" {
				return refuse(t.name, rule)
			}
		}
	}

	return nil
}
