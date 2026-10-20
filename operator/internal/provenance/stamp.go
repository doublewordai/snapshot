// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// StampReconciler owns only the source identity annotation. The provider
// webhook gates creation until this post-create, UID-preconditioned write lands.
type StampReconciler struct {
	Reader       client.Reader
	Writer       client.Writer
	Namespace    string
	VirtualNodes []string
}

func (r *StampReconciler) Validate() error {
	if problems := validation.IsDNS1123Label(r.Namespace); len(problems) != 0 {
		return fmt.Errorf("provenance origin namespace: %s", strings.Join(problems, "; "))
	}
	if len(r.VirtualNodes) == 0 {
		return fmt.Errorf("provenance stamping requires at least one virtual node")
	}
	for _, node := range r.VirtualNodes {
		if problems := validation.IsDNS1123Subdomain(node); len(problems) != 0 {
			return fmt.Errorf("provenance virtual node %q: %s", node, strings.Join(problems, "; "))
		}
	}
	return nil
}

func (r *StampReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	pod := &corev1.Pod{}
	if err := r.Reader.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pod.UID == "" || pod.DeletionTimestamp != nil || !slices.Contains(r.VirtualNodes, pod.Spec.NodeName) {
		return ctrl.Result{}, nil
	}
	if pod.Annotations[podcontract.OriginPodUIDAnnotation] == string(pod.UID) {
		return ctrl.Result{}, nil
	}
	// Both UID and resourceVersion must match: a same-named replacement must not
	// receive an old observation's stamp, even if a writer bypasses the cache.
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": string(pod.UID), "resourceVersion": pod.ResourceVersion,
		"annotations": map[string]string{podcontract.OriginPodUIDAnnotation: string(pod.UID)},
	}})
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Writer.Patch(ctx, pod, client.RawPatch(types.MergePatchType, body)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("stamp origin Pod %s: %w", req.NamespacedName, err)
	}
	return ctrl.Result{}, nil
}

func (r *StampReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("snapshot-origin-provenance").
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			pod, ok := obj.(*corev1.Pod)
			return ok && pod.Namespace == r.Namespace && slices.Contains(r.VirtualNodes, pod.Spec.NodeName)
		})).Complete(r)
}
