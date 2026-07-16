# External Checkpoint Storage

## Overview

By default the checkpoint-restore operator treats every checkpoint archive as a
**node-local file**: the kubelet writes a `.tar` into the checkpoint directory
(`/var/lib/kubelet/checkpoints` by default) on the node where the container
runs, and creation, garbage collection, and restore all operate on that local
file.

External checkpoint storage adds the option to **replicate opted-in checkpoint
archives to S3-compatible object storage** (AWS S3, MinIO, or any S3-compatible
endpoint), so an archive survives the loss of its origin node and can be staged
onto a different node for restore.

The feature is **opt-in and non-disruptive**: until you enable it, nothing
changes. Local checkpoint creation, retention/GC, and restore behave exactly as
before for any checkpoint that is not matched by an opted-in policy.

> **Status: groundwork.** This release ships the control-plane surface — the
> `CheckpointArchive` CRD, the `uploadToExternalStorage` policy field, the
> `externalStorage` backend configuration, and the restore-side wait for
> cross-node archives. The component that performs the actual upload/download
> to object storage — the **checkpoint-syncer** — is not yet implemented.
> Enabling the feature today records `CheckpointArchive` objects but does not
> move any bytes to S3. See [What is and isn't implemented](#what-is-and-isnt-implemented).

## Architecture

The feature is built from four pieces:

1. **`uploadToExternalStorage` policy field** — decides, per checkpoint, whether
   an archive should be synced. Resolved from the same
   `CheckpointRestoreOperator` policy hierarchy used for retention.
2. **`CheckpointArchive` CRD** — one object per opted-in archive. It is the
   record the syncer works from and the restore path waits on. It tracks where
   the archive lives locally and, once uploaded, where it lives in object
   storage.
3. **`externalStorage` backend config** — the S3-compatible bucket, endpoint,
   region, and credentials the checkpoint-syncer will use.
4. **Restore-side sync wait** — `PodRestore` does not render the restore Pod
   until the archive it needs reports that it is available locally on the
   target node.

```
 checkpoint created ──► policy opts in? ──yes──► CheckpointArchive created
 (schedule / annotation /                          │
  event / resource /                               ▼
  forensic chain)                          checkpoint-syncer (future)
                                            uploads to S3, sets
                                            Uploaded / externalURI
 local file GC'd ──────────────────────────► matching CheckpointArchive deleted

 restore on node B ──► wait until archive LocalAvailable on B ──► render Pod
```

## Enabling external storage

External storage is controlled entirely through the `CheckpointRestoreOperator`
resource. Two independent things are configured:

- **Which checkpoints to sync** — via `uploadToExternalStorage` on a policy.
- **Where to sync them** — via the cluster-wide `externalStorage` block.

```yaml
apiVersion: criu.org/v1
kind: CheckpointRestoreOperator
metadata:
  name: checkpointrestoreoperator-sample
spec:
  checkpointDirectory: /var/lib/kubelet/checkpoints
  applyPoliciesImmediately: true

  # 1) Opt checkpoints into external-storage sync.
  globalPolicy:
    maxCheckpointsPerNamespace: 50
    maxCheckpointsPerPod: 30
    maxCheckpointsPerContainer: 10
    uploadToExternalStorage: true      # <-- opt in

  # 2) Configure the S3-compatible backend the syncer uploads to.
  externalStorage:
    backend: s3                        # only "s3" is supported
    bucket: my-checkpoint-bucket
    endpoint: https://s3.us-east-1.amazonaws.com   # optional; set for MinIO/other providers
    region: us-east-1                  # optional
    secretRef:
      name: checkpoint-storage-credentials
```

### The `uploadToExternalStorage` field

`uploadToExternalStorage` is a `*bool` available on **every** policy level —
`globalPolicy`, `namespacePolicies`, `podPolicies`, and `containerPolicies`. It
opts the matched checkpoints into external-storage sync.

- **Default:** `false` (unset). Existing configurations are unaffected until you
  set it.
- **Resolution order:** for a given checkpoint the operator resolves the value
  most-specific-first — **container → pod → namespace → global** — with
  **fall-through on unset fields**. A more specific policy that leaves the field
  unset does *not* mask a broader policy that set it; the broader value applies.

Example: enable globally but exclude one noisy namespace.

```yaml
spec:
  globalPolicy:
    uploadToExternalStorage: true
  namespacePolicies:
    - namespace: scratch
      uploadToExternalStorage: false   # scratch checkpoints stay node-local
```

### The `externalStorage` block

`externalStorage` configures the S3-compatible backend for the checkpoint-syncer.
It is read **only** by the syncer — the controller-manager and cri-proxy never
use these credentials.

| Field | Required | Description |
|-------|----------|-------------|
| `backend` | yes | Storage backend. Only `s3` is supported (works with any S3-compatible endpoint). |
| `bucket` | yes | Destination bucket for uploaded archives. |
| `endpoint` | no | Overrides the default AWS endpoint — set this for MinIO or other S3-compatible providers. |
| `region` | no | The bucket's region. |
| `secretRef` | yes | Name of a `Secret` (in the syncer's namespace) holding the backend credentials (e.g. access key / secret key). |

Leaving `externalStorage` unset disables external storage entirely and has no
effect on local checkpoint creation, retention, or restore.

## The `CheckpointArchive` resource

Whenever a checkpoint is created **and** the resolved policy opts in, the
operator creates a `CheckpointArchive` object in the checkpoint's namespace.
Checkpoints outside an opted-in policy get no `CheckpointArchive` at all, so
clusters that never enable the feature see no extra API objects.

```console
$ kubectl get checkpointarchives -n default
NAME                               NODE                      UPLOADED   EXTERNALURI            AGE
ckpt-web-app-nginx-2wtjc           worker-1                  True       s3://my-bucket/...     3m
ckpt-web-app-nginx-bt48n           worker-1                                                    10s
```

### Spec (set by the operator at creation)

| Field | Description |
|-------|-------------|
| `node` | Node the archive was created on and currently lives on. |
| `localPath` | Absolute path of the `.tar` on the node's disk at creation time. Not re-validated later — the local GC remains the sole authority on whether the file still exists. |
| `namespace` | Namespace of the source pod. |
| `pod` | Name of the source pod. |
| `container` | Name of the source container. |

### Status (set by the checkpoint-syncer)

| Field | Description |
|-------|-------------|
| `conditions[type=LocalAvailable]` | Whether the archive is present on local disk on `spec.node` right now. The restore path waits on this. |
| `conditions[type=Uploaded]` | Whether the syncer has uploaded the archive to object storage. `False` until it has. |
| `externalURI` | Object storage location (e.g. `s3://bucket/key.tar`) once uploaded. Empty until then. |

### Lifecycle and garbage collection

A `CheckpointArchive` is tied to the local file it describes. When the retention
garbage collector removes a local checkpoint archive, it also **deletes the
matching `CheckpointArchive`** (matched by `localPath`), so the sync record
never outlives the file it describes. Pinned checkpoints (protected from GC) keep
both the local file and their `CheckpointArchive`. See
[Checkpoint Lifecycle Policies](retention_policy.md) for retention/GC details.

## Restore across nodes

When restoring, `PodRestore` may target a node other than the one the checkpoint
was taken on (for example, the origin node is gone). If a `CheckpointArchive`
tracks the checkpoint and names a **different** node than the restore target,
the reconciler waits — reporting `WaitingForArchiveSync` — and does **not**
render the restore Pod until the archive reports `LocalAvailable` on the target
node. This prevents a restore from racing ahead of the syncer staging the file
locally.

Restores are unaffected when:

- no `CheckpointArchive` tracks the checkpoint (pure node-local flow), or
- the archive already names the restore target node.

See [Restoring from a Checkpoint](restore.md) for the full restore flow.

## Prerequisites

The operator triggers checkpoints through the kubelet checkpoint API, proxied by
the API server. The kubelet authorizes that request as the API server's own
identity (`kube-apiserver-kubelet-client`), whose default role
`system:kubelet-api-admin` does **not** include the `nodes/checkpoint`
subresource. Without an additional grant, every checkpoint fails with:

```
status 403: Forbidden (user=kube-apiserver-kubelet-client, verb=create,
resource=nodes, subresource(s)=[checkpoint])
```

Grant the permission with a `ClusterRole` bound to that identity:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubelet-checkpoint
rules:
  - apiGroups: [""]
    resources: ["nodes/checkpoint"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: apiserver-kubelet-checkpoint
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubelet-checkpoint
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: kube-apiserver-kubelet-client
```

This is a cluster prerequisite for checkpointing in general, not specific to
external storage.

## What is and isn't implemented

**Implemented now:**

- `CheckpointArchive` CRD and its creation on every opted-in checkpoint (from
  the schedule, annotation, event, resource, and forensic-snapshot-chain
  triggers).
- `uploadToExternalStorage` resolution across the policy hierarchy.
- GC mirroring: deleting a local archive deletes its `CheckpointArchive`.
- `externalStorage` configuration surface on `CheckpointRestoreOperator`.
- Restore-side wait for a cross-node archive to become `LocalAvailable`.

**Not yet implemented:**

- The **checkpoint-syncer** component that actually uploads archives to S3,
  downloads/stages them onto other nodes, and sets the `Uploaded`,
  `LocalAvailable`, and `externalURI` status. Until it exists, `externalStorage`
  moves no data, `CheckpointArchive` status stays empty, and a cross-node
  restore that depends on syncing will wait indefinitely.

Enable `uploadToExternalStorage` today to start **recording** the archives that
should be synced; wiring the syncer to those records is the next step.
