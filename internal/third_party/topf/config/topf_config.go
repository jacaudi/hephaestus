// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, pkg/config/topf_config.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// TopfConfig is the main configuration structure for a Topf cluster
type TopfConfig struct {
	ClusterName       string   `yaml:"clusterName"`
	ClusterEndpoint   Endpoint `yaml:"clusterEndpoint"`
	KubernetesVersion string   `yaml:"kubernetesVersion"`
	TalosVersion      string   `yaml:"talosVersion,omitempty"`
	SchematicID       string   `yaml:"schematicId,omitempty"`

	// Factory is the Talos image factory address (default: factory.talos.dev)
	Factory string `yaml:"factory,omitempty"`
	// Platform is the Talos platform identifier (default: metal)
	Platform string `yaml:"platform,omitempty"`
	// SecureBoot enables the secure boot installer variant
	SecureBoot bool `yaml:"secureboot,omitempty"`

	// SecretsProvider can be optionally set to the path of a binary which is
	// responsible for storing and retrieving secrets.yaml for a cluster. If not
	// set, will use a local secrets.yaml (see SecretsPath) with optional SOPS encryption.
	SecretsProvider string `yaml:"secretsProvider,omitempty"`

	// NodesProvider can be optionally set to the path of a binary which will provide additional nodes
	NodesProvider string `yaml:"nodesProvider,omitempty"`

	// PatchesDir is the directory containing patches and node-specific configurations.
	// Defaults to the directory containing the config file.
	PatchesDir string `yaml:"patchesDir,omitempty"`

	// ConfigDir is deprecated: use PatchesDir instead.
	ConfigDir string `yaml:"configDir,omitempty"`

	// SecretsPath is the path to the secrets.yaml file.
	// Relative paths are resolved against the directory containing the config file.
	// Defaults to "secrets.yaml" next to the config file.
	SecretsPath string `yaml:"secretsPath,omitempty"`

	Nodes []Node `yaml:"nodes"`

	// Data can contain arbitrary data that can be used when templating patches
	Data map[string]any `yaml:"data"`
}

// topfFile is the name of the cluster definition at the root of a tree.
const topfFile = "topf.yaml"

// hephaestus: changed — LoadFromFile(path) (*TopfConfig, []string, error) becomes Load(fsys, values) (*TopfConfig, error):
// it reads topf.yaml at the root of fsys through ReadResolved (references replace the SOPS read), PatchesDir defaults to "."
// and SecretsPath to "secrets.yaml" and a set value must stay inside the tree (CleanRel), nodesProvider is refused, the SOPS
// mask list is not returned and GetSecretsProvider is removed (SecretsProvider and NodesProvider stay so the renderer can refuse them).

// Load loads the TopfConfig from topf.yaml at the root of fsys.
// PatchesDir defaults to the root of fsys and SecretsPath to "secrets.yaml" at the root.
// Both are relative to the root of fsys, as Topf resolves them against the directory of the config file.
func Load(fsys fs.FS, values map[string]string) (*TopfConfig, error) {
	content, err := ReadResolved(fsys, topfFile, values)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := &TopfConfig{}

	err = yaml.Unmarshal(content, config)
	if err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	if config.ConfigDir != "" {
		return nil, fmt.Errorf("deprecated field 'configDir' found in %s: use 'patchesDir' instead", topfFile)
	}

	if config.NodesProvider != "" {
		return nil, errors.New("topf: nodesProvider is not supported")
	}

	// Set default patchesDir if not specified
	if config.PatchesDir == "" {
		config.PatchesDir = "."
	} else if config.PatchesDir, err = CleanRel(config.PatchesDir); err != nil {
		return nil, fmt.Errorf("patchesDir: %w", err)
	}

	// Set default secretsPath if not specified (next to topf.yaml, not inside patchesDir)
	if config.SecretsPath == "" {
		config.SecretsPath = "secrets.yaml"
	} else if config.SecretsPath, err = CleanRel(config.SecretsPath); err != nil {
		return nil, fmt.Errorf("secretsPath: %w", err)
	}

	// Sort nodes by role and hostname, such that control plane nodes come first
	slices.SortStableFunc(config.Nodes, func(a, b Node) int {
		if a.Role == RoleControlPlane && b.Role == RoleWorker {
			return -1
		} else if a.Role == RoleWorker && b.Role == RoleControlPlane {
			return 1
		}

		return strings.Compare(a.Host, b.Host)
	})

	return config, nil
}
