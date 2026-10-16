// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	agenttypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

type remoteFixture struct {
	controller      *NodeController
	origin, local   *corev1.Pod
	mapping, shadow *unstructured.Unstructured
	namespace       *corev1.Namespace
	content         *snapshotv1alpha1.PodSnapshotContent
	capture         *fakeCheckpointer
}

func makeRemoteRestoreFixture(t *testing.T) (*remoteFixture, *restorePlan) {
	t.Helper()
	f := makeRemoteFixture(t)
	origin := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	origin.Name = f.origin.Name
	origin.UID = f.origin.UID
	origin.Spec.NodeName = f.origin.Spec.NodeName
	local := origin.DeepCopy()
	local.Namespace = f.local.Namespace
	local.UID = f.local.UID
	local.Spec.NodeName = f.local.Spec.NodeName
	local.Labels = f.local.Labels
	local.OwnerReferences = f.local.OwnerReferences
	local.Annotations[liqoOriginUID] = string(origin.UID)
	f.origin = origin
	f.local = local
	f.shadow.SetAnnotations(local.Annotations)
	snapshot, content := readySnapshotObjects()
	content.Spec.Source.NodeName = f.controller.metadataNodeName()
	content.Annotations = map[string]string{snapshotv1alpha1.RemoteArtifactCleanupAnnotation: snapshotv1alpha1.RemoteArtifactCleanupAgent}
	f.content = content
	f.controller.client = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(origin, snapshot, content, f.mapping).WithStatusSubresource(&snapshotv1alpha1.PodSnapshotContent{}).Build()
	f.controller.localClient = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(local, f.shadow, f.namespace).Build()
	f.controller.remote.metadata = f.controller.client
	f.controller.remote.local = f.controller.localClient
	f.controller.clientset = k8sfake.NewClientset(local)
	f.controller.compareFn = compat.Compare
	path, err := nsmount.ResolveArtifactPath(f.controller.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, agenttypes.WriteManifest(path, &agenttypes.CheckpointManifest{Artifact: agenttypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))
	plan, err := f.controller.preflightRestore(context.Background(), local)
	require.NoError(t, err)
	return f, plan
}

// The restore executor must receive the provider Pod namespace and canonical
// consumer content UID, and must never run with stale preflight observations.
func TestRemoteRestoreRevalidatesBeforeExecution(t *testing.T) {
	for _, name := range []string{"success", "content replaced", "content bound after preflight", "provider replaced", "runtime pod replaced", "control plane unavailable", "completed before agent restart"} {
		t.Run(name, func(t *testing.T) {
			f, plan := makeRemoteRestoreFixture(t)
			ctx := context.Background()
			calls := 0
			sentinels := 0
			f.controller.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
				calls++
				assert.Equal(t, f.local.Namespace, req.PodNamespace)
				assert.Equal(t, string(f.content.UID), req.ContentUID)
				return executor.RestoreResult{PlaceholderHostPID: 7, RestoredPID: 42}, nil
			}
			f.controller.writeControlSentinelFn = func(int, string, []byte) error { sentinels++; return nil }
			switch name {
			case "content replaced":
				current := f.content.DeepCopy()
				current.UID = "new-content"
				snapshot, _ := readySnapshotObjects()
				f.controller.client = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.origin, f.mapping, snapshot, current).Build()
				f.controller.remote.metadata = f.controller.client
			case "content bound after preflight":
				f.content.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: "store-v1-" + strings.Repeat("a", 64)}
				require.NoError(t, f.controller.client.Update(ctx, f.content))
			case "provider replaced":
				f.local.UID = "stale-provider"
			case "runtime pod replaced":
				f.controller.runtime = &fakeRuntime{podUID: "other-pod"}
			case "control plane unavailable":
				f.controller.remote.metadata = interceptor.NewClient(f.controller.client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("metadata unavailable")
				}})
			case "completed before agent restart":
				f.local.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonInProgress}}
				f.controller.controlSentinelExistsFn = func(int, string) (bool, error) { return true, nil }
			}
			err := f.controller.runRestore(ctx, f.local, plan, "main", testContainerID, time.Time{}, name == "completed before agent restart")
			if name == "success" {
				require.NoError(t, err)
				assert.Equal(t, 1, calls)
				assert.Equal(t, 1, sentinels)
			} else if name == "completed before agent restart" {
				require.NoError(t, err)
				assert.Zero(t, calls)
				assert.Zero(t, sentinels)
			} else {
				require.Error(t, err)
				assert.Zero(t, calls)
				assert.Zero(t, sentinels)
			}
		})
	}
}

func makeRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	config := &agenttypes.RemoteControlPlaneSpec{
		Kubeconfig: "/etc/remote/kubeconfig", VirtualNode: "virtual-a", OriginClusterID: "curie", ProviderClusterID: "provider",
		Namespace: "inference", ProviderNamespace: "offloaded", ProviderNamespaceUID: "namespace-uid",
		NamespaceMapNamespace: "liqo-tenant", NamespaceMapName: "mapping", NamespaceMapUID: "mapping-uid",
	}
	origin := makeSourcePod()
	origin.Spec.NodeName = config.VirtualNode
	local := origin.DeepCopy()
	local.Namespace = config.ProviderNamespace
	local.UID = "provider-pod-uid"
	local.Spec.NodeName = "node-a"
	local.Labels = map[string]string{liqoOrigin: config.OriginClusterID, liqoDestination: config.ProviderClusterID, liqoNode: config.VirtualNode}
	local.Annotations = map[string]string{liqoOriginUID: string(origin.UID)}
	local.OwnerReferences = []metav1.OwnerReference{{APIVersion: "offloading.liqo.io/v1beta1", Kind: "ShadowPod", Name: local.Name, UID: "shadow-uid", Controller: ptr.To(true)}}
	shadow := &unstructured.Unstructured{}
	shadow.SetGroupVersionKind(schema.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "ShadowPod"})
	shadow.SetName(local.Name)
	shadow.SetNamespace(local.Namespace)
	shadow.SetUID("shadow-uid")
	shadow.SetLabels(local.Labels)
	shadow.SetAnnotations(local.Annotations)
	mapping := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "offloading.liqo.io/v1beta1", "kind": "NamespaceMap",
		"metadata": map[string]any{"name": config.NamespaceMapName, "namespace": config.NamespaceMapNamespace, "uid": config.NamespaceMapUID, "labels": map[string]any{"liqo.io/remote-cluster-id": config.ProviderClusterID}},
		"spec":     map[string]any{"desiredMapping": map[string]any{config.Namespace: config.ProviderNamespace}},
		"status":   map[string]any{"currentMapping": map[string]any{config.Namespace: map[string]any{"remoteNamespace": config.ProviderNamespace, "phase": "Accepted"}}},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: local.Namespace, UID: types.UID(config.ProviderNamespaceUID)}}
	content := makeWorkOrder("content", config.VirtualNode, "capture")
	content.Annotations = map[string]string{snapshotv1alpha1.RemoteArtifactCleanupAnnotation: snapshotv1alpha1.RemoteArtifactCleanupAgent}
	capture := &fakeCheckpointer{}
	w := makeNodeController(t, capture, origin, content, mapping)
	w.config.RemoteControlPlane = config
	w.localClient = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(local, shadow, namespace).Build()
	w.remote = &remoteControlPlane{config: config, local: w.localClient, metadata: w.client, nodeName: w.config.NodeName}
	w.runtime = &fakeRuntime{resolveContainerPID: 7, podUID: string(local.UID)}
	return &remoteFixture{w, origin, local, mapping, shadow, namespace, content, capture}
}

// Snapshot metadata must keep its Curie identity while only the validated real
// provider Pod can reach CRIU. No provider Snapshot CRDs are needed for capture.
func TestRemoteCaptureUsesProviderRuntimeAndAuthoritativeContent(t *testing.T) {
	f := makeRemoteFixture(t)
	require.NoError(t, f.controller.reconcileCapture(context.Background(), f.content.Name))
	require.True(t, f.capture.wasCalled())
	params := f.capture.lastParams()
	assert.Equal(t, f.local.UID, params.Pod.UID)
	assert.Equal(t, f.local.Namespace, params.Pod.Namespace)
	assert.Equal(t, string(f.content.UID), params.ContentUID)
	assert.Contains(t, params.HostPath, string(f.content.UID))
	saved := getContent(t, f.controller, f.content.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotContentSucceeded(saved))
	assert.Equal(t, "true", saved.Annotations[remoteCaptureStarted])
	assert.Equal(t, string(f.local.UID), saved.Annotations[remoteCapturePodUID])
	assert.Equal(t, params.ContainerID, saved.Annotations[remoteCaptureContainerID])
	origin := &corev1.Pod{}
	require.NoError(t, f.controller.client.Get(context.Background(), client.ObjectKeyFromObject(f.origin), origin))
	assert.NotContains(t, origin.Labels, snapshotv1alpha1.CaptureEligibleLabel)
}

