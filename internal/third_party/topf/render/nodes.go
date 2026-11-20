// Copyright 2026 PostFinance AG
// SPDX-License-Identifier: MIT
// hephaestus: vendored from github.com/postfinance/topf 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16, internal/topf/nodes.go. Changes are marked "hephaestus: changed" and listed in ../VENDORED.md.

package render

import (
	"cmp"
	"context"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/bundle"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	runtimetypes "github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"go.yaml.in/yaml/v4"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

const defaultInstallDisk string = "/dev/sda" // as defined in talos/gen/config.go

func installerImagePatch(image string, versionContract *talosconfig.VersionContract) (configpatcher.Patch, error) {
	var patchBytes []byte

	var err error

	if versionContract.UnattendedInstallConfig() {
		// we reuse the upstream default: https://github.com/siderolabs/talos/blob/v1.14.0/pkg/machinery/config/generate/init.go#L299-L314
		unattended := runtimetypes.NewUnattendedInstallConfigV1Alpha1()
		unattended.Installer.Image = image
		unattended.ProvisioningSpec.Wipe = new(false)

		expr, exprErr := cel.ParseBooleanExpression(fmt.Sprintf("disk.dev_path == %q", defaultInstallDisk), celenv.DiskLocator())
		if exprErr != nil {
			return nil, fmt.Errorf("failed to build install disk selector: %w", exprErr)
		}

		unattended.ProvisioningSpec.DiskSelector.Match = expr
		patchBytes, err = yaml.Marshal(unattended)
	} else {
		patchBytes, err = yaml.Marshal(map[string]any{
			"machine": map[string]any{
				"install": map[string]any{
					"image": image,
				},
			},
		})
	}

	if err != nil {
		return nil, err
	}

	return configpatcher.LoadPatch(patchBytes)
}

// hephaestus: changed — generateNodeConfig is a function of the cluster config, the node, the Input and the secrets
// bundle (Render reads the bundle once), and returns the rendered nodeState. The patch context comes from
// newPatchContext, which Schematic shares. The mask calls (AddSecretsToMask) are removed, and so are collectNodeInfo,
// filterNodes and FilteredNodes: nothing is queried live and every node is rendered. A canceled context is
// returned before a node is rendered.

// newPatchContext returns the context node's schematic and patches are resolved against, with the schematic id as
// configured (it may still be an @file reference).
func newPatchContext(cfg *config.TopfConfig, node *config.Node, in Input) *config.PatchContext {
	return &config.PatchContext{
		ClusterName:       cfg.ClusterName,
		ClusterEndpoint:   cfg.ClusterEndpoint.String(),
		KubernetesVersion: cfg.KubernetesVersion,
		TalosVersion:      cfg.TalosVersion,
		SchematicID:       cmp.Or(node.SchematicID, cfg.SchematicID, DefaultSchematic),
		Node:              node,
		Data:              cfg.Data,
		PatchesDir:        cfg.PatchesDir,
		FS:                in.FS,
		Values:            in.Values,
		Funcs:             in.Funcs,
	}
}

// generateNodeConfig renders the configuration bundle of node.
func generateNodeConfig(ctx context.Context, cfg *config.TopfConfig, node *config.Node, in Input, secretsBundle *secrets.Bundle) (*nodeState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	state := &nodeState{cfg: cfg, node: node}

	patchContext := newPatchContext(cfg, node, in)

	resolvedSchematic, schematicBody, err := resolve(in, patchContext)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve schematic ID: %w", err)
	}

	state.resolvedSchematic = resolvedSchematic
	state.schematicBody = schematicBody
	patchContext.SchematicID = resolvedSchematic

	patches, err := patchContext.Load()
	if err != nil {
		return nil, fmt.Errorf("couldn't load patches: %w", err)
	}

	versionContract, err := talosconfig.ParseContractFromVersion(state.TalosVersion())
	if err != nil {
		return nil, err
	}

	installPatch, err := installerImagePatch(state.InstallerImage(), versionContract)
	if err != nil {
		return nil, fmt.Errorf("failed to build installer image patch: %w", err)
	}

	patches = append([]configpatcher.Patch{installPatch}, patches...)

	configBundleOpts := []bundle.Option{
		bundle.WithInputOptions(
			&bundle.InputOptions{
				ClusterName: cfg.ClusterName,
				Endpoint:    cfg.ClusterEndpoint.String(),
				KubeVersion: state.KubernetesVersion(),
				GenOptions: []generate.Option{
					generate.WithSecretsBundle(secretsBundle),
					generate.WithVersionContract(versionContract),
				},
			},
		),
		bundle.WithVerbose(false), // prevent printing "generating PKI and tokens"
	}

	switch node.Role {
	case config.RoleControlPlane:
		configBundleOpts = append(configBundleOpts, bundle.WithPatchControlPlane(patches))
	case config.RoleWorker:
		configBundleOpts = append(configBundleOpts, bundle.WithPatchWorker(patches))
	}

	configBundle, err := bundle.NewBundle(configBundleOpts...)
	if err != nil {
		return nil, err
	}

	state.configBundle = configBundle

	return state, nil
}
