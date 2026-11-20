// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, internal/topf/node.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package render

import (
	"cmp"
	"fmt"
	"strings"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/bundle"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/gendata"
	"github.com/siderolabs/talos/pkg/machinery/version"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// hephaestus: changed — DefaultSchematic is a constant, the Talos main module's pkg/images.DefaultInstallerImageSchematic at
// v1.14.4 (pkg/images/images.go:24), so the Talos main module is never imported. DefaultPlatform is a constant too
// (it names a constant). DefaultFactory stays a variable read from machinery's embedded data: it cannot be a constant.
const (
	// DefaultSchematic is the schematic used by Talos when no extensions are configured
	DefaultSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	// DefaultPlatform is the default Talos platform identifier
	DefaultPlatform = constants.PlatformMetal
)

// DefaultFactory is the default Talos image factory address
var DefaultFactory = gendata.ImageFactory //nolint:gochecknoglobals // exported read-only default sourced from talos, treated as constant

// hephaestus: changed — Node is renamed nodeState (the exported Node of this package is the render result) and keeps
// the cluster config, the node, the resolved schematic and its body, and the config bundle. MachineStatus, the running version and
// schematic, Error, MarshalYAML, Attrs, InstallerImageRef and the client are removed: hephaestus renders offline.

// nodeState is a configured node of the cluster with what rendering its configuration produced.
type nodeState struct {
	cfg  *config.TopfConfig
	node *config.Node

	resolvedSchematic string
	schematicBody     []byte
	configBundle      *bundle.Bundle
}

// hephaestus: changed — there is no running version to prefer: the version comes from topf.yaml, else the bundled Talos version.

// TalosVersion returns the Talos version to use for config generation, without the "v".
// Fallback chain: topf.yaml -> bundled Talos version.
func (n *nodeState) TalosVersion() string {
	return strings.TrimPrefix(cmp.Or(n.cfg.TalosVersion, version.Tag), "v")
}

// KubernetesVersion returns the Kubernetes version to use for config generation.
// Fallback chain: topf.yaml -> bundled default (constants.DefaultKubernetesVersion).
func (n *nodeState) KubernetesVersion() string {
	return strings.TrimPrefix(cmp.Or(n.cfg.KubernetesVersion, constants.DefaultKubernetesVersion), "v")
}

// InstallerImage returns the fully resolved installer image for this node.
// The schematic ID is the resolved value (after @-prefixed references have been expanded).
// Factory, platform, talosVersion, and secureboot resolve per node -> cluster config -> default.
func (n *nodeState) InstallerImage() string {
	factory := cmp.Or(n.node.Factory, n.cfg.Factory, DefaultFactory)
	platform := cmp.Or(n.node.Platform, n.cfg.Platform, DefaultPlatform)
	schematic := cmp.Or(n.resolvedSchematic, DefaultSchematic)
	talosVersion := strings.TrimPrefix(cmp.Or(n.node.TalosVersion, n.cfg.TalosVersion, version.Tag), "v")

	installer := platform + "-installer"
	if n.node.SecureBoot || n.cfg.SecureBoot {
		installer += "-secureboot"
	}

	return fmt.Sprintf("%s/%s/%s:v%s", factory, installer, schematic, talosVersion)
}

// ConfigProvider returns the config bundle associated with the node's role
func (n *nodeState) ConfigProvider() talosconfig.Provider {
	var provider talosconfig.Provider

	if n.node.Role == config.RoleControlPlane {
		provider = n.configBundle.ControlPlaneCfg
	} else {
		provider = n.configBundle.WorkerCfg
	}

	return provider
}
