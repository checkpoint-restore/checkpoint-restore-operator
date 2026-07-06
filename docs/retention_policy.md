
# Checkpoint Lifecycle Policies

## Overview

The checkpoint-restore operator allows users to configure automatic cleanup of container checkpoints in a Kubernetes cluster. The following sections provide an overview of the checkpoint retention policy and describe the available configuration options.

## Checkpoint Deletion

When deleting checkpoints, the CheckpointRestoreOperator always **removes the oldest** checkpoints first. This ensures that the most recent checkpoints are retained, allowing for the most recent state of the resource to be restored if needed.

## Checkpoint Retention Policy

To apply a retention policy, you need to create a `CheckpointRestoreOperator` resource. Below is an example configuration:
```yaml
`apiVersion: criu.org/v1
kind: CheckpointRestoreOperator
metadata:
  labels:
    app.kubernetes.io/name: checkpointrestoreoperator
    app.kubernetes.io/instance: checkpointrestoreoperator-sample
    app.kubernetes.io/part-of: checkpoint-restore-operator
    app.kubernetes.io/managed-by: kustomize
    app.kubernetes.io/created-by: checkpoint-restore-operator
  name: checkpointrestoreoperator-sample
spec:
  checkpointDirectory: /var/lib/kubelet/checkpoints
  applyPoliciesImmediately: false
  globalPolicy:
    retainOrphan: true
    maxCheckpointsPerNamespace: 50
    maxCheckpointsPerPod: 30
    maxCheckpointsPerContainer: 10
  # Uncomment the following lines to apply container/pod/namespace specific retention policies
  # containerPolicies:
  #   - namespace: <namespace>
  #     pod: <pod_name>
  #     container: <container_name>
  #     retainOrphan: false  # Set to false will delete all orphan checkpoints
  #     maxCheckpoints: 5
  #     maxCheckpointSize: 6  # Maximum size of a single checkpoint in MB
  #     maxTotalSize: 20  # Maximum total size of checkpoints for the container in MB
  # podPolicies:
  #   - namespace: <namespace>
  #     pod: <pod_name>
  #     maxCheckpoints: 10
  #     maxCheckpointSize: 8  # Maximum size of a single checkpoint in MB
  #     maxTotalSize: 50  # Maximum total size of checkpoints for the pod in MB
  # namespacePolicies:
  #   - namespace: <namespace>
  #     maxCheckpoints: 15`
```
A sample configuration file is available [here](/config/samples/_v1_checkpointrestoreoperator.yaml).

## Understanding Policy Fields

-   `checkpointDirectory`: Specifies the directory where checkpoints are stored.
-   `applyPoliciesImmediately`: If set to `true`, the policies are applied immediately. If `false` (default value), they are applied after new checkpoint creation.
-   `globalPolicy`: Defines global checkpoint retention limits.
    -   `retainOrphan`: If set to `true` (default), orphan checkpoints (checkpoints whose associated resources have been deleted) will be retained. If set to `false`, orphan checkpoints will be automatically deleted. This is particularly useful for transient checkpoints used to recover from errors by replacing 'container restart' with 'container restore'.
    -   `maxCheckpointsPerNamespace`: Maximum number of checkpoints per namespace.
    -   `maxCheckpointsPerPod`: Maximum number of checkpoints per pod.
    -   `maxCheckpointsPerContainer`: Maximum number of checkpoints per container.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSizePerNamespace`: Maximum total size of checkpoints per namespace in MB.
    -   `maxTotalSizePerPod`: Maximum total size of checkpoints per pod in MB.
    -   `maxTotalSizePerContainer`: Maximum total size of checkpoints per container in MB.
-   `containerPolicies` (optional): Specific retention policies for containers.
    -   `namespace`: Namespace of the container.
    -   `pod`: Pod name of the container.
    -   `container`: Container name.
    -   `retainOrphan`: If set to `true` (default), orphan checkpoints for this container will be retained. If set to `false`, orphan checkpoints will be deleted.
    -   `maxCheckpoints`: Maximum number of checkpoints for the container.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the container in MB.
-   `podPolicies` (optional): Specific retention policies for pods.
    -   `namespace`: Namespace of the pod.
    -   `pod`: Pod name.
    -   `retainOrphan`: If set to `true` (default), orphan checkpoints for this pod will be retained. If set to `false`, orphan checkpoints will be deleted.
    -   `maxCheckpoints`: Maximum number of checkpoints for the pod.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the pod in MB.
-   `namespacePolicies` (optional): Specific retention policies for namespaces.
    -   `namespace`: Namespace name.
    -   `retainOrphan`: If set to `true` (default), orphan checkpoints for this namespace will be retained. If set to `false`, orphan checkpoints will be deleted.
    -   `maxCheckpoints`: Maximum number of checkpoints for the namespace.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the namespace in MB.

## Policy Hierarchy and Application

The CheckpointRestoreOperator uses a hierarchical approach to apply retention policies. Policies can be defined at different levels of specificity:

