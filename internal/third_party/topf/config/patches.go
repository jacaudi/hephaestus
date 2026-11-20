// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, pkg/config/patches.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"gopkg.in/yaml.v3"
)

// PatchContext are the things that can be templated in patches
//
// hephaestus: changed — DecryptCache is removed; FS, Values and Funcs are added. Templates execute against
// TemplateData(), not against the PatchContext, so none of these fields is reachable from a template.
type PatchContext struct {
	ClusterName       string
	ClusterEndpoint   string
	KubernetesVersion string
	TalosVersion      string
	SchematicID       string
	Data              map[string]any
	Node              *Node
	// PatchesDir is relative to the root of FS (see CleanRel).
	PatchesDir string
	// FS is the definition tree the patches are read from.
	FS fs.FS
	// Values holds the value of each secret reference (see SubstituteReferences).
	Values map[string]string
	// Funcs is the function map of the templates, for example sprig's.
	Funcs template.FuncMap
}

// Load loads all patches applicable for the node This includes general patches,
// role (worker/control-plane) specific patches and node specific patches in
// that order.
//
// hephaestus: changed — the SOPS mask list is not returned, and the folders are read from FS.
func (p *PatchContext) Load() ([]configpatcher.Patch, error) {
	var patches []configpatcher.Patch

	for _, folder := range []string{
		path.Join(p.PatchesDir, "all"),
		// patches relating to role of node, control-plane or worker
		path.Join(p.PatchesDir, string(p.Node.Role)),
		// patches relating to single specific node
		path.Join(p.PatchesDir, "node", p.Node.Host),
	} {
		folderPatches, err := p.loadFolder(folder)
		if err != nil {
			return nil, err
		}

		patches = append(patches, folderPatches...)
	}

	return patches, nil
}

// hephaestus: changed — the files come from collectPatchFiles and no SOPS mask list is returned.

func (p *PatchContext) loadFolder(folder string) ([]configpatcher.Patch, error) {
	filePaths, err := collectPatchFiles(p.FS, folder)
	if err != nil {
		return nil, err
	}

	var patches []configpatcher.Patch

	for _, filePath := range filePaths {
		templatedFileContent, err := p.loadFile(filePath)
		if err != nil {
			return nil, err
		}

		patchesInFile, err := parsePatches(templatedFileContent)
		if err != nil {
			return nil, fmt.Errorf("failed to parse patches in file %s: %w", filePath, err)
		}

		patches = append(patches, patchesInFile...)
	}

	return patches, nil
}

// patchWalker collects the patch files below a folder.
type patchWalker struct {
	fsys    fs.FS
	pattern *regexp.Regexp
	files   []string
}

// hephaestus: changed — collectPatchFiles is Topf's loadFolder walk over an fs.FS:
//   - a symlink is resolved with EvalSymlinks and fs.Stat instead of filepath.EvalSymlinks and os.Stat, and a target
//     that leaves the tree is ErrEscapes;
//   - a linked directory is walked under its link name, where Topf walks it under its real path, and a directory
//     already on the walk chain is ErrSymlinkLoop, where Topf recurses until the path is too long;
//   - only a folder missing as a plain path element is empty: Topf's os.IsNotExist on the whole walk also emptied a
//     folder that held a dangling symlink, silently dropping every patch in it, and here a symlink on the way whose
//     target does not exist is an error (EvalSymlinks marks it errLinkTarget).

// collectPatchFiles returns the patch files below folder, in walk order, the name of a link being the link's own path.
// A folder that does not exist has none.
func collectPatchFiles(fsys fs.FS, folder string) ([]string, error) {
	realFolder, err := EvalSymlinks(fsys, folder)
	if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errLinkTarget) {
		// A plain path element is missing: the folder does not exist. (A link whose target is missing is an error.)
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	// A file system without links is not checked by EvalSymlinks.
	if _, err := fs.Stat(fsys, realFolder); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	w := &patchWalker{fsys: fsys, pattern: regexp.MustCompile(`.*\.ya?ml(\.tpl)?`)}

	if err := w.walk(folder, realFolder, nil); err != nil {
		return nil, err
	}

	return w.files, nil
}

// walk collects the files below folder, whose real path is realFolder. chain holds the real paths of the
// directories being walked above it.
func (w *patchWalker) walk(folder, realFolder string, chain []string) error {
	if slices.Contains(chain, realFolder) {
		return fmt.Errorf("%w: %q", ErrSymlinkLoop, folder)
	}

	chain = append(chain[:len(chain):len(chain)], realFolder)

	return fs.WalkDir(w.fsys, folder, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// handle symlinks
		if d.Type()&fs.ModeSymlink != 0 {
			return w.link(name, chain)
		}

		if !d.IsDir() && w.pattern.MatchString(d.Name()) {
			w.files = append(w.files, name)
		}

		return nil
	})
}

// link handles a symlink met during a walk.
func (w *patchWalker) link(name string, chain []string) error {
	realPath, err := EvalSymlinks(w.fsys, name)
	if err != nil {
		return err
	}

	info, err := fs.Stat(w.fsys, realPath)
	if err != nil {
		return err
	}

	// If symlink points to a directory, walk it recursively
	if info.IsDir() {
		return w.walk(name, realPath, chain)
	}

	// If symlink points to a file, check the pattern against the target's name
	if w.pattern.MatchString(path.Base(realPath)) {
		w.files = append(w.files, name)
	}

	return nil
}

// RenderTemplate renders a Go template file with the given data.
// The template has access to funcs and uses "missingkey=error".
//
// hephaestus: changed — the file is read from fsys, and the function map is a parameter where Topf fixed it to
// the sprig function library (http://masterminds.github.io/sprig/).
func RenderTemplate(fsys fs.FS, name string, funcs template.FuncMap, data any) ([]byte, error) {
	content, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(path.Base(name)).Funcs(funcs).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to execute template %s: %w", name, err)
	}

	return buf.Bytes(), nil
}

// hephaestus: changed — a template is rendered with TemplateData() and any other file is read through
// ReadResolved, where Topf rendered with the PatchContext and read through the SOPS cache. A template's output
// has no references substituted, as in Topf.
func (p *PatchContext) loadFile(filename string) ([]byte, error) {
	if strings.HasSuffix(filename, ".tpl") {
		return RenderTemplate(p.FS, filename, p.Funcs, p.TemplateData())
	}

	return ReadResolved(p.FS, filename, p.Values)
}

func parsePatches(data []byte) (patches []configpatcher.Patch, err error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	documentIndex := 0

	for {
		var doc any

		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("invalid patch at document index %d: %w", documentIndex, err)
		}

		if doc == nil {
			continue // skip empty documents
		}

		data, ok := doc.(map[string]any)
		if !ok {
			if _, isArray := doc.([]any); isArray {
				return nil, fmt.Errorf("document at index %d looks like a JSON patch (array of operations), which is not supported - use strategic merge patches instead", documentIndex)
			}

			return nil, fmt.Errorf("invalid patch at document index %d: expected a YAML mapping, got %T", documentIndex, doc)
		}

		if len(data) == 0 {
			continue // skip empty documents
		}

		patchBytes, err := yaml.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal YAML at document index %d: %w", documentIndex, err)
		}

		patch, err := configpatcher.LoadPatch(patchBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to load patch at document index %d: %w", documentIndex, err)
		}

		patches = append(patches, patch)
		documentIndex++
	}

	return patches, nil
}
