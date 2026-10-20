// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/agent/pkg/provenance"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	liqoOrigin               = provenance.OriginClusterLabel
	liqoDestination          = provenance.ProviderClusterLabel
	liqoNode                 = provenance.VirtualNodeLabel
	originPodUIDAnnotation   = podcontract.OriginPodUIDAnnotation
	remoteCaptureStarted     = "nvidia.com/remote-capture-started"
	remoteCapturePodUID      = "nvidia.com/remote-capture-pod-uid"
	remoteCaptureContainerID = "nvidia.com/remote-capture-container-id"
	remoteCaptureAttempt     = "nvidia.com/remote-capture-attempt"
)

// errRuntimeIdentityUnavailable marks a runtime lookup that failed, as opposed
// to one that answered with another Pod. Only the latter is a terminal verdict.
var errRuntimeIdentityUnavailable = errors.New("runtime pod identity unavailable")

// snapshotRequestAnnotations returns a Pod's Snapshot request annotations:
// restore-from, the container map and the compatibility bypass among them.
func snapshotRequestAnnotations(p *corev1.Pod) map[string]string {
	result := map[string]string{}
	for k, v := range p.Annotations {
		if strings.HasPrefix(k, "nvidia.com/") {
			result[k] = v
		}
	}
	return result
}

// remoteControlPlane never translates Snapshot UIDs. Only Pods have a second
// incarnation; that translation is verified against a live Liqo ShadowPod.
type remoteControlPlane struct {
	config   *types.RemoteControlPlaneSpec
	local    client.Client
	metadata client.Client
	nodeName string
}

// Remote operations require a fresh authoritative work order. Native capture
// retains the upstream informer-cache path.
func (w *NodeController) captureContent(ctx context.Context, name string) (*snapshotv1alpha1.PodSnapshotContent, error) {
	if w.remote == nil {
		return w.cachedContent(name)
	}
	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get remote PodSnapshotContent %q: %w", name, err)
	}
	return content, nil
}

// Remote cleanup owns a node-local content directory, not an artifact-addressed
// store. Never route a bound publication through that filesystem lifecycle.
func validateRemoteContentStorage(content *snapshotv1alpha1.PodSnapshotContent) error {
	if content.Spec.Storage != nil {
		return fmt.Errorf("remote control plane does not support artifact-addressed content")
	}
	return nil
}

func remoteRESTConfig(path string) (*rest.Config, error) {
	return provenance.RESTConfig(path)
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
	return r.config.ValidateMapping(ctx, r.metadata, r.local)
}

// resolve returns uncached API observations of both Pod incarnations. UID
// preconditions at Pod mutations retain the provider's actual identity.
// Callers read Snapshot requests from expectedLocal, a queued or CRI copy, so
// its request annotations must equal the fresh, authoritative ones.
func (r *remoteControlPlane) resolve(ctx context.Context, name string, expectedLocal *corev1.Pod) (*corev1.Pod, *corev1.Pod, error) {
	if err := r.validateMapping(ctx); err != nil {
		return nil, nil, err
	}
	c := r.config
	origin, err := c.OriginPod(ctx, r.metadata, name)
	if err != nil {
		return nil, nil, err
	}
	local := &corev1.Pod{}
	if err := r.local.Get(ctx, client.ObjectKey{Namespace: c.ProviderNamespace, Name: name}, local); err != nil {
		return nil, nil, fmt.Errorf("get provider pod: %w", err)
	}
	if local.UID == "" || local.DeletionTimestamp != nil || local.Spec.NodeName != r.nodeName {
		return nil, nil, fmt.Errorf("provider pod is not a live incarnation on this physical node")
	}
	if expectedLocal != nil && (expectedLocal.Namespace != local.Namespace || expectedLocal.UID != local.UID || expectedLocal.Annotations[originPodUIDAnnotation] != string(origin.UID)) {
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
		if labels[liqoOrigin] != c.OriginClusterID || labels[liqoDestination] != c.ProviderClusterID || labels[liqoNode] != c.VirtualNode || obj.GetAnnotations()[originPodUIDAnnotation] != string(origin.UID) {
			return nil, nil, fmt.Errorf("Liqo reflection provenance does not match origin pod incarnation")
		}
	}
	// Snapshot requests and compatibility bypasses are authoritative on Curie.
	// Reject added as well as changed provider-side request annotations.
	if !reflect.DeepEqual(snapshotRequestAnnotations(origin), snapshotRequestAnnotations(local)) {
		return nil, nil, fmt.Errorf("provider Snapshot annotations differ from authoritative pod")
	}
	if expectedLocal != nil && !reflect.DeepEqual(snapshotRequestAnnotations(expectedLocal), snapshotRequestAnnotations(local)) {
		return nil, nil, fmt.Errorf("provider pod Snapshot annotations changed since it was observed")
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
		return fmt.Errorf("%w: %w", errRuntimeIdentityUnavailable, err)
	}
	if uid != string(pod.UID) {
		return fmt.Errorf("runtime container belongs to another provider pod incarnation")
	}
	return nil
}

// reserveRemoteCapture durably claims one attempt before touching CUDA. A lost
// agent cannot prove how far the attempt reached. Reconciliation first recovers
// a committed artifact; without one it must never replay an uncertain dump.
// The one reservation that may be reused is this process's own, unexecuted one:
// a failed check after the claim, or a claim whose reply was lost, leaves it.
func (w *NodeController) reserveRemoteCapture(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod, containerID string) error {
	if content.Annotations[remoteCaptureStarted] != "" {
		if !w.resumableRemoteCapture(content) {
			return fmt.Errorf("previous capture attempt for provider pod %q and container %q has no committed artifact; refusing replay",
				content.Annotations[remoteCapturePodUID], content.Annotations[remoteCaptureContainerID])
		}
		if content.Annotations[remoteCapturePodUID] == string(pod.UID) && content.Annotations[remoteCaptureContainerID] == containerID {
			return nil
		}
	}
	before := content.DeepCopy()
	if content.Annotations == nil {
		content.Annotations = map[string]string{}
	}
	content.Annotations[remoteCaptureStarted] = "true"
	content.Annotations[remoteCapturePodUID] = string(pod.UID)
	content.Annotations[remoteCaptureContainerID] = containerID
	content.Annotations[remoteCaptureAttempt] = w.captureAttemptID
	if err := w.client.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persist remote capture attempt: %w", err)
	}
	return nil
}

// resumableRemoteCapture reports whether this process wrote the content's
// reservation and has not handed the content to the checkpoint since, which
// proves no dump started. An agent restart loses that proof by design.
func (w *NodeController) resumableRemoteCapture(content *snapshotv1alpha1.PodSnapshotContent) bool {
	if w.captureAttemptID == "" || content.Annotations[remoteCaptureAttempt] != w.captureAttemptID {
		return false
	}
	_, executed := w.executedCaptures.Load(content.UID)
	return !executed
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
