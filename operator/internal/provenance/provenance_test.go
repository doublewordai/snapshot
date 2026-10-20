// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	remote "github.com/ai-dynamo/snapshot/agent/pkg/provenance"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

type fixture struct {
	validator       *ShadowValidator
	origin          *corev1.Pod
	shadow, mapping *unstructured.Unstructured
	namespace       *corev1.Namespace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	binding := &remote.Binding{Kubeconfig: "/etc/metadata/kubeconfig", VirtualNode: "virtual-a", Namespace: "source",
		OriginClusterID: "consumer", ProviderClusterID: "provider", ProviderNamespace: "offloaded", ProviderNamespaceUID: "provider-namespace-uid",
		NamespaceMapNamespace: "tenant", NamespaceMapName: "mapping", NamespaceMapUID: "mapping-uid"}
	origin := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: binding.Namespace, UID: "source-uid",
		Annotations: map[string]string{podcontract.OriginPodUIDAnnotation: "source-uid"}}, Spec: corev1.PodSpec{NodeName: binding.VirtualNode}}
	shadow := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "offloading.liqo.io/v1beta1", "kind": "ShadowPod"}}
	shadow.SetName(origin.Name)
	shadow.SetNamespace(binding.ProviderNamespace)
	shadow.SetUID("shadow-uid")
	shadow.SetAnnotations(origin.Annotations)
	shadow.SetLabels(map[string]string{remote.OriginClusterLabel: binding.OriginClusterID, remote.ProviderClusterLabel: binding.ProviderClusterID, remote.VirtualNodeLabel: binding.VirtualNode})
	mapping := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "offloading.liqo.io/v1beta1", "kind": "NamespaceMap",
		"metadata": map[string]any{"name": binding.NamespaceMapName, "namespace": binding.NamespaceMapNamespace, "uid": binding.NamespaceMapUID,
			"labels": map[string]any{"liqo.io/remote-cluster-id": binding.ProviderClusterID}},
		"spec":   map[string]any{"desiredMapping": map[string]any{binding.Namespace: binding.ProviderNamespace}},
		"status": map[string]any{"currentMapping": map[string]any{binding.Namespace: map[string]any{"remoteNamespace": binding.ProviderNamespace, "phase": "Accepted"}}},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: binding.ProviderNamespace, UID: types.UID(binding.ProviderNamespaceUID)}}
	validator := &ShadowValidator{Binding: binding, ReflectorUsername: "liqo-reflector"}
	f := &fixture{validator, origin, shadow, mapping, namespace}
	f.clients(t)
	return f
}

func (f *fixture) clients(t *testing.T) {
	t.Helper()
	f.validator.Origin = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(f.origin, f.mapping).Build()
	f.validator.Provider = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(f.namespace).Build()
}

func request(t *testing.T, operation admissionv1.Operation, current, old *unstructured.Unstructured) admission.Request {
	t.Helper()
	data, err := json.Marshal(current)
	require.NoError(t, err)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request", Operation: operation, Namespace: current.GetNamespace(), Name: current.GetName(),
		Resource: metav1.GroupVersionResource{Group: "offloading.liqo.io", Version: "v1beta1", Resource: "shadowpods"},
		Kind:     metav1.GroupVersionKind{Group: "offloading.liqo.io", Version: "v1beta1", Kind: "ShadowPod"},
		Object:   runtime.RawExtension{Raw: data}, UserInfo: authenticationv1.UserInfo{Username: "liqo-reflector"},
	}}
	if old != nil {
		req.OldObject.Raw, err = json.Marshal(old)
		require.NoError(t, err)
	}
	return req
}

func TestCreateRequiresCurrentStampedOriginAndPinnedMapping(t *testing.T) {
	for _, name := range []string{"success", "missing stamp", "forged stamp", "unstamped origin", "origin name reused", "origin deleting", "wrong virtual node", "wrong clusters", "wrong reflector", "mapping replaced", "namespace replaced", "mapping not accepted", "metadata outage", "provider outage", "wrong resource", "wrong namespace"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			user := "liqo-reflector"
			switch name {
			case "missing stamp":
				f.shadow.SetAnnotations(nil)
			case "forged stamp":
				f.shadow.SetAnnotations(map[string]string{podcontract.OriginPodUIDAnnotation: "other-uid"})
			case "unstamped origin":
				f.origin.Annotations = nil
			case "origin name reused":
				f.origin.UID = "replacement-uid"
				f.origin.Annotations[podcontract.OriginPodUIDAnnotation] = string(f.origin.UID)
			case "origin deleting":
				now := metav1.Now()
				f.origin.DeletionTimestamp = &now
				f.origin.Finalizers = []string{"keep"}
			case "wrong virtual node":
				f.origin.Spec.NodeName = "another-node"
			case "wrong clusters":
				f.shadow.SetLabels(map[string]string{remote.OriginClusterLabel: "another-cluster"})
			case "wrong reflector":
				user = "untrusted"
			case "mapping replaced":
				f.mapping.SetUID("new-mapping")
			case "namespace replaced":
				f.namespace.UID = "new-namespace"
			case "mapping not accepted":
				require.NoError(t, unstructured.SetNestedField(f.mapping.Object, "Pending", "status", "currentMapping", f.origin.Namespace, "phase"))
			case "wrong namespace":
				f.shadow.SetNamespace("another-namespace")
			}
			f.clients(t)
			down := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("API unavailable")
			}}
			if name == "metadata outage" {
				f.validator.Origin = interceptor.NewClient(f.validator.Origin.(client.WithWatch), down)
			}
			if name == "provider outage" {
				f.validator.Provider = interceptor.NewClient(f.validator.Provider.(client.WithWatch), down)
			}
			req := request(t, admissionv1.Create, f.shadow, nil)
			req.UserInfo.Username = user
			if name == "wrong resource" {
				req.Resource.Resource = "pods"
			}
			response := f.validator.Handle(context.Background(), req)
			if name == "success" {
				assert.True(t, response.Allowed, "%v", response.Result)
			} else {
				assert.False(t, response.Allowed)
			}
		})
	}
}

