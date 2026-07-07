# Forensic Snapshot Capture with ForensicSnapshotChain

The `ForensicSnapshotChain` custom resource allows the operator to capture a sequence of container checkpoints over time for forensic analysis. It selects a set of pods and containers and repeatedly creates checkpoints until a configured limit is reached.

Checkpoints are taken through the kubelet checkpoint API, which the operator reaches through the Kubernetes API server's node proxy. The resulting checkpoint archives are written by the kubelet to `/var/lib/kubelet/checkpoints` on the node running the checkpointed pod.

## Prerequisites

- Kubernetes 1.30 or newer, due to the availability of the checkpoint API

- A container runtime with checkpoint support enabled

- CRIU installed on every node

- The Checkpoint Restore Operator deployed in the cluster


## How to?

Create a `ForensicSnapshotChain` resource:

```yaml
apiVersion: criu.org/v1
kind: ForensicSnapshotChain
metadata:
  name: webapp-incident-2026-06-04
  namespace: default
spec:
  namespace: default
  selector:
    matchLabels:
      app: webapp
  containerNames:
    - frontend
  capture:
    interval: 5s
    maxSnapshots: 30
    maxDuration: 5m
  integrity:
    hashAlgorithm: sha256
```

The operator will repeatedly create checkpoints of the selected container every five seconds until either:

- 30 capture rounds have been completed, or

- 5 minutes have elapsed since the capture run started.

Once the resource reaches `Completed`, the configured `postSnapshotAction` runs exactly once. But note, the evidence is preserved.

When an integrity spec is set, the operator forms a forensic chain of snapshots in which each snapshot carries its own hash and a link to the previous snapshot's hash. Only `sha256` is currently supported.

## Spec Reference

The field documentation is also available from the cluster with:

```bash
kubectl explain forensicsnapshotchain.spec
```

### Pod Selection

- `namespace` (string): namespace in which pods are selected.

- `selector` (LabelSelector): selects target pods by label.

- `containerNames` ([]string): restricts checkpointing to specific containers. Empty means all containers in matching pods.


### Capture Configuration

- `interval` (duration): time between checkpoints. Expressed as a duration string such as `5s`, `1m`, or `1h`. If omitted, the operator uses a default interval.

- `maxSnapshots` (int): maximum number of capture rounds to perform before completing the capture run. Each round checkpoints every selected container of every matching pod once.

- `maxDuration` (duration): maximum time the snapshot run is allowed to continue before completing.

At least one of `maxSnapshots` or `maxDuration` must be set; this is enforced by the API server so that every capture run is guaranteed to terminate rather than checkpoint indefinitely.

Empty rounds (where the selector matched no running pods) do not count toward `maxSnapshots`. When `maxDuration` is set, those empty rounds are bounded by the time backstop. When only `maxSnapshots` is set, a capture run whose selector never matches would otherwise requeue forever, so it instead completes once `maxSnapshots` rounds have been *attempted* (with reason `NoMatchingPods`). This keeps the termination guarantee even for a mis-targeted selector.

