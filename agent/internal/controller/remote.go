// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	liqoOrigin               = "offloading.liqo.io/origin"
	liqoDestination          = "offloading.liqo.io/destination"
	liqoNode                 = "offloading.liqo.io/nodename"
	liqoOriginUID            = "offloading.liqo.io/origin-pod-uid"
	remoteCaptureStarted     = "nvidia.com/remote-capture-started"
	remoteCapturePodUID      = "nvidia.com/remote-capture-pod-uid"
	remoteCaptureContainerID = "nvidia.com/remote-capture-container-id"
)

// remoteControlPlane never translates Snapshot UIDs. Only Pods have a second
// incarnation; that translation is verified against a live Liqo ShadowPod.
type remoteControlPlane struct {
	config   *types.RemoteControlPlaneSpec
	local    client.Client
	metadata client.Client
	nodeName string
}

func remoteRESTConfig(path string) (*rest.Config, error) {
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
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

func (w *NodeController) metadataNodeName() string {
	if w.remote != nil {
		return w.remote.config.VirtualNode
	}
	return w.config.NodeName
}

func (w *NodeController) podClient() client.Client {
	if w.localClient != nil {
		return w.localClient
	}
	return w.client
}

// validateMapping reads both APIs on every operation. A stale mapping, a
// recreated namespace or an unavailable API never authorizes runtime access.
func (r *remoteControlPlane) validateMapping(ctx context.Context) error {
	c := r.config
	nm := &unstructured.Unstructured{}
	nm.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "NamespaceMap"})
	if err := r.metadata.Get(ctx, client.ObjectKey{Namespace: c.NamespaceMapNamespace, Name: c.NamespaceMapName}, nm); err != nil {
		return fmt.Errorf("get authoritative NamespaceMap: %w", err)
	}
	if string(nm.GetUID()) != c.NamespaceMapUID || nm.GetDeletionTimestamp() != nil || nm.GetLabels()["liqo.io/remote-cluster-id"] != c.ProviderClusterID {
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
	if err := r.local.Get(ctx, client.ObjectKey{Name: c.ProviderNamespace}, ns); err != nil {
		return fmt.Errorf("get provider namespace: %w", err)
	}
	if string(ns.UID) != c.ProviderNamespaceUID || ns.DeletionTimestamp != nil {
		return fmt.Errorf("provider namespace incarnation changed or is deleting")
	}
	return nil
}

// resolve returns uncached API observations of both Pod incarnations. UID
// preconditions at Pod mutations retain the provider's actual identity.
func (r *remoteControlPlane) resolve(ctx context.Context, name string, expectedLocal *corev1.Pod) (*corev1.Pod, *corev1.Pod, error) {
	if err := r.validateMapping(ctx); err != nil {
		return nil, nil, err
	}
	c := r.config
	origin := &corev1.Pod{}
	if err := r.metadata.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, origin); err != nil {
		return nil, nil, fmt.Errorf("get origin pod: %w", err)
	}
	if origin.UID == "" || origin.DeletionTimestamp != nil || origin.Spec.NodeName != c.VirtualNode {
		return nil, nil, fmt.Errorf("origin pod is not a live incarnation on the configured virtual node")
	}
	local := &corev1.Pod{}
	if err := r.local.Get(ctx, client.ObjectKey{Namespace: c.ProviderNamespace, Name: name}, local); err != nil {
		return nil, nil, fmt.Errorf("get provider pod: %w", err)
	}
	if local.UID == "" || local.DeletionTimestamp != nil || local.Spec.NodeName != r.nodeName {
		return nil, nil, fmt.Errorf("provider pod is not a live incarnation on this physical node")
	}
	if expectedLocal != nil && (expectedLocal.Namespace != local.Namespace || expectedLocal.UID != local.UID || expectedLocal.Annotations[liqoOriginUID] != string(origin.UID)) {
		return nil, nil, fmt.Errorf("provider pod incarnation changed")
	}
	shadow := &unstructured.Unstructured{}
	shadow.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "ShadowPod"})
	if err := r.local.Get(ctx, client.ObjectKeyFromObject(local), shadow); err != nil {
		return nil, nil, fmt.Errorf("get provider ShadowPod: %w", err)
	}
	owner := metav1.GetControllerOf(local)
	if owner == nil || owner.APIVersion != "offloading.liqo.io/v1beta1" || owner.Kind != "ShadowPod" || owner.Name != shadow.GetName() || owner.UID == "" || owner.UID != shadow.GetUID() || shadow.GetDeletionTimestamp() != nil {
		return nil, nil, fmt.Errorf("provider pod is not controlled by the current ShadowPod")
	}
	for _, obj := range []metav1.Object{local, shadow} {
		labels := obj.GetLabels()
		if labels[liqoOrigin] != c.OriginClusterID || labels[liqoDestination] != c.ProviderClusterID || labels[liqoNode] != c.VirtualNode || obj.GetAnnotations()[liqoOriginUID] != string(origin.UID) {
			return nil, nil, fmt.Errorf("Liqo reflection provenance does not match origin pod incarnation")
		}
	}
	// Snapshot requests and compatibility bypasses are authoritative on Curie.
	// Reject added as well as changed provider-side request annotations.
	requestAnnotations := func(p *corev1.Pod) map[string]string {
		result := map[string]string{}
		for k, v := range p.Annotations {
			if strings.HasPrefix(k, "nvidia.com/") {
				result[k] = v
			}
		}
		return result
	}
	if !reflect.DeepEqual(requestAnnotations(origin), requestAnnotations(local)) {
		return nil, nil, fmt.Errorf("provider Snapshot annotations differ from authoritative pod")
	}
	return origin, local, nil
}

