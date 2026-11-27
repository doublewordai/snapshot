// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	remote "github.com/ai-dynamo/snapshot/agent/pkg/provenance"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// ShadowValidator runs on the provider API. CREATE requires a current consumer
// UID observed through both the reflected request and a fresh consumer read.
// UPDATE cannot acquire or change that binding, even after source replacement.
type ShadowValidator struct {
	Binding           *remote.Binding
	Origin            client.Reader
	Provider          client.Reader
	ReflectorUsername string
	// ReflectorUsernames replaces ReflectorUsername on a provider cluster whose
	// nodes Liqo reflects to under separate identities: each bound virtual
	// node's reflector username.
	ReflectorUsernames map[string]string
}

func (v *ShadowValidator) Validate() error {
	if v.Binding == nil {
		return fmt.Errorf("provenance binding is required")
	}
	if err := v.Binding.Validate(); err != nil {
		return err
	}
	if len(v.ReflectorUsernames) == 0 {
		if v.ReflectorUsername == "" || strings.TrimSpace(v.ReflectorUsername) != v.ReflectorUsername {
			return fmt.Errorf("provenance reflector username must be nonempty without surrounding whitespace")
		}
		return nil
	}
	if v.ReflectorUsername != "" {
		return fmt.Errorf("provenance needs one reflector username or one per virtual node, not both")
	}
	nodes := v.Binding.VirtualNodes
	if v.Binding.VirtualNode != "" {
		nodes = []string{v.Binding.VirtualNode}
	}
	if len(v.ReflectorUsernames) != len(nodes) {
		return fmt.Errorf("provenance needs a reflector username for each bound virtual node, and no other")
	}
	for _, node := range nodes {
		name := v.ReflectorUsernames[node]
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("provenance reflector username for virtual node %q must be nonempty without surrounding whitespace", node)
		}
	}
	return nil
}

// reflectorFor is the username Liqo reflects a node's ShadowPods under.
func (v *ShadowValidator) reflectorFor(node string) string {
	if len(v.ReflectorUsernames) == 0 {
		return v.ReflectorUsername
	}
	return v.ReflectorUsernames[node]
}

func (v *ShadowValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if err := v.Validate(); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if req.Resource.Group != "offloading.liqo.io" || req.Resource.Version != "v1beta1" || req.Resource.Resource != "shadowpods" || req.SubResource != "" {
		return admission.Denied("unexpected provenance admission resource")
	}
	if req.Namespace != v.Binding.ProviderNamespace {
		return admission.Denied("ShadowPod is outside the configured provider namespace")
	}
	current, err := decodeShadow(req.Object.Raw)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if current.GetNamespace() != req.Namespace || current.GetName() == "" || (req.Name != "" && req.Name != current.GetName()) {
		return admission.Denied("ShadowPod request identity does not match its object")
	}
	switch req.Operation {
	case admissionv1.Create:
		err = v.validateCreate(ctx, req, current)
	case admissionv1.Update:
		err = validateUpdate(req, current)
	default:
		err = fmt.Errorf("unexpected provenance admission operation")
	}
	if err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("Snapshot provenance binding is valid")
}

func decodeShadow(data []byte) (*unstructured.Unstructured, error) {
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, object); err != nil {
		return nil, fmt.Errorf("decode ShadowPod: %w", err)
	}
	if object.GetAPIVersion() != "offloading.liqo.io/v1beta1" || object.GetKind() != "ShadowPod" {
		return nil, fmt.Errorf("unexpected ShadowPod object kind")
	}
	return object, nil
}

func (v *ShadowValidator) validateCreate(ctx context.Context, req admission.Request, shadow *unstructured.Unstructured) error {
	expected := v.reflectorFor(shadow.GetLabels()[remote.VirtualNodeLabel])
	if expected == "" || req.UserInfo.Username != expected {
		return fmt.Errorf("only the configured Liqo reflector can create bound ShadowPods")
	}
	stamp := shadow.GetAnnotations()[podcontract.OriginPodUIDAnnotation]
	if stamp == "" {
		return fmt.Errorf("ShadowPod is awaiting the Snapshot origin Pod UID stamp")
	}
	if err := v.Binding.ValidateLabels(shadow.GetLabels()); err != nil {
		return err
	}
	if err := v.Binding.ValidateMapping(ctx, v.Origin, v.Provider); err != nil {
		return err
	}
	origin, err := v.Binding.OriginPod(ctx, v.Origin, shadow.GetName())
	if err != nil {
		return err
	}
	if stamp != string(origin.UID) {
		return fmt.Errorf("ShadowPod stamp does not match the live origin Pod UID")
	}
	// With several bound nodes, a ShadowPod must be reflected for the node its
	// origin Pod runs on, not for another bound node.
	if origin.Spec.NodeName != shadow.GetLabels()[remote.VirtualNodeLabel] {
		return fmt.Errorf("ShadowPod virtual node does not match its origin Pod's node")
	}
	return nil
}

func validateUpdate(req admission.Request, current *unstructured.Unstructured) error {
	old, err := decodeShadow(req.OldObject.Raw)
	if err != nil {
		return err
	}
	if old.GetUID() == "" || current.GetUID() != old.GetUID() || current.GetNamespace() != old.GetNamespace() || current.GetName() != old.GetName() {
		return fmt.Errorf("ShadowPod incarnation changed")
	}
	before, after := old.GetAnnotations()[podcontract.OriginPodUIDAnnotation], current.GetAnnotations()[podcontract.OriginPodUIDAnnotation]
	if before != after {
		return fmt.Errorf("ShadowPod provenance is immutable; recreate an unbound or stale ShadowPod through a controlled rollout")
	}
	for _, key := range []string{remote.OriginClusterLabel, remote.ProviderClusterLabel, remote.VirtualNodeLabel} {
		if old.GetLabels()[key] != current.GetLabels()[key] {
			return fmt.Errorf("ShadowPod reflection label %s is immutable", key)
		}
	}
	// Unbound legacy objects may remain unbound or be deleted, but never adopted.
	// No source/API lookup is needed for an unchanged binding. This lets Liqo
	// remove finalizers and garbage-collect after the source Pod disappears.
	return nil
}

var _ admission.Handler = (*ShadowValidator)(nil)
