// hephaestus: new

package config

import "net/netip"

// TemplateData is what a patch template executes against. TemplateData has no methods and TemplateNode has Endpoint only
// (a template also reaches the methods of the netip.Addr in Node.IP), so a template reaches nothing of a PatchContext:
// not PatchesDir, the file system, the reference values or Load.
// Field names equal PatchContext's, so a Topf template renders the same text.
type TemplateData struct {
	ClusterName       string
	ClusterEndpoint   string
	KubernetesVersion string
	TalosVersion      string
	SchematicID       string
	Data              map[string]any
	Node              *TemplateNode
}

// TemplateNode is the node a template renders for. Field names equal Node's.
type TemplateNode struct {
	Host string
	// IP is a fresh copy per render, and nil when nodes[].ip is unset (a template prints "<nil>", as in Topf).
	IP *netip.Addr
	// Role is "control-plane" or "worker".
	Role string
	Data map[string]any

	TalosVersion string
	SchematicID  string
	Factory      string
	Platform     string
	SecureBoot   bool
	// RuntimeData is always zero: hephaestus renders offline, without a live node.
	RuntimeData RuntimeData
}

// Endpoint returns the IP address if set, otherwise the Host, as Node.Endpoint does.
func (n *TemplateNode) Endpoint() string {
	if n.IP != nil {
		return n.IP.String()
	}

	return n.Host
}

// TemplateData returns what the templates of p execute against.
func (p *PatchContext) TemplateData() TemplateData {
	node := &TemplateNode{
		Host:         p.Node.Host,
		Role:         string(p.Node.Role),
		Data:         p.Node.Data,
		TalosVersion: p.Node.TalosVersion,
		SchematicID:  p.Node.SchematicID,
		Factory:      p.Node.Factory,
		Platform:     p.Node.Platform,
		SecureBoot:   p.Node.SecureBoot,
	}

	if p.Node.IP != nil {
		node.IP = new(*p.Node.IP)
	}

	return TemplateData{
		ClusterName:       p.ClusterName,
		ClusterEndpoint:   p.ClusterEndpoint,
		KubernetesVersion: p.KubernetesVersion,
		TalosVersion:      p.TalosVersion,
		SchematicID:       p.SchematicID,
		Data:              p.Data,
		Node:              node,
	}
}
