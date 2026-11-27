// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Binding pins a consumer namespace and virtual node to one provider namespace
// incarnation. The node agent additionally enforces its physical node.
//
// A provider cluster of several nodes, each its own virtual node named after
// its hostname, binds them all in VirtualNodes instead: its one admission
// webhook sees every node's ShadowPods, and each node's agent serves its own
// (ForNode).
type Binding struct {
	Kubeconfig            string   `yaml:"kubeconfig"`
	VirtualNode           string   `yaml:"virtualNode,omitempty"`
	VirtualNodes          []string `yaml:"virtualNodes,omitempty"`
	OriginClusterID       string   `yaml:"originClusterID"`
	ProviderClusterID     string   `yaml:"providerClusterID"`
	Namespace             string   `yaml:"namespace"`
	ProviderNamespace     string   `yaml:"providerNamespace"`
	ProviderNamespaceUID  string   `yaml:"providerNamespaceUID"`
	NamespaceMapNamespace string   `yaml:"namespaceMapNamespace"`
	NamespaceMapName      string   `yaml:"namespaceMapName"`
	NamespaceMapUID       string   `yaml:"namespaceMapUID"`
}

// Validate rejects incomplete or ambiguous remote bindings.
func (c Binding) Validate() error {
	// A slice, not a map: with several fields invalid, every start reports the same one.
	for _, field := range []struct{ name, value string }{
		{"kubeconfig", c.Kubeconfig},
		{"virtualNode", c.VirtualNode + strings.Join(c.VirtualNodes, ",")},
		{"originClusterID", c.OriginClusterID},
		{"providerClusterID", c.ProviderClusterID},
		{"namespace", c.Namespace},
		{"providerNamespace", c.ProviderNamespace},
		{"providerNamespaceUID", c.ProviderNamespaceUID},
		{"namespaceMapNamespace", c.NamespaceMapNamespace},
		{"namespaceMapName", c.NamespaceMapName},
		{"namespaceMapUID", c.NamespaceMapUID},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("remoteControlPlane.%s must be nonempty without surrounding whitespace", field.name)
		}
	}
	if c.VirtualNode != "" && len(c.VirtualNodes) != 0 {
		return fmt.Errorf("remoteControlPlane needs exactly one of virtualNode and virtualNodes")
	}
	seen := map[string]bool{}
	for _, node := range c.VirtualNodes {
		if node == "" || strings.TrimSpace(node) != node || seen[node] {
			return fmt.Errorf("remoteControlPlane.virtualNodes must be distinct, nonempty and without surrounding whitespace")
		}
		seen[node] = true
	}
	if !filepath.IsAbs(c.Kubeconfig) {
		return fmt.Errorf("remoteControlPlane.kubeconfig must be an absolute path")
	}
	if c.OriginClusterID == c.ProviderClusterID {
		return fmt.Errorf("remoteControlPlane cluster IDs must differ")
	}
	return nil
}

// Binds reports whether the binding covers a virtual node.
func (c Binding) Binds(node string) bool {
	if c.VirtualNode != "" {
		return node == c.VirtualNode
	}
	return slices.Contains(c.VirtualNodes, node)
}

// ForNode narrows a binding of several virtual nodes to the one a node's agent
// serves: a cluster member's virtual node is named after its hostname.
func (c Binding) ForNode(node string) (Binding, error) {
	if len(c.VirtualNodes) == 0 {
		return c, nil
	}
	if !c.Binds(node) {
		return Binding{}, fmt.Errorf("node %q is not one of remoteControlPlane.virtualNodes", node)
	}
	c.VirtualNode, c.VirtualNodes = node, nil
	return c, nil
}