The same guarantee covers a target that matches pods but cannot be checkpointed. Individual checkpoint errors are retried (see [Failed](#failed)), but when only `maxSnapshots` is set, a run of consecutive failed rounds eventually moves the resource to `Failed` rather than retrying indefinitely. When `maxDuration` is set, retries continue until the time backstop elapses.


### Integrity

- `hashAlgorithm` (string): algorithm used to hash each checkpoint and link it into the forensic snapshot chain. Only `sha256` is supported; leave empty to disable hashing.

- Hashes are computed via a short-lived helper pod on the target node with read-only access to `/var/lib/kubelet/checkpoints`.

- Each entry in `snapshotChainRecords` includes `sha256Hash`, `checkpointPath`, and `previousSHA256Hash` (a link to the prior snapshot's hash).

- The `IntegrityVerified` condition reports hash success or failure.

- Hash failures do not fail the chain: capture continues and only the condition reflects the problem.


## Snapshot Lifecycle

A `ForensicSnapshotChain` progresses through the following phases:

```
Pending
   |
   v
Running
   |
   v
Completed
   |
   v
Postsnapshot action (optional)
```

or

```
Pending
   |
   v
Running
   |
   v
Failed
```

### Pending

The resource has been created and is waiting to begin execution.

### Running

The operator is actively creating checkpoints according to the configured capture settings.

### Completed

The capture run has successfully finished because one of the configured completion conditions was reached:

- `maxSnapshots` reached

- `maxDuration` reached

- the selector matched no pods for `maxSnapshots` attempted rounds and no `maxDuration` was set (reason `NoMatchingPods`)

### Post-Snapshot Action

- `postSnapshotAction` (string): an action to execute once, after the snapshot run reaches a terminal state (`Completed`).
  This does not run after each individual snapshot, only once after the entire capture run finishes.

  Supported values:
  - `None` (default): no action is taken.
  - `DeletePod`: deletes all matching pods after the capture run completes. If the
    pods are managed by a Deployment, ReplicaSet, or similar controller,
    Kubernetes will automatically create replacement pods.

  This is intended for incident-response workflows where forensic evidence
  must be preserved via checkpointing before the compromised workload is
  terminated as a containment measure.

  The action runs **at most once** and only after completion is durably recorded
  as `Completed`, so evidence is never destroyed before completion is
  persisted. The trade-off is that it is best-effort: if the operator restarts
  in the window between recording `Completed` and issuing the deletes, the
  deletes are not retried (a `Completed` resource does no further work). Do not
  rely on `DeletePod` as the sole containment mechanism for a compromised
  workload; pair it with a network policy or admission control.

### Failed

A terminal phase for unrecoverable errors. A *single* checkpoint error (for
example a transient kubelet checkpoint API failure) does **not** move the resource
to `Failed`: it is treated as transient, the latest error is recorded in
`status.errorMessage` and a `Ready=False` condition with reason
`CheckpointError`, and the reconcile is retried with backoff. This keeps a
forensic capture alive across a brief glitch, and `status.failureCount` tracks
the current run of consecutive failures (it resets to zero after any round that
does not fail).

The resource moves to `Failed` (reason `CheckpointFailed`) only when the target is
persistently un-checkpointable **and** no `maxDuration` is configured: after a
fixed number of consecutive failed rounds it gives up rather than retrying
forever. When `maxDuration` is set, that time backstop governs instead and the
capture run keeps retrying failures until it elapses, so set `maxDuration` if you want
retries bounded by time rather than by failure count.

An unsupported `integrity.hashAlgorithm` is also terminal: the resource moves to
`Failed` immediately, before any checkpoint is attempted. `postSnapshotAction`
does not run on a `Failed` resource.

## Observing

View the current status:

```bash
kubectl get forensicsnapshotchain <name> -o yaml
```

Example:

```yaml
status:
  phase: Completed
  snapshotCount: 3
  startTime: "2026-06-07T06:45:27Z"
  completionTime: "2026-06-07T06:50:27Z"
```

`snapshotCount` is the number of capture rounds completed. Rounds in which no pods matched the selector are not counted. `attemptCount` (also in status) counts every round attempted, including empty ones, and is what bounds a `maxSnapshots`-only capture run whose selector never matches.

The status conditions indicate whether the resource is Pending, Running, Completed, or Failed.

### Status durability

The resource tracks all of its progress (phase, `startTime`, `snapshotCount`,
`attemptCount`, `failureCount`) in `status`, which is also what drives the phase
state machine. If `status` is lost while the resource is still running, for example
through an etcd restore or a backup/restore tool that resets the status
subresource, the controller treats the object as new: it restarts from
`Pending`, captures from scratch, and resets the `maxDuration` clock. This can
produce duplicate checkpoints. Complete or delete in-flight resources before
performing such operations.

## Security considerations

`spec.namespace` is an arbitrary namespace and is independent of the namespace
the `ForensicSnapshotChain` object itself lives in. Combined with
`postSnapshotAction: DeletePod`, this means a resource can checkpoint and delete
pods in a different namespace from where it was created. The operator's
ServiceAccount holds cluster-wide `pods` `get/list/delete` permission to make
this work. Restrict who may create `ForensicSnapshotChain` objects accordingly,
and prefer admission policy to constrain `spec.namespace` if cross-namespace
targeting is not desired in your cluster.
