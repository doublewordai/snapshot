// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"reflect"
	"testing"
)

// With several fields missing, every start must report the same one, so an
// operator fixing the binding is not chased between messages.
func TestBindingValidateReportsTheFirstInvalidFieldDeterministically(t *testing.T) {
	for range 50 {
		err := Binding{}.Validate()
		if err == nil || err.Error() != "remoteControlPlane.kubeconfig must be nonempty without surrounding whitespace" {
			t.Fatalf("Validate() = %v, want the kubeconfig error first", err)
		}
	}
}

// A node's agent serves only its own virtual node of a cluster's binding.
func TestForNodeNarrowsAClusterBindingToItsMember(t *testing.T) {
	cluster := Binding{VirtualNodes: []string{"zarbon-01", "zarbon-05"}}
	own, err := cluster.ForNode("zarbon-05")
	if err != nil || own.VirtualNode != "zarbon-05" || own.VirtualNodes != nil {
		t.Fatalf("ForNode(zarbon-05) = %+v, %v", own, err)
	}
	if _, err := cluster.ForNode("zarbon-02"); err == nil {
		t.Fatal("ForNode accepted a node outside the binding")
	}
	single := Binding{VirtualNode: "beerus"}
	if got, err := single.ForNode("ip-172-31-43-244"); err != nil || !reflect.DeepEqual(got, single) {
		t.Fatalf("ForNode changed a one-node binding: %+v, %v", got, err)
	}
	both := Binding{Kubeconfig: "/k", VirtualNode: "a", VirtualNodes: []string{"b"}, OriginClusterID: "o",
		ProviderClusterID: "p", Namespace: "n", ProviderNamespace: "n", ProviderNamespaceUID: "u",
		NamespaceMapNamespace: "m", NamespaceMapName: "m", NamespaceMapUID: "u"}
	if err := both.Validate(); err == nil {
		t.Fatal("Validate accepted both virtualNode and virtualNodes")
	}
}