func TestRemoteCaptureRejectsUnboundRuntime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*remoteFixture)
	}{
		{"legacy reflection", func(f *remoteFixture) { delete(f.local.Annotations, liqoOriginUID) }},
		{"reused origin name", func(f *remoteFixture) { f.origin.UID = "new-origin" }},
		{"stale ShadowPod owner", func(f *remoteFixture) { f.local.OwnerReferences[0].UID = "stale" }},
		{"spoofed reflection", func(f *remoteFixture) { f.local.Labels[liqoOrigin] = "other" }},
		{"another physical node", func(f *remoteFixture) { f.local.Spec.NodeName = "node-b" }},
		{"another virtual node", func(f *remoteFixture) { f.origin.Spec.NodeName = "virtual-b" }},
		{"recreated namespace", func(f *remoteFixture) { f.namespace.UID = "new-namespace" }},
		{"recreated mapping", func(f *remoteFixture) { f.mapping.SetUID("new-mapping") }},
		{"unaccepted mapping", func(f *remoteFixture) {
			require.NoError(t, unstructured.SetNestedField(f.mapping.Object, "Terminating", "status", "currentMapping", "inference", "phase"))
		}},
		{"wrong mapped namespace", func(f *remoteFixture) {
			require.NoError(t, unstructured.SetNestedField(f.mapping.Object, "elsewhere", "spec", "desiredMapping", "inference"))
		}},
		{"changed restore policy", func(f *remoteFixture) { f.local.Annotations[podcontract.RestoreFromAnnotation] = "injected" }},
		{"missing cleanup delegation", func(f *remoteFixture) { f.content.Annotations = nil }},
		{"artifact-addressed content", func(f *remoteFixture) {
			f.content.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: "store-v1-" + strings.Repeat("a", 64)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := makeRemoteFixture(t)
			tc.change(f)
			f.controller.client = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.origin, f.content, f.mapping).WithStatusSubresource(&snapshotv1alpha1.PodSnapshotContent{}).Build()
			f.controller.localClient = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.local, f.shadow, f.namespace).Build()
			f.controller.remote.local = f.controller.localClient
			f.controller.remote.metadata = f.controller.client
			_ = f.controller.reconcileCapture(context.Background(), f.content.Name)
			assert.False(t, f.capture.wasCalled())
		})
	}
}

func TestRemoteKubeconfigRejectsInsecureOrExecutableAuth(t *testing.T) {
	for _, tc := range []struct {
		name, server, tls, auth string
		valid                   bool
	}{
		{"verified TLS", "https://metadata.example", "", "token: test-token", true},
		{"plain HTTP", "http://metadata.example", "", "token: test-token", false},
		{"unverified TLS", "https://metadata.example", "insecure-skip-tls-verify: true", "token: test-token", false},
		{"executable auth", "https://metadata.example", "", "exec: {apiVersion: client.authentication.k8s.io/v1, command: do-not-execute, interactiveMode: Never}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := "apiVersion: v1\nkind: Config\ncurrent-context: metadata\nclusters:\n- name: metadata\n  cluster:\n    server: " + tc.server + "\n    " + tc.tls + "\ncontexts:\n- name: metadata\n  context: {cluster: metadata, user: agent}\nusers:\n- name: agent\n  user:\n    " + tc.auth + "\n"
			path := filepath.Join(t.TempDir(), "kubeconfig")
			require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
			_, err := remoteRESTConfig(path)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRemoteResolverRechecksProviderUIDAndRuntimeUID(t *testing.T) {
	f := makeRemoteFixture(t)
	stale := f.local.DeepCopy()
	stale.UID = "previous-incarnation"
	_, _, err := f.controller.remote.resolve(context.Background(), stale.Name, stale)
	require.ErrorContains(t, err, "incarnation changed")
	f.controller.runtime = &fakeRuntime{podUID: "another-runtime-pod"}
	require.ErrorContains(t, f.controller.validateRuntimePodUID(context.Background(), "container", f.local), "another provider pod")
}

func TestRemoteRestoreReadsSnapshotInOriginNamespace(t *testing.T) {
	f := makeRemoteFixture(t)
	name := "snapshot"
	f.origin.Annotations = map[string]string{podcontract.RestoreFromAnnotation: name}
	f.local.Annotations[podcontract.RestoreFromAnnotation] = name
	require.NoError(t, f.controller.client.Update(context.Background(), f.origin))
	require.NoError(t, f.controller.localClient.Update(context.Background(), f.local))
	snap := &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.origin.Namespace, UID: "snapshot-uid"}, Spec: snapshotv1alpha1.PodSnapshotSpec{Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{Containers: []string{"main"}}}}}
	require.NoError(t, f.controller.client.Create(context.Background(), snap))
	destinations, err := f.controller.RestoreDestinations(context.Background(), f.local)
	require.NoError(t, err)
	assert.Equal(t, []string{"main"}, destinations)
	// Even an explicit container map must not bypass reflection authorization.
	f.local.UID = "recreated"
	f.local.Annotations[podcontract.RestoreContainerMapAnnotation] = "main=main"
	_, err = f.controller.RestoreDestinations(context.Background(), f.local)
	require.Error(t, err)
}

