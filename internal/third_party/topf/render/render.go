// hephaestus: new

package render

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"text/template"

	"github.com/siderolabs/talos/pkg/machinery/config/encoder"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

// Input is the cluster definition to render.
type Input struct {
	// FS is the cluster subpath; nothing outside it is read.
	FS fs.FS
	// Values holds the value of each secret reference: the whole reference string ("ref+op://…") to its value.
	Values map[string]string
	// Funcs are the template functions; Topf's sprig.TxtFuncMap() is not used.
	Funcs template.FuncMap
}

// Node is the rendered configuration of one node of the cluster.
type Node struct {
	// Host is the node's host name, Role "control-plane" or "worker", and IP "" when nodes[].ip is unset.
	Host, Role, IP string
	// Config is ConfigProvider().EncodeBytes(encoder.WithComments(encoder.CommentsDisabled), encoder.WithOmitEmpty(true)),
	// the bytes `topf render` writes to <host>.yaml.
	Config []byte
	// SchematicID is the resolved schematic id.
	SchematicID string
	// SchematicBody is schematic.Canonical of an "@file" schematic; nil for a literal id.
	SchematicBody []byte
	// TalosVersion is "v" and the node's Talos version.
	TalosVersion string
	// KubernetesVersion is "v" and the node's Kubernetes version.
	KubernetesVersion string
}

// Render loads the cluster definition, reads the secrets bundle once and renders every node, in config order
// (control planes first, then by host name), one after the other. The first node error is returned wrapped with the host name.
func Render(ctx context.Context, in Input) ([]Node, error) {
	cfg, err := config.Load(in.FS, in.Values)
	if err != nil {
		return nil, err
	}

	if cfg.SecretsProvider != "" {
		return nil, errors.New("topf: secretsProvider is not supported")
	}

	secretsBundle, err := loadSecretsBundle(in, cfg)
	if err != nil {
		return nil, err
	}

	nodes := make([]Node, 0, len(cfg.Nodes))

	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]

		state, err := generateNodeConfig(ctx, cfg, node, in, secretsBundle)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", node.Host, err)
		}

		configBytes, err := state.ConfigProvider().EncodeBytes(
			encoder.WithComments(encoder.CommentsDisabled),
			encoder.WithOmitEmpty(true),
		)
		if err != nil {
			return nil, fmt.Errorf("node %s: failed to encode config: %w", node.Host, err)
		}

		out := Node{
			Host:              node.Host,
			Role:              string(node.Role),
			Config:            configBytes,
			SchematicID:       state.resolvedSchematic,
			SchematicBody:     state.schematicBody,
			TalosVersion:      "v" + state.TalosVersion(),
			KubernetesVersion: "v" + state.KubernetesVersion(),
		}

		if node.IP != nil {
			out.IP = node.IP.String()
		}

		nodes = append(nodes, out)
	}

	return nodes, nil
}

// Schematic loads the cluster definition and resolves the schematic of host: its id, and the canonical body of an
// "@file" schematic (nil for a literal id). No patch and no secrets bundle is read.
func Schematic(_ context.Context, in Input, host string) (id string, body []byte, err error) {
	cfg, err := config.Load(in.FS, in.Values)
	if err != nil {
		return "", nil, err
	}

	for i := range cfg.Nodes {
		if cfg.Nodes[i].Host == host {
			return resolve(in, newPatchContext(cfg, &cfg.Nodes[i], in))
		}
	}

	return "", nil, fmt.Errorf("topf: no node %q in cluster %s", host, cfg.ClusterName)
}