> [!NOTE]
> The retention policies are applied in the following order, with the most specific policy taking the highest priority:
> Container Policy :arrow_right: Pod Policy :arrow_right: Namespace Policy :arrow_right: Global Policy

### Container Policy
Applies to a specific container within a specific pod and namespace. If a container policy is defined, it is the most specific and takes the highest priority, overriding pod, namespace, and global policies for that specific container.

### Pod Policy
Applies to all containers within a specific pod. If a pod policy is defined, it overrides both the namespace and global policies for that specific pod, unless a container policy is also defined.

### Namespace Policy
Applies to all pods and containers within a specific namespace. If a namespace policy is defined, it overrides the global policy for that specific namespace, unless pod or container policies are also defined.

### Global Policy
Applies to all namespaces, pods, and containers if no more specific policy is defined. If no other policies are defined, the global policy will be applied. In the example above, the global policy limits checkpoints to 50 per namespace, 30 per pod, and 10 per container.

### Example

If a pod has a defined pod policy, but one of its containers has a defined container policy, the container policy will take precedence for that container. The pod policy will apply to the remaining containers within the pod.

## Pinning Individual Checkpoints

To prevent a specific checkpoint archive from being deleted by retention policies, create a `.keep`
marker file next to the `.tar` archive. The file must have the same name as the archive with `.keep`
appended:

```
/var/lib/kubelet/checkpoints/
  checkpoint-mypod_default-mycontainer-2026-06-12T10:00:00Z.tar
  checkpoint-mypod_default-mycontainer-2026-06-12T10:00:00Z.tar.keep
```

### Creating a marker file

The operator only checks whether the marker file exists:

```bash
touch /var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<container>-<timestamp>.tar.keep
```

The file can be empty, or store any notes (e.g., why or when the checkpoint was pinned).

### How pinned checkpoints interact with retention policies

Pinned checkpoints count toward all retention limits (`maxCheckpoints`, `maxTotalSize`,
`maxCheckpointSize`), but are never deleted by the GC, even if they individually exceed
`maxCheckpointSize` or contribute to a `maxTotalSize` breach.

For the count (`maxCheckpoints`) and total size (`maxTotalSize`) limits, if the limit still cannot
be met after deleting every unpinned candidate, the operator logs a message identifying the
remaining pinned archives by name. Retention is re-evaluated on every checkpoint write and pod
event. To avoid flooding, the log this message is logged only when the set of blocking archives
changes for a given scope and limit, not on every evaluation.

A pinned archive that individually exceeds `maxCheckpointSize` is retained and logged
(`checkpoint is pinned, skipping deletion`), but does not trigger the named-archive message above.

### Operator-managed markers (PodRestore)

The marker described above is the manual case: a file you create and remove yourself. The
operator also creates markers on its own while a `PodRestore` is active, and these have a
structured content that lets several restores share one archive safely.

An operator-managed marker is a small JSON document (shown formatted here; on disk it is a
single compact line):

```json
{
  "managedBy": "checkpoint-restore-operator/podrestore",
  "podRestoreOwners": ["default/redis-restore", "default/redis-restore-2"]
}
```

Each owner is the `namespace/name` of a `PodRestore`. The `podRestoreOwners` list works like a
reference count. When the `PodRestore` controller reconciles a live restore, it adds that
restore to the list; when a restore is deleted, its cleanup removes just its own entry. The
archive stays pinned as long as any owner remains, and the marker file is deleted only once the
last owner is gone, so two restores pointing at the same checkpoint cannot unpin it out from
under each other.

The `managedBy` field is what separates these markers from manual ones. The operator only
touches a marker whose `managedBy` matches its own identifier; a marker it cannot parse as its
own (an empty file, free-text notes, or a different `managedBy`) is treated as a manual pin and
left untouched. Manual pins and restore pins therefore never interfere.

Managed markers are written atomically: the new contents are written to a `.keep.tmp` file next
to the archive and then renamed onto `.keep`. This is why the `.keep` file only ever moves from
one complete state to the next, and why a crash midway through an update cannot leave a
half-written marker that strands the archive pinned or drops another restore's ownership. You
may occasionally see a `.keep.tmp` file during an update; the GC ignores it, and it is not a
checkpoint archive.

For retention, a managed marker is just a `.keep` file: the GC only checks that the marker
exists and never reads its content, so a managed pin counts toward the limits and is protected
from deletion exactly like a manual one (see above).

### Unpinning a checkpoint

Delete the `.keep` file to unpin a checkpoint:

```bash
rm /var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<container>-<timestamp>.tar.keep
```

The checkpoint becomes eligible for deletion on the **next GC cycle** (not immediately).
The GC does not cache pinning state between cycles.

This is the way to remove a manual pin. Removing an operator-managed marker (see above) by hand
is not durable: while the owning `PodRestore` is still active the controller re-creates the
marker on its next reconcile. To release a restore's pin, delete the `PodRestore`.