func TestRemoteControlPlaneOutageNeverStartsCapture(t *testing.T) {
	f := makeRemoteFixture(t)
	down := interceptor.NewClient(f.controller.client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("API unavailable")
	}})
	f.controller.client = down
	f.controller.remote.metadata = down
	require.Error(t, f.controller.reconcileCapture(context.Background(), f.content.Name))
	assert.False(t, f.capture.wasCalled())
}

func TestRemoteCleanupRequiresBothAPIsAndNoRestoreUsers(t *testing.T) {
	for _, tc := range []string{"explicit deletion", "origin restore user", "provider restore user", "metadata unavailable", "unsafe artifact", "artifact-addressed content"} {
		t.Run(tc, func(t *testing.T) {
			f := makeRemoteFixture(t)
			ctx := context.Background()
			require.NoError(t, f.controller.client.Delete(ctx, f.content))
			f.content.DeletionTimestamp = ptr.To(metav1.Now())
			f.content.Finalizers = []string{snapshotv1alpha1.PodSnapshotContentArtifactCleanupFinalizer}
			if tc == "artifact-addressed content" {
				f.content.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: "store-v1-" + strings.Repeat("a", 64)}
			}
			f.controller.client = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.origin, f.mapping, f.content).Build()
			f.controller.remote.metadata = f.controller.client
			root, err := artifact.ResolveContentRoot(f.controller.config.Storage.BasePath, string(f.content.UID))
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(root), 0o750))
			if tc == "unsafe artifact" {
				require.NoError(t, os.Symlink(t.TempDir(), root))
			} else {
				require.NoError(t, os.MkdirAll(root, 0o750))
			}
			if tc == "origin restore user" {
				f.origin.Annotations = map[string]string{podcontract.RestoreFromAnnotation: f.content.Spec.PodSnapshotRef.Name}
				require.NoError(t, f.controller.client.Update(ctx, f.origin))
			}
			if tc == "provider restore user" {
				f.local.Annotations[podcontract.RestoreFromAnnotation] = f.content.Spec.PodSnapshotRef.Name
				require.NoError(t, f.controller.localClient.Update(ctx, f.local))
			}
			if tc == "metadata unavailable" {
				f.controller.remote.metadata = interceptor.NewClient(f.controller.client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("API unavailable")
				}})
			}
			err = f.controller.reconcileCapture(ctx, f.content.Name)
			if tc == "explicit deletion" {
				require.NoError(t, err)
				_, err = os.Lstat(root)
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.Error(t, err)
				_, err = os.Lstat(root)
				require.NoError(t, err)
			}
		})
	}
}

