//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func minimalCRD(kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".offloading.liqo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "offloading.liqo.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Singular: plural[:len(plural)-1], Kind: kind, ListKind: kind + "List"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptr.To(true)}},
			}},
		},
	}
}

func startAPI(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) (*rest.Config, client.WithWatch) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	require.NotEmpty(t, assets, "integration tests require pinned envtest assets")
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, admissionregistrationv1.AddToScheme(scheme))
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return cfg, c
}

// A real provider apiserver invokes the TLS webhook, while a second apiserver
// holds authoritative consumer identities. No Liqo fork, GPU or runtime seam is
// involved. Reflection itself is simulated by copying the source annotations.
func TestStockLiqoProvenanceAdmissionLifecycle(t *testing.T) {
	ctx := context.Background()
	_, origin := startAPI(t, minimalCRD("NamespaceMap", "namespacemaps"))
	providerConfig, provider := startAPI(t, minimalCRD("ShadowPod", "shadowpods"))
	f := newFixture(t)
	binding := f.validator.Binding
	for _, namespace := range []string{binding.Namespace, binding.NamespaceMapNamespace} {
		require.NoError(t, origin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: binding.ProviderNamespace}}
	require.NoError(t, provider.Create(ctx, ns))
	binding.ProviderNamespaceUID = string(ns.UID)
	mapping := f.mapping.DeepCopy()
	mapping.SetUID("")
	mapping.SetResourceVersion("")
	require.NoError(t, origin.Create(ctx, mapping))
	binding.NamespaceMapUID = string(mapping.GetUID())
	source := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: binding.Namespace},
		Spec: corev1.PodSpec{NodeName: binding.VirtualNode, Containers: []corev1.Container{{Name: "engine", Image: "example.invalid/engine"}}}}
	require.NoError(t, origin.Create(ctx, source))
	// A legacy ShadowPod created before guard installation must never be adopted.
	legacy := f.shadow.DeepCopy()
	legacy.SetName("legacy")
	legacy.SetUID("")
	legacy.SetAnnotations(nil)
	require.NoError(t, provider.Create(ctx, legacy))
	block, _ := pem.Decode(providerConfig.CertData)
	require.NotNil(t, block)
	certificate, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	validator := &ShadowValidator{Binding: binding, Origin: origin, Provider: provider, ReflectorUsername: certificate.Subject.CommonName}
	server := httptest.NewTLSServer(&admission.Webhook{Handler: validator})
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	hook := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-provenance-test"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "shadowpod-provenance.snapshot.nvidia.com", AdmissionReviewVersions: []string{"v1"},
			SideEffects: ptr.To(admissionregistrationv1.SideEffectClassNone), FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			ClientConfig:      admissionregistrationv1.WebhookClientConfig{URL: ptr.To(server.URL + "/validate-shadowpod"), CABundle: ca},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": binding.ProviderNamespace}},
			Rules: []admissionregistrationv1.RuleWithOperations{{Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{APIGroups: []string{"offloading.liqo.io"}, APIVersions: []string{"v1beta1"}, Resources: []string{"shadowpods"}}}},
		}},
	}
	require.NoError(t, provider.Create(ctx, hook))
	t.Cleanup(func() { require.NoError(t, provider.Delete(ctx, hook)) })
	// Wait for the apiserver's admission configuration informer to observe the
	// webhook. Dry-run creates cannot leave an unguarded object while it warms up.
	shadow := f.shadow.DeepCopy()
	shadow.SetUID("")
	shadow.SetAnnotations(nil)
	require.Eventually(t, func() bool { return provider.Create(ctx, shadow.DeepCopy(), client.DryRunAll) != nil }, 10*time.Second, 50*time.Millisecond)
	require.Error(t, provider.Create(ctx, shadow.DeepCopy()), "early reflection must wait for stamping")
	missing := shadow.DeepCopy()
	require.True(t, apierrors.IsNotFound(provider.Get(ctx, client.ObjectKeyFromObject(shadow), missing)))
	stamper := &StampReconciler{Reader: origin, Writer: origin, Namespace: binding.Namespace, VirtualNodes: []string{binding.VirtualNode}}
	_, err = stamper.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(source)})
	require.NoError(t, err)
	require.NoError(t, origin.Get(ctx, client.ObjectKeyFromObject(source), source))
	shadow.SetAnnotations(source.Annotations)
	require.NoError(t, provider.Create(ctx, shadow), "stock reflection's retry must succeed after stamping")

	// Changing only annotations on an old ShadowPod cannot bind it to a newly
	// created consumer Pod, even though normal Liqo reflection updates metadata.
	oldUID := source.UID
	require.NoError(t, origin.Delete(ctx, source, client.GracePeriodSeconds(0)))
	replacement := source.DeepCopy()
	replacement.SetUID("")
	replacement.SetResourceVersion("")
	replacement.SetAnnotations(nil)
	replacement.Status = corev1.PodStatus{}
	require.NoError(t, origin.Create(ctx, replacement))
	require.NotEqual(t, oldUID, replacement.UID)
	_, err = stamper.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(replacement)})
	require.NoError(t, err)
	require.NoError(t, origin.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
	rebound := shadow.DeepCopy()
	rebound.SetAnnotations(replacement.Annotations)
	require.Error(t, provider.Update(ctx, rebound), "a reused source name cannot rebind an existing ShadowPod")
	adopted := legacy.DeepCopy()
	adopted.SetAnnotations(replacement.Annotations)
	require.Error(t, provider.Update(ctx, adopted), "legacy objects cannot acquire provenance")

	// New bindings need live APIs. Existing unchanged bindings can still be
	// garbage-collected after the source or mapping disappears.
	require.NoError(t, origin.Delete(ctx, mapping))
	fresh := shadow.DeepCopy()
	fresh.SetName("another-worker")
	fresh.SetUID("")
	fresh.SetResourceVersion("")
	require.Error(t, provider.Create(ctx, fresh), "mapping disappearance blocks creation")
	require.NoError(t, origin.Delete(ctx, replacement, client.GracePeriodSeconds(0)))
	shadow.SetFinalizers([]string{"example.com/protection"})
	require.NoError(t, provider.Update(ctx, shadow))
	shadow.SetFinalizers(nil)
	require.NoError(t, provider.Update(ctx, shadow), "cleanup must not depend on the origin API")
	legacy.SetAnnotations(map[string]string{"example.com/note": "still-unbound"})
	require.NoError(t, provider.Update(ctx, legacy))

	// The server is unavailable: failurePolicy=Fail must prevent admission.
	server.Close()
	require.Error(t, provider.Create(ctx, fresh), "webhook outage must fail closed")
}

func TestStampCannotOverwriteSameNamedReplacement(t *testing.T) {
	ctx := context.Background()
	_, c := startAPI(t, minimalCRD("NamespaceMap", "namespacemaps"))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "source"}}
	require.NoError(t, c.Create(ctx, namespace))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: namespace.Name},
		Spec: corev1.PodSpec{NodeName: "virtual-a", Containers: []corev1.Container{{Name: "engine", Image: "example.invalid/engine"}}}}
	require.NoError(t, c.Create(ctx, pod))
	replacement := pod.DeepCopy()
	replacement.SetUID("")
	replacement.SetResourceVersion("")
	writer := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, inner client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
		require.NoError(t, inner.Delete(ctx, pod, client.GracePeriodSeconds(0)))
		require.NoError(t, inner.Create(ctx, replacement))
		return inner.Patch(ctx, object, patch, options...)
	}})
	r := &StampReconciler{Reader: c, Writer: writer, Namespace: namespace.Name, VirtualNodes: []string{"virtual-a"}}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
	require.Error(t, err)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(replacement), replacement))
	require.NotEqual(t, pod.UID, replacement.UID)
	require.Empty(t, replacement.Annotations[podcontract.OriginPodUIDAnnotation])
}