func TestUpdateCannotRebindOrAdoptButAllowsCleanupWithoutOrigin(t *testing.T) {
	for _, name := range []string{"ordinary metadata", "source unavailable", "binding changed", "binding removed", "legacy adoption", "legacy stays unbound", "shadow replaced", "cluster labels changed"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			old := f.shadow.DeepCopy()
			f.shadow.SetFinalizers(nil)
			switch name {
			case "binding changed":
				f.shadow.SetAnnotations(map[string]string{podcontract.OriginPodUIDAnnotation: "replacement-uid"})
			case "binding removed":
				f.shadow.SetAnnotations(nil)
			case "legacy adoption":
				old.SetAnnotations(nil)
			case "legacy stays unbound":
				old.SetAnnotations(nil)
				f.shadow.SetAnnotations(nil)
			case "shadow replaced":
				f.shadow.SetUID("new-shadow")
			case "cluster labels changed":
				f.shadow.SetLabels(nil)
			}
			// An unchanged binding must not depend on origin existence or either API.
			f.validator.Origin = nil
			f.validator.Provider = nil
			response := f.validator.Handle(context.Background(), request(t, admissionv1.Update, f.shadow, old))
			allowed := name == "ordinary metadata" || name == "source unavailable" || name == "legacy stays unbound"
			assert.Equal(t, allowed, response.Allowed, "%v", response.Result)
		})
	}
}

func TestStampScopeIdentityAndRetry(t *testing.T) {
	for _, name := range []string{"stamp", "forged stamp repaired", "already stamped", "other namespace", "other node", "unscheduled", "deleting", "API failure", "patch conflict"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.origin.Annotations = map[string]string{"example.com/preserve": "yes"}
			switch name {
			case "forged stamp repaired":
				f.origin.Annotations[podcontract.OriginPodUIDAnnotation] = "forged"
			case "already stamped":
				f.origin.Annotations[podcontract.OriginPodUIDAnnotation] = string(f.origin.UID)
			case "other namespace":
				f.origin.Namespace = "other"
			case "other node":
				f.origin.Spec.NodeName = "other"
			case "unscheduled":
				f.origin.Spec.NodeName = ""
			case "deleting":
				now := metav1.Now()
				f.origin.DeletionTimestamp = &now
				f.origin.Finalizers = []string{"keep"}
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(f.origin).Build()
			calls := 0
			funcs := interceptor.Funcs{Patch: func(ctx context.Context, inner client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				calls++
				data, err := patch.Data(object)
				require.NoError(t, err)
				var body struct {
					Metadata struct{ UID, ResourceVersion string }
				}
				require.NoError(t, json.Unmarshal(data, &body))
				assert.Equal(t, string(f.origin.UID), body.Metadata.UID)
				assert.NotEmpty(t, body.Metadata.ResourceVersion)
				if name == "patch conflict" {
					return errors.New("conflict")
				}
				return inner.Patch(ctx, object, patch, options...)
			}}
			if name == "API failure" {
				funcs.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("unavailable")
				}
			}
			stamped := interceptor.NewClient(c, funcs)
			r := &StampReconciler{Reader: stamped, Writer: stamped, Namespace: "source", VirtualNodes: []string{"virtual-a"}}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.origin)}
			_, err := r.Reconcile(context.Background(), req)
			if name == "API failure" || name == "patch conflict" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if name == "stamp" || name == "forged stamp repaired" {
				require.Equal(t, 1, calls)
				current := &corev1.Pod{}
				require.NoError(t, c.Get(context.Background(), req.NamespacedName, current))
				assert.Equal(t, string(current.UID), current.Annotations[podcontract.OriginPodUIDAnnotation])
				assert.Equal(t, "yes", current.Annotations["example.com/preserve"])
				_, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err)
				assert.Equal(t, 1, calls, "repeat reconciliation must be a no-op")
			} else {
				assert.Zero(t, calls)
			}
		})
	}
}

func TestStampConfigurationRejectsUnscopedControllers(t *testing.T) {
	for _, c := range []StampReconciler{
		{}, {Namespace: "source"}, {Namespace: "source", VirtualNodes: []string{""}},
		{Namespace: "source", VirtualNodes: []string{"node,"}}, {Namespace: "bad/namespace", VirtualNodes: []string{"node"}},
	} {
		require.Error(t, c.Validate())
	}
	require.NoError(t, (&StampReconciler{Namespace: "source", VirtualNodes: []string{"virtual-a"}}).Validate())
}