// An agent restart must recover a committed artifact or fail closed. It cannot
// infer that an earlier dump is safe to replay from current Pod readiness.
func TestRemoteCaptureDoesNotReplayUncertainAttempt(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "committed"}[committed], func(t *testing.T) {
			f := makeRemoteFixture(t)
			f.content.Annotations[remoteCaptureStarted] = "true"
			f.content.Annotations[remoteCapturePodUID] = "previous-provider-incarnation"
			require.NoError(t, f.controller.client.Update(context.Background(), f.content))
			if committed {
				path, err := nsmount.ResolveArtifactPath(f.controller.config.Storage.BasePath, string(f.content.UID), "main")
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(path, 0o700))
				require.NoError(t, agenttypes.WriteManifest(path, &agenttypes.CheckpointManifest{Artifact: agenttypes.ArtifactManifest{ContentUID: string(f.content.UID), ContainerName: "main"}}))
			}
			require.NoError(t, f.controller.reconcileCapture(context.Background(), f.content.Name))
			assert.False(t, f.capture.wasCalled())
			saved := getContent(t, f.controller, f.content.Name)
			if committed {
				assert.True(t, snapshotv1alpha1.IsPodSnapshotContentSucceeded(saved))
			} else {
				condition := meta.FindStatusCondition(saved.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
				require.NotNil(t, condition)
				assert.Equal(t, "CaptureOutcomeUnknown", condition.Reason)
			}
		})
	}
}

func TestRemoteCaptureRequiresDurableReservation(t *testing.T) {
	f := makeRemoteFixture(t)
	f.controller.client = interceptor.NewClient(f.controller.client.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("metadata write unavailable")
		},
	})
	require.ErrorContains(t, f.controller.reconcileCapture(context.Background(), f.content.Name), "persist remote capture attempt")
	assert.False(t, f.capture.wasCalled())
	assert.Empty(t, getContent(t, f.controller, f.content.Name).Annotations[remoteCaptureStarted])
}

// Only a confirmed terminal restore user without protection can release its
// artifact. Unknown or newly introduced phases must fail closed on both APIs.
func TestRemoteCleanupRequiresTerminalUnprotectedRestoreUsers(t *testing.T) {
	cases := []struct {
		name               string
		phase              corev1.PodPhase
		protected, blocked bool
	}{
		{name: "pending", phase: corev1.PodPending, blocked: true},
		{name: "running", phase: corev1.PodRunning, blocked: true},
		{name: "empty", blocked: true},
		{name: "unknown", phase: corev1.PodUnknown, blocked: true},
		{name: "unrecognized", phase: "FuturePhase", blocked: true},
		{name: "succeeded", phase: corev1.PodSucceeded},
		{name: "failed", phase: corev1.PodFailed},
		{name: "protected succeeded", phase: corev1.PodSucceeded, protected: true, blocked: true},
		{name: "protected failed", phase: corev1.PodFailed, protected: true, blocked: true},
	}
	for _, scope := range []string{"origin", "provider"} {
		for _, tc := range cases {
			t.Run(scope+"/"+tc.name, func(t *testing.T) {
				f := makeRemoteFixture(t)
				ctx := context.Background()
				user := f.origin
				if scope == "provider" {
					user = f.local
				}
				if user.Annotations == nil {
					user.Annotations = map[string]string{}
				}
				user.Annotations[podcontract.RestoreFromAnnotation] = f.content.Spec.PodSnapshotRef.Name
				user.Status.Phase = tc.phase
				if tc.protected {
					user.Finalizers = append(user.Finalizers, restorePodFinalizer)
				}
				f.content.DeletionTimestamp = ptr.To(metav1.Now())
				f.content.Finalizers = []string{snapshotv1alpha1.PodSnapshotContentArtifactCleanupFinalizer}
				f.controller.client = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.origin, f.mapping, f.content).Build()
				f.controller.remote.metadata = f.controller.client
				f.controller.localClient = fake.NewClientBuilder().WithScheme(contentScheme(t)).WithObjects(f.local, f.shadow, f.namespace).Build()
				f.controller.remote.local = f.controller.localClient
				root, err := artifact.ResolveContentRoot(f.controller.config.Storage.BasePath, string(f.content.UID))
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(root, 0o750))
				err = f.controller.reconcileCapture(ctx, f.content.Name)
				if tc.blocked {
					require.ErrorContains(t, err, "artifact still referenced")
					_, err = os.Stat(root)
					require.NoError(t, err)
					saved := getContent(t, f.controller, f.content.Name)
					assert.Contains(t, saved.Finalizers, snapshotv1alpha1.PodSnapshotContentArtifactCleanupFinalizer)
				} else {
					require.NoError(t, err)
					_, err = os.Stat(root)
					assert.ErrorIs(t, err, os.ErrNotExist)
				}
			})
		}
	}
}
