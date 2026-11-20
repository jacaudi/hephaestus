// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, internal/topf/secrets.go and pkg/providers/secrets.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package render

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"go.yaml.in/yaml/v4"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// ErrSecretsBundleMissing reports a secrets bundle that does not exist or is empty.
var ErrSecretsBundleMissing = errors.New("topf: secrets bundle missing")

// ErrSecretsBundleUnparsable reports a secrets bundle that does not decode or does not validate.
var ErrSecretsBundleUnparsable = errors.New("topf: secrets bundle unparsable")

// hephaestus: changed — loadSecretsBundle merges Topf's Secrets() and providers.LoadSecretsBundle: it reads the
// bundle at cfg.SecretsPath from the Input's file system and replaces its secret references by their values
// (config.SubstituteReferences; a reference with no value is a *config.MissingReferenceError), and never generates
// one: a missing or empty file is ErrSecretsBundleMissing, a file that does not decode or validate (a null document
// included) is ErrSecretsBundleUnparsable. The provider, the interactive prompt, generateAndStoreSecrets, Put, the
// cache and the secrets masking (collectSecrets) are removed.

// loadSecretsBundle reads and validates the secrets bundle of the cluster.
func loadSecretsBundle(in Input, cfg *config.TopfConfig) (*secrets.Bundle, error) {
	// A link out of the tree is not followed; a link that dangles is a missing file, as for any other path.
	if _, err := config.EvalSymlinks(in.FS, cfg.SecretsPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("secrets bundle %s: %w", cfg.SecretsPath, err)
	}

	// The read and the substitution are config.ReadResolved split in two, so a file whose YAML does not decode is
	// ErrSecretsBundleUnparsable, as one that does not decode as a bundle.
	raw, err := fs.ReadFile(in.FS, cfg.SecretsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrSecretsBundleMissing, cfg.SecretsPath)
	}

	if err != nil {
		return nil, fmt.Errorf("secrets bundle %s: %w", cfg.SecretsPath, err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrSecretsBundleMissing, cfg.SecretsPath)
	}

	content, err := config.SubstituteReferences(raw, in.Values)
	if _, missing := errors.AsType[*config.MissingReferenceError](err); missing {
		return nil, fmt.Errorf("secrets bundle %s: %w", cfg.SecretsPath, err)
	}

	if err != nil {
		return nil, fmt.Errorf("%w: resolve references in %s: %w", ErrSecretsBundleUnparsable, cfg.SecretsPath, err)
	}

	// use a clock skewed slightly to the past to ensure generated certs are valid even
	// if there is some time drift between the talos node and the machine running topf
	//
	// hephaestus: changed — the bundle is decoded into a value, not through a **Bundle: a null document (null, ~, ---)
	// would set the pointer to nil and Validate would dereference it.
	bundle := secrets.Bundle{Clock: secrets.NewFixedClock(time.Now().Add(-time.Second * 1))}

	if err := yaml.Unmarshal(content, &bundle); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal secrets: %w", ErrSecretsBundleUnparsable, err)
	}

	// a nil contract validates the bundle against the current Talos version
	if err := bundle.Validate(nil); err != nil {
		return nil, fmt.Errorf("%w: invalid secrets bundle: %w", ErrSecretsBundleUnparsable, err)
	}

	return &bundle, nil
}
