package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	topfrender "go.jacaudi.dev/hephaestus/internal/third_party/topf/render"
)

// The render rules, as a *RefusalError carries them.
const (
	ruleSecretsUnparsable      = "secrets bundle unparsable"
	ruleSecretReachesSchematic = "secret reaches schematic"
)

// HostRender is the render of one host: its machine config, the same config redacted for display, their hashes and
// its schematic.
type HostRender struct {
	// Host, Role and IP are the host's entry of the definition.
	Host, Role, IP string
	// Config is the machine config with every secret in clear. It is never shown or logged.
	Config []byte
	// Redacted is Config with every secret replaced by Marker, the only form fit for display.
	Redacted []byte
	// ConfigHash is hex(sha256(Config)); DisplayHash is hex(sha256(Redacted)).
	ConfigHash, DisplayHash string
	// SchematicID is the Talos Image Factory schematic id of the host.
	SchematicID string
	// SchematicBody is the canonical schematic body when the schematicId is an @ file, and nil otherwise.
	SchematicBody []byte
	// TalosVersion is the host's Talos version with a leading "v".
	TalosVersion string
}

// Render renders every host of the cluster with the resolved values of its references (reference to value). A secrets
// bundle that does not decode or validate once the values are in is a *RefusalError; a schematic template whose body
// depends on a secret value is a *RefusalError; a display render that still holds a value is a *RedactionError. Any
// other error wraps the text of the render, which can quote a value: callers store it sealed and never log it.
func (c *Cluster) Render(ctx context.Context, values map[string]string) ([]HostRender, error) {
	nodes, err := topfrender.Render(ctx, topfrender.Input{FS: c.FS, Values: values, Funcs: FuncMap()})
	if errors.Is(err, topfrender.ErrSecretsBundleUnparsable) {
		return nil, refuse(c.secretsPath, ruleSecretsUnparsable)
	}

	if err != nil {
		return nil, fmt.Errorf("render cluster %s: %w", c.Name, err)
	}

	dummies := dummyValues(values)
	hosts := make([]HostRender, 0, len(nodes))

	for i := range nodes {
		node := &nodes[i]

		if err := c.checkSchematic(ctx, node, dummies); err != nil {
			return nil, err
		}

		redacted, err := Redact(node.Config, values)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", node.Host, err)
		}

		hosts = append(hosts, HostRender{
			Host:          node.Host,
			Role:          node.Role,
			IP:            node.IP,
			Config:        node.Config,
			Redacted:      redacted,
			ConfigHash:    sha256Hex(node.Config),
			DisplayHash:   sha256Hex(redacted),
			SchematicID:   node.SchematicID,
			SchematicBody: node.SchematicBody,
			TalosVersion:  node.TalosVersion,
		})
	}

	return hosts, nil
}

// checkSchematic applies the schematic secret check to a host whose schematicId is an @ template: the template is
// rendered again with dummies in place of the values, and a body that differs from the real one, or a render that
// fails, means a secret value reaches the schematic, which goes to the public image factory. The text of the render
// error is dropped: it can quote a value.
func (c *Cluster) checkSchematic(ctx context.Context, node *topfrender.Node, dummies map[string]string) error {
	file, ok := strings.CutPrefix(c.schematics[node.Host], "@")
	if !ok || !strings.HasSuffix(file, templateSuffix) {
		return nil
	}

	// Load cleaned the path with config.CleanRel and refused one that escapes the tree, so path.Clean gives the same name.
	file = path.Clean(file)

	_, body, err := topfrender.Schematic(ctx, topfrender.Input{FS: c.FS, Values: dummies, Funcs: FuncMap()}, node.Host)
	if err != nil || !bytes.Equal(body, node.SchematicBody) {
		return refuse(file, ruleSecretReachesSchematic)
	}

	return nil
}

// dummyValues maps every reference to a text of the length of its value that is not the value: "x" repeated, or "y"
// repeated for a value that is all "x".
func dummyValues(values map[string]string) map[string]string {
	dummies := make(map[string]string, len(values))

	for ref, value := range values {
		dummy := strings.Repeat("x", len(value))
		if dummy == value {
			dummy = strings.Repeat("y", len(value))
		}

		dummies[ref] = dummy
	}

	return dummies
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
