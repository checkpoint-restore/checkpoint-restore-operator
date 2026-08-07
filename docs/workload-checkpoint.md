# Checkpointing a Workload with WorkloadCheckpoint

The `WorkloadCheckpoint` custom resource checkpoints an entire Kubernetes
workload — a `Job`, `Deployment`, or `StatefulSet` — as a single, coherent
group. You name the workload; the operator finds the pods it owns and
checkpoints their containers, recording for every replica which archive
landed on which node.

The [kubelet checkpoint
API](https://kubernetes.io/docs/reference/node/kubelet-checkpoint-api/) only
understands one thing: checkpoint *one container* of *one pod* on *one node*.
It has no concept of a Deployment or Job. `WorkloadCheckpoint` is the
workload-aware front door to that low-level API — it resolves a workload to
its pods and then drives the existing per-container checkpoint for each.
Nothing about the underlying checkpoint mechanism changes.

## How it works

```text
WorkloadCheckpoint (targetRef: {kind, name})
        │  reconcile ("" -> Pending -> Running -> terminal)
        ▼
WorkloadResolver          resolve owned, Running pods via ownerReferences
   ├── Job         -> Pod
   ├── Deployment  -> ReplicaSet -> Pod
   └── StatefulSet -> Pod
        │  for each pod, for each selected container
        ▼
kubelet checkpoint API (via the API server node proxy)
        │
        ▼
/var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<container>-<ts>.tar
        │
        ▼
status.checkpoints  (per replica: pod, node, container -> archive path)
```

**Ownership resolution.** The resolver never matches pods by label. It follows
Kubernetes `ownerReferences` (the parent pointer every pod carries), matching
on UID, so it captures exactly the pods the named workload owns and nothing
else:

* `Job` — pods are owned directly by the Job.
* `Deployment` — Deployment → ReplicaSet → pods (two hops).
* `StatefulSet` — pods are owned directly by the StatefulSet.

For a Deployment mid-rollout, every ReplicaSet the Deployment controls is
included (including a stale one from a previous rollout), so the snapshot
reflects the pods that actually exist.

**One-shot.** A `WorkloadCheckpoint` captures the workload's state once, at
reconcile time, and then stops. Recurring or triggered checkpointing is the
job of [`CheckpointSchedule`](checkpoint-schedules.md).

**Not globally atomic by default.** Pods are checkpointed one after another, so
the group snapshot is *crash-consistent per pod*, not a single instant across
all replicas. Set `spec.coordinated: true` to instead rendezvous every member
at a barrier — see [Coordinated checkpointing](#coordinated-checkpointing).

## Prerequisites

* Kubernetes 1.30 or newer (the `ContainerCheckpoint` feature gate is enabled
  by default since 1.30)
* A container runtime with checkpoint support (CRIO with
  `enable_criu_support = true`, or containerd 2.x)
* [CRIU](https://criu.org) installed on every node
* The operator deployed with permission to reach the kubelet checkpoint API
  (`nodes/proxy` and `nodes/checkpoint`)

## Using WorkloadCheckpoint

```yaml
apiVersion: criu.org/v1
kind: WorkloadCheckpoint
metadata:
  name: web-snapshot
  namespace: default
spec:
  targetRef:
    kind: Deployment # Job | Deployment | StatefulSet
    name: web
  # containerNames: []        # empty = all containers of each pod
  failurePolicy: BestEffort   # BestEffort | AllOrNothing
```

Apply it, then watch it reach a terminal phase:

```console
$ kubectl apply -f web-snapshot.yaml
$ kubectl get workloadcheckpoint web-snapshot
NAME           KIND         TARGET   PHASE       PODS
web-snapshot   Deployment   web      Completed   3
```

### Spec fields

* `targetRef.kind` (required) — `Job`, `Deployment`, or `StatefulSet`.
* `targetRef.name` (required) — name of the workload, in the same namespace as
  the `WorkloadCheckpoint`. Immutable once set.
* `containerNames` (optional) — restrict checkpointing to these containers.
  Empty means all containers of each pod.
* `failurePolicy` (optional) — `BestEffort` (default) checkpoints what it can
  and reports per-pod failures; `AllOrNothing` fails the whole run if any pod
  fails.
* `coordinated` (optional, default `false`) — checkpoint all resolved members
  together at the criu-coordinator barrier instead of one after another. See
  [Coordinated checkpointing](#coordinated-checkpointing).

## Coordinated checkpointing

By default the members of a workload are checkpointed independently, one after
another. For a workload whose pods hold **open connections to each other**
(a distributed database, a message queue, tightly-coupled services), that is
not enough: if pod A is dumped seconds before pod B, an in-flight message
between them can be lost or duplicated on restore. `spec.coordinated: true`
solves this by dumping every member at the same instant, mediated by
[`criu-coordinator`](https://github.com/checkpoint-restore/criu-coordinator).

When `coordinated` is set the operator:

1. Resolves the workload's containers and builds a **full-mesh dependency map**
   (every member depends on every other), keyed by CRI container ID.
2. Delivers that map to each involved node's **node-agent**, which writes it to
   `/etc/criu/criu-coordinator.json` on the node host. The map carries the
   coordinator Service's numeric **ClusterIP** — the CRIU client runs on the
   node host and cannot resolve cluster DNS.
3. Fires all members **concurrently**. Each member's CRIU action-script blocks
   at the barrier until every peer has arrived, then the coordinator server
   releases them together.
4. Clears the node map when the run finishes.

```text
operator ──WriteMap(HTTP)──▶ node-agent ──▶ /etc/criu/criu-coordinator.json
   │                                            ▲
   └── fire all members concurrently            │ read by the action-script
              │                                  │
              ▼                                  │
      kubelet checkpoint API ─▶ CRIU ─▶ criu-coordinator client
                                                 │
                                                 ▼
                          criu-coordinator server (barrier, ClusterIP)
```

**Group-atomic.** Coordinated runs are always all-or-nothing regardless of
`failurePolicy`: if any member cannot reach the barrier the whole run fails
(`phase: Failed`, reason `CoordinatedGroupFailed`). A map-delivery failure
fails the run *before* any member is fired.

### Additional prerequisites

Coordinated checkpointing needs the components installed by the chart when
`coordinated.enabled=true`:

* the **criu-coordinator server** (a Deployment + ClusterIP Service) that acts
  as the barrier;
* the privileged **node-agent DaemonSet**, which installs the CRIU
  action-script and writes/clears the per-run dependency map on each node;
* the `criu-coordinator` binary present at `/usr/local/bin/criu-coordinator` on
  every node, built with
  [PR #26](https://github.com/checkpoint-restore/criu-coordinator/pull/26) so
  that containers not part of a group are treated as non-participants rather
  than aborting their dump.

```console
$ helm install cro charts/checkpoint-restore-operator \
    --set image.tag=<tag> \
    --set coordinated.enabled=true \
    --set coordinated.coordinator.image.tag=<tag> \
    --set coordinated.nodeAgent.image.tag=<tag>
```

### Example

```yaml
apiVersion: criu.org/v1
kind: WorkloadCheckpoint
metadata:
  name: db-snapshot
  namespace: default
spec:
  targetRef:
    kind: StatefulSet
    name: db
  coordinated: true
```

## Reading the result

The controller records the coherent group snapshot in `status`:

```yaml
status:
  phase: Completed
  resolvedPodCount: 3
  checkpointedPodCount: 3
  failedPodCount: 0
  checkpoints:
    - podName: web-7d9f-abcde
      nodeName: node-1
      replicaKey: web-7d9f-abcde
      containers:
        - container: app
          path: /var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-app-<ts>.tar
          checkpointTime: "2026-07-26T04:56:32Z"
    # ... one entry per replica
```

### Phases

* `Pending` — accepted; about to start.
* `Running` — resolving pods and checkpointing containers.
* `Completed` — every selected container of every resolved pod was
  checkpointed.
* `PartiallyFailed` — `BestEffort`: some pods failed; per-pod errors are in
  `status.checkpoints`.
* `Failed` — `AllOrNothing` and a pod failed, a coordinated run lost a member
  at the barrier (`reason: CoordinatedGroupFailed`), or the workload could not
  be resolved (see the `Ready` condition's `reason`/`message`).

The `checkpoints` list is written incrementally and pods already recorded are
skipped, so a controller restart mid-run does not re-checkpoint completed pods.

## Verifying a checkpoint

The archive paths in `status.checkpoints` point at real files on the node:

```console
# on the node (or, for kind, inside the node container)
$ tar tf "$ARCHIVE" | head    # pages-*.img, core-*.img, config/spec.dump
$ checkpointctl inspect "$ARCHIVE"
```

The captured process state (for example a running counter's value) lives in
the `pages-*.img` files. To confirm it end to end, restore the archive with
[`PodRestore`](restore.md): the process resumes from the checkpointed state
rather than starting over.

## Relationship to the other resources

`WorkloadCheckpoint` is purely additive and reuses the operator's existing
checkpoint machinery:

* It calls the **same kubelet checkpoint path** as
  [`CheckpointSchedule`](checkpoint-schedules.md) and
  [`ForensicSnapshotChain`](forensic_checkpoint.md); only the "which pods" step
  in front of it is new.
* The archives it produces are governed by the same
  [retention policy](retention_policy.md) as any other checkpoint.
* Its `status.checkpoints` (per replica: node + archive path) is the metadata a
  future workload-level restore would consume; restoring a single archive today
  is done with [`PodRestore`](restore.md).
