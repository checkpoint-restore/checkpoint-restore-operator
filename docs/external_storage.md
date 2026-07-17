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

> **Status: end to end.** This release ships both the control-plane surface —
> the `CheckpointArchive` CRD, the `uploadToExternalStorage` policy field, the
> `externalStorage` backend configuration, and the restore-side wait for
> cross-node archives — and the **checkpoint-syncer** component that performs
> the actual upload/download/delete against S3-compatible object storage. The
> syncer ships as an opt-in DaemonSet; see [Enabling the
> syncer](#enabling-the-syncer) to deploy it. See [What is and isn't
> implemented](#what-is-and-isnt-implemented) for the exact split.

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

```text
 checkpoint created ──► policy opts in? ──yes──► CheckpointArchive created
 (schedule / annotation /                          │
  event / resource /                               ▼
  forensic chain)                          checkpoint-syncer (DaemonSet)
                                            uploads to S3, sets
                                            Uploaded / externalURI
 local file GC'd ──────────────────────────► matching CheckpointArchive deleted

 restore on node B ──► wait until node B in status.availableNodes ──► render Pod
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
| --- | --- | --- |
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
| --- | --- |
| `node` | Node the archive was created on and currently lives on. |
| `localPath` | Absolute path of the `.tar` on the node's disk at creation time. Not re-validated later — the local GC remains the sole authority on whether the file still exists. |
| `namespace` | Namespace of the source pod. |
| `pod` | Name of the source pod. |
| `container` | Name of the source container. |
| `requestedNodes` | Nodes that need a local copy staged for an imminent restore. `PodRestore` appends its target node; the syncer on each listed node downloads the archive locally. |

### Status

| Field | Description |
| --- | --- |
| `conditions[type=Uploaded]` | Whether the syncer has uploaded the archive to object storage. `False` until it has. |
| `externalURI` | Object storage location (e.g. `s3://bucket/key.tar`) once uploaded. Empty until then. |
| `availableNodes` | Nodes that currently hold a local copy of the archive. The origin node (`spec.node`) is listed at creation; a syncer appends its node after a successful download. The restore path waits until the target node appears here. |

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
tracks the checkpoint and the restore target is **not** listed in
`status.availableNodes`, the reconciler appends the target to
`spec.requestedNodes`, waits — reporting `WaitingForArchiveSync` — and does
**not** render the restore Pod until the target node appears in
`status.availableNodes`. This prevents a restore from racing ahead of the syncer
staging the file locally.

Restores are unaffected when:

- no `CheckpointArchive` tracks the checkpoint (pure node-local flow), or
- the restore target node is already in `status.availableNodes`.

See [Restoring from a Checkpoint](restore.md) for the full restore flow.

## Prerequisites

The operator triggers checkpoints through the kubelet checkpoint API, proxied by
the API server. The kubelet authorizes that request as the API server's own
identity (`kube-apiserver-kubelet-client`), whose default role
`system:kubelet-api-admin` does **not** include the `nodes/checkpoint`
subresource. Without an additional grant, every checkpoint fails with:

```text
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

## Enabling the syncer

The checkpoint-syncer is the component that reads `CheckpointArchive` objects
and actually moves bytes: it uploads newly created archives to the configured
S3 bucket, deletes the object when the `CheckpointArchive` is deleted, and (on
the target node during a cross-node restore) downloads the object back to
local disk. It ships as a DaemonSet so an instance runs on every node — each
instance only acts on archives whose `spec.node` matches its own node.

### 1. Deploy the DaemonSet

The DaemonSet is disabled by default and opt-in via the Helm chart:

```bash
# Build (and, for kind, load) the checkpoint-syncer image first.
docker build -t checkpoint-syncer:dev -f Dockerfile.checkpoint-syncer .
kind load docker-image checkpoint-syncer:dev

helm upgrade --install checkpoint-restore-operator ./charts/checkpoint-restore-operator \
  --namespace checkpoint-restore-operator --create-namespace \
  --set checkpointSyncer.enabled=true \
  --set checkpointSyncer.image.repository=checkpoint-syncer \
  --set checkpointSyncer.image.tag=dev
```

Confirm it's running:

```bash
kubectl -n checkpoint-restore-operator get daemonset checkpoint-restore-operator-checkpoint-syncer
```

The syncer runs as root (`runAsUser: 0`, configurable via
`checkpointSyncer.podSecurityContext`) because kubelet writes checkpoint
archives as `root:root` with mode `0600`; the syncer must read them to upload
and write them when staging a downloaded archive.

### 2. Create the credentials Secret

The syncer reads its S3 credentials from a `Secret` in its own namespace
(`checkpoint-restore-operator` by default), with exactly two keys:

```bash
kubectl -n checkpoint-restore-operator create secret generic checkpoint-storage-credentials \
  --from-literal=accessKeyID=<your-access-key-id> \
  --from-literal=secretAccessKey=<your-secret-access-key>
```

The key names are fixed: `accessKeyID` and `secretAccessKey`.

### 3. Point a MinIO (or any S3-compatible) backend at it

For local/dev testing, `test/minio.yaml` deploys a single-instance MinIO
server in the `checkpoint-restore-operator` namespace, a `checkpoints`
bucket, and this same `checkpoint-storage-credentials` Secret
(`minioadmin`/`minioadmin`):

```bash
kubectl apply -f test/minio.yaml
```

Then set `spec.externalStorage` on the `CheckpointRestoreOperator` (see
[The `externalStorage` block](#the-externalstorage-block) above) to point at
it:

```yaml
spec:
  externalStorage:
    backend: s3
    bucket: checkpoints
    endpoint: http://minio.checkpoint-restore-operator:9000
    region: us-east-1
    secretRef:
      name: checkpoint-storage-credentials
```

For AWS S3, omit `endpoint` (or set it to the regional S3 endpoint) and point
`secretRef` at a Secret holding a real IAM access key/secret key pair.

When targeting a real S3 (or other production) backend, note:

- **The bucket must already exist.** The syncer never creates it — the MinIO
  fixture above creates the `checkpoints` bucket for you, but for AWS you must
  create the bucket yourself, in the region you set in `region`. A missing or
  mismatched region surfaces as a signing/redirect error on the first upload.
- **Credential permissions.** The IAM identity behind the access key needs
  `s3:PutObject`, `s3:GetObject`, and `s3:DeleteObject` on the bucket's objects
  (`arn:aws:s3:::<bucket>/*`).
- **Only static access keys are supported.** Credentials come exclusively from
  the `accessKeyID`/`secretAccessKey` Secret keys; IAM Roles for Service
  Accounts (IRSA), instance profiles, and STS are not (yet) wired up, so on EKS
  you still supply a long-lived access key pair via the Secret.
- **Transport security.** An empty or `https://` endpoint uses TLS; only an
  explicit `http://` endpoint (as in the MinIO dev fixture) is plaintext.

### 4. Verify uploads and deletion

After a checkpoint is created under an opted-in policy, watch its
`CheckpointArchive`:

```bash
kubectl get checkpointarchive -n <namespace> <name> -o yaml
```

- `status.conditions[type=Uploaded].status` becomes `True` once the syncer has
  uploaded the archive.
- `status.externalURI` is populated with the object's location, e.g.
  `s3://checkpoints/<namespace>/<pod>/<container>/<file>.tar`.

To confirm the object is actually in the bucket (e.g. against the MinIO
fixture above):

```bash
kubectl run mc --rm -it --restart=Never --image=minio/mc -n checkpoint-restore-operator -- \
  sh -c "mc alias set local http://minio.checkpoint-restore-operator:9000 minioadmin minioadmin && \
         mc ls -r local/checkpoints"
```

Deleting the `CheckpointArchive` (directly, or indirectly via local GC) drives
the syncer's delete finalizer, which removes the object from the bucket
before the finalizer is cleared and the CR disappears:

```bash
kubectl delete checkpointarchive -n <namespace> <name>
# re-run `mc ls -r local/checkpoints` above — the object should be gone.
```

See `test/run_tests.bats` (`test_external_storage_upload_and_delete_minio`)
for a full, scripted version of this flow against the `test/minio.yaml`
fixture.

## What is and isn't implemented

**Implemented now:**

- `CheckpointArchive` CRD and its creation on every opted-in checkpoint (from
  the schedule, annotation, event, resource, and forensic-snapshot-chain
  triggers).
- `uploadToExternalStorage` resolution across the policy hierarchy.
- GC mirroring: deleting a local archive deletes its `CheckpointArchive`.
- `externalStorage` configuration surface on `CheckpointRestoreOperator`.
- Restore-side wait for a cross-node archive: the reconciler holds the restore
  until the target node appears in `status.availableNodes` (requesting it via
  `spec.requestedNodes`).
- The **checkpoint-syncer** DaemonSet: uploads opted-in archives to the
  configured S3-compatible bucket and sets `Uploaded`/`externalURI`,
  downloads/stages archives onto a restore target node and appends its node to
  `status.availableNodes`, and deletes the remote object (via a finalizer)
  when a `CheckpointArchive` is deleted.

**Not yet implemented / out of scope for this release:**

- Independent retention/lifecycle management of objects already uploaded to
  remote storage (today, remote object lifecycle is driven entirely by local
  GC deleting the matching `CheckpointArchive`).
- Storage backends other than S3-compatible (`backend: s3` is the only
  supported value).
- A genuine multi-node download e2e in CI (kind runs a single node, so the
  syncer's download/staging path is exercised by envtest rather than a live
  cross-node kind restore).
