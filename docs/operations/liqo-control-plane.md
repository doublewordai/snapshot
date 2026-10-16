# Snapshot through a Liqo provider

This opt-in mode keeps Snapshot metadata on a consumer cluster while executing
checkpoint and restore against real Pods on one provider node. It requires
the matching operator and agent changes in this branch and a Liqo reflector
that stamps `offloading.liqo.io/origin-pod-uid`. It is not enabled by default.
Unit tests and local chart rendering do not qualify production GPU operation.

## Metadata and runtime ownership

The consumer Snapshot operator owns `SnapshotJob`, its batch Job and source Pod,
`PodSnapshot`, and `PodSnapshotContent`. The serving orchestrator and its restore
admission webhook also stay on the consumer. Liqo reflects workload Pods and
their status, not Snapshot objects. The provider runs the agent and PageBroker;
it must not run a second Snapshot operator for these workloads.

The agent uses its provider service account for local Pods, ShadowPods, namespace
identity and runtime discovery. A separate kubeconfig supplies the consumer
metadata client and content informer. Content status and artifact deletion
acknowledgements are written to the consumer; Pod conditions and finalizers are
written to the provider. Liqo carries the Pod status back to the consumer.
Snapshot objects and their UIDs are never copied to another API server.

## Required binding

An agent binds exactly one consumer namespace, accepted Liqo NamespaceMap
incarnation, provider namespace incarnation, virtual node, and physical node.
Provider namespace and NamespaceMap UIDs are administrator-supplied pins, not
inferred from Pod annotations. Recreating either requires an explicit binding
update after reviewing existing artifacts and users.

Before runtime access the agent reads both APIs, checks the namespace mapping,
and resolves the live source/restore Pod. It requires the current ShadowPod
controller owner reference, both cluster labels, virtual-node label, origin UID
stamp on both ShadowPod and Pod, and matching Snapshot request annotations.
The capture Pod UID must also equal the content's authoritative source UID.
The runtime container must belong to the validated provider Pod UID. Restore
rechecks the live snapshot/content binding immediately before execution.

Legacy ShadowPods without the origin UID stamp are rejected. Recreate them
through a capacity-preserving serving rollout after installing the reflector
patch. Do not add provenance to existing Pods by hand.

The provider namespace must be reserved for trusted reflection controllers and
operators. Workload service accounts must not create or mutate Pods, ShadowPods,
NamespaceMaps, namespace metadata or Snapshot objects. Owner references and
labels are not signatures against an administrator of that API server.

## Helm deployment split

Use the same reviewed Snapshot build for the consumer operator, provider agent
and PageBroker. The provider's ordinary in-cluster configuration must continue
to point to the provider. Never replace it with the consumer kubeconfig.

