// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, internal/schematic/resolver.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package render

import (
	"fmt"
	"io/fs"
	"strings"

	"go.jacaudi.dev/hephaestus/internal/third_party/imagefactory/schematic"
	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// hephaestus: changed — Resolver is gone: resolve is a function of the Input, and the factory client, submitSchematic,
// the HTTP client and the user agent are removed (the id is always computed locally, nothing is submitted). The path
// is relative to the root of the tree, where Topf joined it to the directory of the config file, and it must stay
// inside the tree. The file is read through the Input's file system, and resolve also returns the canonical schematic
// body, so the caller can send it to the image factory itself.

// resolve resolves the schematic ID of patchCtx. If the ID starts with @, it is treated as a path to a schematic
// file (relative to the root of the tree). Files ending in .tpl are rendered through Go templates against the
// patch context's TemplateData(). The schematic ID is computed locally from the canonical YAML hash, and the canonical body
// is returned with it. Non-@-prefixed IDs are returned unchanged, with a nil body.
func resolve(in Input, patchCtx *config.PatchContext) (id string, body []byte, err error) {
	ref, ok := strings.CutPrefix(patchCtx.SchematicID, "@")
	if !ok {
		return patchCtx.SchematicID, nil, nil
	}

	path, err := config.CleanRel(ref)
	if err != nil {
		return "", nil, err
	}

	if _, err := config.EvalSymlinks(in.FS, path); err != nil {
		return "", nil, fmt.Errorf("failed to read schematic file %s: %w", path, err)
	}

	content, err := readFile(in, path, patchCtx)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read schematic file %s: %w", path, err)
	}

	body, err = schematic.Canonical(content)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse schematic file %s: %w", path, err)
	}

	id, err = schematic.ID(content)
	if err != nil {
		return "", nil, fmt.Errorf("failed to compute schematic ID: %w", err)
	}

	return id, body, nil
}

// hephaestus: changed — a template renders against TemplateData() with the Input's function map, any other file is read from the Input's file system.

func readFile(in Input, path string, patchCtx *config.PatchContext) ([]byte, error) {
	if !strings.HasSuffix(path, ".tpl") {
		return fs.ReadFile(in.FS, path)
	}

	return config.RenderTemplate(in.FS, path, in.Funcs, patchCtx.TemplateData())
}
