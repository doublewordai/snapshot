// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// cleanupRemoteContent is serialized with capture by its content queue key.
// It never scans for orphans: missing authoritative metadata is not permission
// to erase node-local state. Both APIs must be available for explicit deletion.
func (w *NodeController) cleanupRemoteContent(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) error {
	finalizer := snapshotv1alpha1.PodSnapshotContentArtifactCleanupFinalizer
	if !slices.Contains(content.Finalizers, finalizer) {
		return nil
	}
	if err := w.remote.validateMapping(ctx); err != nil {
		return err
	}
	for _, scope := range []struct {
		reader    client.Client
		namespace string
	}{
		{w.client, w.remote.config.Namespace}, {w.podClient(), w.remote.config.ProviderNamespace},
	} {
		pods := &corev1.PodList{}
		if err := scope.reader.List(ctx, pods, client.InNamespace(scope.namespace)); err != nil {
			return fmt.Errorf("list restore users before cleanup: %w", err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Annotations[podcontract.RestoreFromAnnotation] == content.Spec.PodSnapshotRef.Name &&
				(pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == "" || hasFinalizer(pod, restorePodFinalizer)) {
				return fmt.Errorf("artifact still referenced by pod %s/%s", pod.Namespace, pod.Name)
			}
		}
	}
	current := &snapshotv1alpha1.PodSnapshotContent{}
	if err := w.client.Get(ctx, client.ObjectKeyFromObject(content), current); err != nil {
		return err
	}
	if current.UID == "" || current.UID != content.UID || current.DeletionTimestamp == nil || current.Spec.Source.NodeName != w.metadataNodeName() {
		return fmt.Errorf("content deletion identity changed")
	}
	if err := validateRemoteContentStorage(current); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := removeRemoteArtifact(w.config.Storage.BasePath, string(current.UID)); err != nil {
		return err
	}
	before := current.DeepCopy()
	current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == finalizer })
	return w.client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func removeRemoteArtifact(basePath, uid string) error {
	if _, err := artifact.ResolveContentRoot(basePath, uid); err != nil {
		return err
	}
	rootPath, err := artifact.ResolveRoot(basePath)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(rootPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(uid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact root is not an ordinary directory")
	}
	return root.RemoveAll(uid)
}
