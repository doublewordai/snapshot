// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Binding pins a consumer namespace and virtual node to one provider namespace
// incarnation. The node agent additionally enforces its physical node.
type Binding struct {
	Kubeconfig            string `yaml:"kubeconfig"`
	VirtualNode           string `yaml:"virtualNode"`
	OriginClusterID       string `yaml:"originClusterID"`
	ProviderClusterID     string `yaml:"providerClusterID"`
	Namespace             string `yaml:"namespace"`
	ProviderNamespace     string `yaml:"providerNamespace"`
	ProviderNamespaceUID  string `yaml:"providerNamespaceUID"`
	NamespaceMapNamespace string `yaml:"namespaceMapNamespace"`
	NamespaceMapName      string `yaml:"namespaceMapName"`
	NamespaceMapUID       string `yaml:"namespaceMapUID"`
}

// Validate rejects incomplete or ambiguous remote bindings.
func (c Binding) Validate() error {
	for name, value := range map[string]string{
		"kubeconfig":            c.Kubeconfig,
		"virtualNode":           c.VirtualNode,
		"originClusterID":       c.OriginClusterID,
		"providerClusterID":     c.ProviderClusterID,
		"namespace":             c.Namespace,
		"providerNamespace":     c.ProviderNamespace,
		"providerNamespaceUID":  c.ProviderNamespaceUID,
		"namespaceMapNamespace": c.NamespaceMapNamespace,
		"namespaceMapName":      c.NamespaceMapName,
		"namespaceMapUID":       c.NamespaceMapUID,
	} {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("remoteControlPlane.%s must be nonempty without surrounding whitespace", name)
		}
	}
	if !filepath.IsAbs(c.Kubeconfig) {
		return fmt.Errorf("remoteControlPlane.kubeconfig must be an absolute path")
	}
	if c.OriginClusterID == c.ProviderClusterID {
		return fmt.Errorf("remoteControlPlane cluster IDs must differ")
	}
	return nil
}
