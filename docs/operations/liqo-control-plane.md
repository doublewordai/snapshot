# Snapshot through a Liqo provider

This opt-in mode keeps Snapshot metadata on a consumer cluster while executing
checkpoint and restore against real Pods on one provider node. It requires
the matching Snapshot operator, agent and provenance webhook from this branch.
Liqo remains unmodified; no Liqo fork or custom virtual-kubelet image is required.
This mode is not enabled by default.
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

### Stock Liqo provenance lifecycle

The consumer operator stamps `nvidia.com/snapshot-origin-pod-uid` from the actual
Pod UID after creation, using UID and resource-version preconditions. It watches
all Pods in the configured consumer namespace on the configured virtual nodes,
including ordinary workers that may later be restore candidates. Unscheduled,
deleting and out-of-scope Pods are left alone. Incorrect source stamps are
repaired; they are never accepted as evidence of identity.

A separate provider validating webhook intercepts ShadowPod CREATE and UPDATE
in the bound provider namespace. CREATE requires the configured authenticated
Liqo reflector username, valid cluster/node labels, fresh namespace/mapping UID
checks, and a stamp matching the live consumer Pod. A request that beats the
stamp is rejected; stock Liqo reports a reflection error and retries it.
The selected Liqo source returns this error before marking creation successful.

UPDATE cannot acquire, remove or change a binding or its cluster/node labels.
An unchanged binding can be updated without a consumer lookup, allowing finalizer
removal after the source disappears or an API becomes unavailable. Legacy
ShadowPods may remain unbound or be deleted, but cannot be adopted. The agent
requires the new Snapshot stamp on the origin Pod, ShadowPod and provider Pod;
the old reflector-owned `offloading.liqo.io/origin-pod-uid` key is not accepted.

Use a new dedicated mapped namespace and install the guard before introducing
eligible workloads. Do not enable stamping blindly on an existing serving
namespace: adding a stamp to an old source makes its legacy ShadowPod metadata
updates fail until the workload is recreated. The webhook fails closed for every ShadowPod in that
namespace, so all newly offloaded Pods there depend on stamping and webhook
availability. Ensure Liqo's annotation filtering preserves the Snapshot key.
Recreate existing unbound workloads through a capacity-preserving serving
rollout; do not add provider provenance manually.

The consumer operator receives only a namespaced Pod patch permission in addition
to its existing reads. The provider webhook uses its own local service account
with only GET on the pinned provider namespace, and a separate consumer
credential with only GET on Pods and the named NamespaceMap. It has no content
status permissions, artifact volume, CRI access or GPU privileges.

The provider namespace must be reserved for trusted reflection controllers and
operators. Workload service accounts must not create or mutate Pods, ShadowPods,
NamespaceMaps, namespace metadata or Snapshot objects. Owner references and
labels are not signatures against an administrator of that API server.

## Helm deployment split

Use the same reviewed Snapshot build for the consumer operator, provider agent
and PageBroker. The operator image also contains `/provenance-webhook`, deployed
as a separate provider process without Snapshot reconciliation or storage access.
The provider's ordinary in-cluster configuration must continue
to point to the provider. Never replace it with the consumer kubeconfig.

Consumer values (this does not enable the serving orchestrator's checkpoint gate):

```yaml
daemonset:
  enabled: false
operator:
  enabled: true
  artifactCleanup:
    remoteAgentNodes: [liqo-qualified-provider]
provenance:
  consumer:
    enabled: true
    namespace: dynamo-workers
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
provenance:
  provider:
    enabled: true
    kubeconfigSecret: snapshot-admission-consumer-access
    tlsSecret: snapshot-provenance-serving-tls
    caBundle: REPLACE_WITH_BASE64_PEM_CA
    reflectorUsername: REPLACE_WITH_AUTHENTICATED_REFLECTOR_USERNAME
```

Use separate dedicated credential Secrets for the agent and admission webhook,
each with a `kubeconfig` key. All its keys are
projected as files under `/etc/snapshot-remote`, read-only and only into the
process that consumes that Secret. For rotating bearer tokens, add a `token` key and set the kubeconfig's
`tokenFile: /etc/snapshot-remote/token`; omit an embedded `token` value. Other
keys can supply CA and client certificate files referenced by absolute paths
under the same directory. Use HTTPS with CA validation; insecure TLS, HTTP,
exec authentication and auth-provider plugins are rejected. Token-file rotation
does not require replacing an in-memory bearer token. Do not use a `subPath`
mount for rotating credentials.
Client certificates or an embedded token require an agent restart on rotation.
Changing binding/configuration values also requires restarting the agent.

Provider rendering requires a physical-hostname selector and provenance webhook,
and rejects a provider-local Snapshot operator. Consumer remote-node delegation
requires the stamper to be enabled in that release. These are configuration
checks; they do not prove the other cluster's components are running.
Admission requires an existing TLS Secret (`tls.crt` and `tls.key`), a CA bundle
trusted by the provider API server, and the actual Liqo reflector username. The
serving certificate must cover the rendered webhook Service DNS name
`<service-name>.<release-namespace>.svc`. Provision it through the existing secret
process before installation. The webhook hot-reloads serving certificate updates;
CA changes also require updating its `caBundle` through GitOps. No certificate
issuer is installed automatically. Keep the old CA trusted during rotation.
The provider API server must reach the webhook Service over HTTPS (Service
port 443 to Pod port 9443). Readiness waits for the webhook server. If that
server is unavailable, CREATE and UPDATE, including finalizer updates, block
until it recovers. A consumer API outage alone still permits unchanged binding
updates. Recover the webhook instead of bypassing the admission guard.

Run exactly one agent for each physical/virtual-node binding, and do not install
overlapping agent releases. Provision and retain the provider artifact PVC and
compatible runtime socket/storage paths using the ordinary installation guide.
The consumer operator's local PVC is not the provider artifact store.

Agent consumer credential permissions:

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

## Storage and runtime compatibility

Remote mode currently supports only the legacy node-local PVC lifecycle:
`PodSnapshotContent.spec.storage` must be absent. Capture, restore and deletion
reject artifact-addressed content before runtime or filesystem operations.
The remote agent cannot use its content-UID directory to acknowledge deletion
of a publication owned by an artifact-addressed store. Native installations
retain upstream's artifact-addressed path. Do not remove an existing storage
binding or convert a published artifact to bypass this restriction.

The agent registers its namespace-scoped NRI plugin before starting its
controller. NRI v0.10 or newer (containerd 2.2 or newer) enables the PID-1 restore
proxy. On older runtimes or nodes without NRI, upstream's fallback restores
beside an inert destination command and requires a liveness probe of the actual
workload. Without the proxy, workload exit does not terminate the container and
container termination signals do not reach the restored workload. Qualify
drain, termination and failure detection separately before using that fallback;
see [the restore contract](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

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

Local integration tests use two real Kubernetes API servers and a TLS admission
webhook to exercise early rejection, successful retry after stamping, stale and
legacy binding rejection, fail-closed admission, and cleanup after source loss.
They simulate annotation reflection and do not run the full Liqo deployment.

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
