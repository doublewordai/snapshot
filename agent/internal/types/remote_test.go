// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// An incomplete binding must not silently turn a provider into a same-cluster
// agent or authorize a namespace that happens to reuse a former name.
func TestRemoteControlPlaneRejectsIncompleteBinding(t *testing.T) {
	valid := RemoteControlPlaneSpec{Kubeconfig: "/config/kubeconfig", VirtualNode: "virtual", OriginClusterID: "origin", ProviderClusterID: "provider", Namespace: "source", ProviderNamespace: "target", ProviderNamespaceUID: "namespace-uid", NamespaceMapNamespace: "tenant", NamespaceMapName: "mapping", NamespaceMapUID: "mapping-uid"}
	require.NoError(t, valid.Validate())
	for i := 0; i < reflect.TypeFor[RemoteControlPlaneSpec]().NumField(); i++ {
		t.Run(reflect.TypeFor[RemoteControlPlaneSpec]().Field(i).Name, func(t *testing.T) {
			broken := valid
			reflect.ValueOf(&broken).Elem().Field(i).SetString("")
			require.Error(t, broken.Validate())
		})
	}
	valid.Kubeconfig = "relative"
	require.ErrorContains(t, valid.Validate(), "absolute")
}