func (w *NodeController) validateRemoteRestore(ctx context.Context, pod *corev1.Pod) error {
	if w.remote == nil {
		return nil
	}
	_, _, err := w.remote.resolve(ctx, pod.Name, pod)
	return err
}

func (w *NodeController) validateRuntimePodUID(ctx context.Context, containerID string, pod *corev1.Pod) error {
	uid, err := w.runtime.ResolveContainerPodUID(ctx, containerID)
	if err != nil {
		return fmt.Errorf("resolve runtime pod UID: %w", err)
	}
	if uid != string(pod.UID) {
		return fmt.Errorf("runtime container belongs to another provider pod incarnation")
	}
	return nil
}

// reserveRemoteCapture durably claims one attempt before touching CUDA. A lost
// agent cannot prove how far the attempt reached. Reconciliation first recovers
// a committed artifact; without one it must never replay an uncertain dump.
func (w *NodeController) reserveRemoteCapture(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod, containerID string) error {
	if content.Annotations[remoteCaptureStarted] != "" {
		return fmt.Errorf("previous capture attempt for provider pod %q and container %q has no committed artifact; refusing replay",
			content.Annotations[remoteCapturePodUID], content.Annotations[remoteCaptureContainerID])
	}
	before := content.DeepCopy()
	if content.Annotations == nil {
		content.Annotations = map[string]string{}
	}
	content.Annotations[remoteCaptureStarted] = "true"
	content.Annotations[remoteCapturePodUID] = string(pod.UID)
	content.Annotations[remoteCaptureContainerID] = containerID
	if err := w.client.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persist remote capture attempt: %w", err)
	}
	return nil
}

func (w *NodeController) validateRemoteArtifact(ctx context.Context, pod *corev1.Pod, artifact *restoreArtifact) error {
	snapshot, err := w.getPodSnapshotFromPod(ctx, pod)
	if err != nil {
		return err
	}
	if err := validatePodSnapshotForRestore(snapshot); err != nil {
		return err
	}
	content, err := w.getPodSnapshotContentFromSnapshot(ctx, pod, snapshot)
	if err != nil {
		return err
	}
	if err := validatePodSnapshotContentForRestore(content); err != nil {
		return err
	}
	if string(content.UID) != artifact.ContentUID || content.Spec.Source.NodeName != w.metadataNodeName() {
		return fmt.Errorf("restore content identity or node changed before execution")
	}
	if content.Annotations[snapshotv1alpha1.RemoteArtifactCleanupAnnotation] != snapshotv1alpha1.RemoteArtifactCleanupAgent {
		return fmt.Errorf("remote artifact cleanup delegation is missing")
	}
	return nil
}

func (w *NodeController) metadataNamespace(pod *corev1.Pod) (string, error) {
	if w.remote == nil {
		return pod.Namespace, nil
	}
	if pod.Namespace != w.remote.config.ProviderNamespace {
		return "", fmt.Errorf("restore pod is outside the configured provider namespace")
	}
	return w.remote.config.Namespace, nil
}
