// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	OriginClusterLabel   = "offloading.liqo.io/origin"
	ProviderClusterLabel = "offloading.liqo.io/destination"
	VirtualNodeLabel     = "offloading.liqo.io/nodename"
)

// RESTConfig loads data-only consumer credentials and requires verified HTTPS.
func RESTConfig(path string) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("load metadata kubeconfig: %w", err)
	}
	// Agent credentials are data, not executable plugins. TokenFile supports
	// rotation without granting kubeconfig authors code execution in the agent.
	for _, auth := range raw.AuthInfos {
		if auth == nil {
			return nil, fmt.Errorf("metadata kubeconfig contains an empty user")
		}
		if auth.Exec != nil || auth.AuthProvider != nil {
			return nil, fmt.Errorf("metadata kubeconfig must not use exec or auth-provider plugins")
		}
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("configure metadata client: %w", err)
	}
	if !strings.HasPrefix(cfg.Host, "https://") || cfg.Insecure {
		return nil, fmt.Errorf("metadata API requires HTTPS with certificate verification")
	}
	// Bounds each one-shot request. Callers that watch must copy the config and
	// clear it, or the watch ends when the timeout does.
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

// ValidateMapping checks the pinned NamespaceMap and provider namespace
// incarnations. Callers must supply uncached readers for authorization.
func (c *Binding) ValidateMapping(ctx context.Context, metadata, local client.Reader) error {
	nm := &unstructured.Unstructured{}
	nm.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceMap"})
	key := client.ObjectKey{Namespace: c.NamespaceMapNamespace, Name: c.NamespaceMapName}
	if err := metadata.Get(ctx, key, nm); err != nil {
		return fmt.Errorf("get authoritative NamespaceMap: %w", err)
	}
	if string(nm.GetUID()) != c.NamespaceMapUID || nm.GetDeletionTimestamp() != nil ||
		nm.GetLabels()["liqo.io/remote-cluster-id"] != c.ProviderClusterID {
		return fmt.Errorf("NamespaceMap incarnation or provider does not match agent binding")
	}
	desired, _, err := unstructured.NestedString(nm.Object, "spec", "desiredMapping", c.Namespace)
	if err != nil {
		return err
	}
	actual, _, err := unstructured.NestedString(nm.Object, "status", "currentMapping", c.Namespace, "remoteNamespace")
	if err != nil {
		return err
	}
	phase, _, err := unstructured.NestedString(nm.Object, "status", "currentMapping", c.Namespace, "phase")
	if err != nil {
		return err
	}
	if desired != c.ProviderNamespace || actual != desired || phase != "Accepted" {
		return fmt.Errorf("NamespaceMap has no accepted configured namespace mapping")
	}
	ns := &corev1.Namespace{}
	if err := local.Get(ctx, client.ObjectKey{Name: c.ProviderNamespace}, ns); err != nil {
		return fmt.Errorf("get provider namespace: %w", err)
	}
	if string(ns.UID) != c.ProviderNamespaceUID || ns.DeletionTimestamp != nil {
		return fmt.Errorf("provider namespace incarnation changed or is deleting")
	}
	return nil
}

// OriginPod reads the authoritative current incarnation, including the stamp
// maintained by Snapshot. Callers must also verify their reflected object's UID.
func (c *Binding) OriginPod(ctx context.Context, metadata client.Reader, name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := metadata.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, pod); err != nil {
		return nil, fmt.Errorf("get origin pod: %w", err)
	}
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Spec.NodeName != c.VirtualNode {
		return nil, fmt.Errorf("origin pod is not a live incarnation on the configured virtual node")
	}
	if pod.Annotations[podcontract.OriginPodUIDAnnotation] != string(pod.UID) {
		return nil, fmt.Errorf("origin pod is awaiting its Snapshot UID stamp")
	}
	return pod, nil
}

// ValidateLabels checks the administrator-configured cluster and virtual node.
func (c *Binding) ValidateLabels(labels map[string]string) error {
	if labels[OriginClusterLabel] != c.OriginClusterID || labels[ProviderClusterLabel] != c.ProviderClusterID ||
		labels[VirtualNodeLabel] != c.VirtualNode {
		return fmt.Errorf("reflection cluster or virtual-node labels do not match the binding")
	}
	return nil
}