Consumer values (this does not enable the serving orchestrator's checkpoint gate):

```yaml
daemonset:
  enabled: false
operator:
  enabled: true
  artifactCleanup:
    remoteAgentNodes: [liqo-qualified-provider]
```

Provider values, with all placeholders replaced using read-only inventory:

```yaml
operator:
  enabled: false
daemonset:
  nodeSelector:
    kubernetes.io/hostname: qualified-gpu-node
  remoteControlPlane:
    kubeconfigSecret: snapshot-consumer-access
    virtualNode: liqo-qualified-provider
    originClusterID: consumer-cluster-id
    providerClusterID: provider-cluster-id
    namespace: dynamo-workers
    providerNamespace: mapped-provider-namespace
    providerNamespaceUID: replace-with-live-namespace-uid
    namespaceMapNamespace: consumer-tenant-namespace
    namespaceMapName: consumer-namespace-map
    namespaceMapUID: replace-with-live-namespacemap-uid
```

Use a dedicated credential Secret with a `kubeconfig` key. All its keys are
projected as files under `/etc/snapshot-remote`, read-only and only into the
agent. For rotating bearer tokens, add a `token` key and set the kubeconfig's
`tokenFile: /etc/snapshot-remote/token`; omit an embedded `token` value. Other
keys can supply CA and client certificate files referenced by absolute paths
under the same directory. Use HTTPS with CA validation; insecure TLS, HTTP,
exec authentication and auth-provider plugins are rejected. Token-file rotation
does not require replacing an in-memory bearer token. Do not use a `subPath`
mount for rotating credentials.
Client certificates or an embedded token require an agent restart on rotation.
Changing binding/configuration values also requires restarting the agent.

The chart refuses remote mode without a physical-hostname selector or with a
provider-local operator. Run exactly one agent for each physical/virtual-node
binding, and do not install overlapping agent releases. Provision and retain the provider artifact PVC and
compatible runtime socket/storage paths using the ordinary installation guide.
The consumer operator's local PVC is not the provider artifact store.

Consumer credential permissions:

| Scope | Resource | Verbs |
| --- | --- | --- |
| Consumer workload namespace | Pods | get, list |
| Consumer workload namespace | PodSnapshots | get |
| NamespaceMap namespace | The pinned NamespaceMap name | get |
| Cluster | PodSnapshotContents | get, list, watch, patch |
| Cluster | PodSnapshotContents/status | patch, update |

Kubernetes RBAC cannot express a per-node label condition for dynamic content
names. Cluster-scoped content permissions are an explicit trust boundary, not
namespace isolation. Use a separate identity per provider; keep it out of
workload Pods and audit access. Provider credentials never need permission to
create Snapshot resources on the consumer. Revoke/rotate identities through
the normal credential management system; this feature does not issue tokens.

## Artifact deletion and recovery

The consumer operator stamps cleanup delegation on new content for configured
remote virtual nodes. An unmarked content cannot start remote capture. Consumer
maintenance preserves the cleanup finalizer of delegated content, including
after its configured remote-node list changes. It cannot acknowledge deletion
merely because a directory is absent on its own filesystem.

The provider agent processes explicit content deletion on its capture queue.
It verifies the mapping and authoritative content incarnation, and requires
fresh Pod lists from both APIs. Every restore user of that snapshot must be
confirmed `Succeeded` or `Failed` and have no restore protection finalizer;
unknown, empty or unrecognized phases block deletion.
It removes only the ordinary artifact directory belonging to the consumer
content UID, then removes that content's cleanup finalizer using optimistic
locking. Symlinks, traversal, changed identities and API errors block cleanup.
Capture and cleanup for one content are serialized. Restore users retain Pod
protection until the operation finishes.

An agent restart recovers committed captures from their existing content-UID
paths and uses existing terminal restore status/sentinels to avoid replay.
Before capture, the agent records a durable attempt bound to the provider Pod
UID and container ID. After a crash, an attempt without a committed artifact
fails with `CaptureOutcomeUnknown`; create a new source and work order after
inspecting and cleaning up the old source. Never clear the marker to retry an
uncertain dump.
Provider loss or inaccessible metadata leaves resources pending rather than
authorizing a different node or deleting artifacts. Both APIs must recover
before new capture, restore or cleanup can proceed.

Rollback disables new serving checkpoint requests first, restores the ordinary
worker image with overlap capacity, and waits for in-flight operations and
streams. Retain this operator/agent protocol and storage while any delegated
content or restore user remains. Do not downgrade consumer maintenance to a
build that does not understand cleanup delegation. Do not delete finalizers,
NamespaceMaps or PVCs as a rollback shortcut.

## Qualification and limits

Before production activation, exercise capture and two independent restores
through the real offloading path, normal and streaming requests, tools/schema
output, advancing CPU telemetry, controller restart, metadata unavailability,
provider Pod replacement, explicit cleanup, and loaded serving drain/rollback.
Reserve capture and surge resources without preempting customer workers.

This implementation does not provide cross-node artifact transfer, multi-node
virtual-node scheduling, native Outpost reflection, tenant-controlled provider
namespaces, credential issuance, or automatic orphan artifact scans on provider
storage. Orphans remain retained for audited manual reclamation. Do not enable
a second Snapshot installation's local orphan scanner over this provider store:
it cannot see the consumer's authoritative content records. Multi-GPU/backend
support still depends on separate qualification of the exact runtime tuple.
