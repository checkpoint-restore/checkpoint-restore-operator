# Automated Checkpointing with CheckpointSchedule

The `CheckpointSchedule` custom resource lets the operator create container
checkpoints automatically. It selects a set of pods and describes what should
initiate a checkpoint of their containers: a periodic interval, a pod
annotation, resource usage thresholds, or pod disruption events.

Checkpoints are taken through the [kubelet checkpoint
API](https://kubernetes.io/docs/reference/node/kubelet-checkpoint-api/), which
the operator reaches via the API server's node proxy. The resulting archives
are written by the kubelet to `/var/lib/kubelet/checkpoints` on the node
running the checkpointed pod.

## Prerequisites

* Kubernetes 1.30 or newer (the `ContainerCheckpoint` feature gate is enabled
  by default since 1.30)
* A container runtime with checkpoint support, for example CRI-O with
  `enable_criu_support = true` or containerd 2.x
* [CRIU](https://criu.org) installed on every node
* For resource-based triggers:
  [metrics-server](https://github.com/kubernetes-sigs/metrics-server)

## Quick Start

Deploy the operator and create a `CheckpointSchedule`:

```yaml
apiVersion: criu.org/v1
kind: CheckpointSchedule
metadata:
  name: myapp-checkpoints
  namespace: default
spec:
  namespace: default
  selector:
    matchLabels:
      app: myapp
  triggers:
    interval: 1h
```

Every pod in namespace `default` with the label `app=myapp` is now
checkpointed once an hour. A sample manifest using all four triggers can be
found at `config/samples/criu_v1_checkpointschedule.yaml`.

## Spec Reference

The field documentation is also available from the cluster with
`kubectl explain checkpointschedule.spec`.

* `namespace` (string): namespace in which pods are selected.
* `selector` (LabelSelector): selects the pods to checkpoint by label. Only
  pods in the `Running` phase are checkpointed.
* `containerNames` ([]string): restricts checkpointing to the named
  containers. Empty means all containers.
* `triggers` (object): what initiates a checkpoint. Multiple triggers can be
  combined.

### Triggers

* `interval` (duration): time between periodic checkpoints, expressed as a
  duration string (e.g. `30s`, `15m`, `12h`). Minimum `1s`. Note that days
  are not a supported unit; use multiples of hours instead (e.g. `24h`).
* `onAnnotation` (bool): enable on-demand checkpoints via pod annotation.
* `resourceThreshold` (object): checkpoint on resource usage relative to the
  container's limit.
* `onKubernetesEvents` ([]string): checkpoint on pod disruption signals:
  `NodeDrain`, `PodEviction`, `Preemption`.

### Resource Threshold

* `cpuPercent.upper` / `cpuPercent.lower` (int): checkpoint when CPU usage
  exceeds the upper or drops below the lower percentage of the container's
  limit.
* `memoryPercent.upper` / `memoryPercent.lower` (int): the same bounds for
  memory usage.
* `pollIntervalSeconds` (int): seconds between metrics polls. Defaults
  to `30`.

## Triggers in Detail

### Periodic Checkpoints

```yaml
triggers:
  interval: 1h
```

The matching pods are checkpointed every `interval`. The first
checkpoint is taken one interval after the `CheckpointSchedule` is created.
The time of the last checkpoint is persisted in the resource status, so the
cadence is preserved across operator restarts, and changing the interval
takes effect on the next reconcile.

### On-Demand Checkpoints

```yaml
triggers:
  onAnnotation: true
```

Annotating a matching pod requests a checkpoint of its containers:

```sh
kubectl annotate pod <pod> checkpoint.criu.org/trigger=true
```

The operator polls the matching pods every 30 seconds and removes the
annotation after the checkpoint is taken, so the pod is not checkpointed
again on the next poll.

### Resource Thresholds

```yaml
triggers:
  resourceThreshold:
    memoryPercent:
      upper: 80
    cpuPercent:
      upper: 90
    pollIntervalSeconds: 30
```

Container usage is read from the Kubernetes Metrics API and compared against
the container's resource limit. Containers without a limit are skipped, since
a percentage cannot be computed without a ceiling. A five-minute
per-container cooldown prevents checkpoint storms while a threshold stays
breached across multiple polls.

The `lower` bounds can be used to capture a container in a quiet state, for
example before scaling it down.

### Combining Triggers

All configured triggers are active at the same time and fire independently:
each one checkpoints the matching pods whenever its own condition is met, and
no trigger waits for, or is suppressed by, another. The sample manifest at
`config/samples/criu_v1_checkpointschedule.yaml` configures all four.

* All triggers share the same pod selection: `namespace`, `selector` and
  `containerNames` apply uniformly to every trigger.
* Triggers do not deduplicate against each other. If, for example, the
  periodic interval elapses while a node is being drained, the same pod can
  be checkpointed twice in short succession - once by each trigger.
* Each trigger limits repeats on its own: the interval trigger is anchored on
  `status.lastCheckpointTime`, the annotation is removed after the
  checkpoint, resource thresholds have a five-minute per-container cooldown,
  and disruption events checkpoint once per disruption.
* `status.lastCheckpointTime` and `status.checkpointsCreated` record only the
  interval trigger's activity; on-demand, threshold and event checkpoints
  appear in the operator log and in the node's checkpoint directory.

### Pod Disruption Events

```yaml
triggers:
  onKubernetesEvents:
    - NodeDrain
    - PodEviction
    - Preemption
```

The operator polls every five seconds for disruption signals and checkpoints
the affected pods once per disruption:

* `NodeDrain`: the pod's node is marked unschedulable
  (`kubectl cordon`/`kubectl drain`). All matching pods on the node are
  checkpointed once per drain cycle.
* `PodEviction`: the pod has a deletion timestamp and is about to terminate.
* `Preemption`: the pod has the `DisruptionTarget` condition. Requires
  Kubernetes 1.26 or newer.

## Observing

The resource status records the activity of the interval trigger:

```console
$ kubectl get checkpointschedule myapp-checkpoints -o yaml
...
status:
  checkpointsCreated: 2
  lastCheckpointTime: "2026-06-03T14:11:07Z"
```

The operator logs every checkpoint request with the target pod, container
and kubelet endpoint.

## Retention

Checkpoint archives accumulate on the node. Use the
`CheckpointRestoreOperator` resource to limit the number or total size of
archives per namespace, pod or container; see the
[README](../README.md#description) for the available retention policies.

## Using the Checkpoints

A checkpoint archive can be converted into an OCI image and restored into a
new pod by a checkpoint-aware runtime. See the [Forensic container
checkpointing](https://kubernetes.io/blog/2022/12/05/forensic-container-checkpointing-alpha/)
blog post and [checkpointctl](https://github.com/checkpoint-restore/checkpointctl)
for working with checkpoint archives.
