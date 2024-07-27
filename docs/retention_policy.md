
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
    maxCheckpointsPerNamespace: 50
    maxCheckpointsPerPod: 30
    maxCheckpointsPerContainer: 10
  # Uncomment the following lines to apply container/pod/namespace specific retention policies
  # containerPolicies:
  #   - namespace: <namespace>
  #     pod: <pod_name>
  #     container: <container_name>
  #     maxCheckpoints: 5
  # podPolicies:
  #   - namespace: <namespace>
  #     pod: <pod_name>
  #     maxCheckpoints: 10
  # namespacePolicies:
  #   - namespace: <namespace>
  #     maxCheckpoints: 15` 
```
A sample configuration file is available [here](/config/samples/_v1_checkpointrestoreoperator.yaml).

## Understanding Policy Fields

-   `checkpointDirectory`: Specifies the directory where checkpoints are stored.
-   `applyPoliciesImmediately`: If set to `true`, the policies are applied immediately. If `false` (default value), they are applied after new checkpoint creation.
-   `globalPolicy`: Defines global checkpoint retention limits.
    -   `maxCheckpointsPerNamespace`: Maximum number of checkpoints per namespace.
    -   `maxCheckpointsPerPod`: Maximum number of checkpoints per pod.
    -   `maxCheckpointsPerContainer`: Maximum number of checkpoints per container.
-   `containerPolicies` (optional): Specific retention policies for containers.
    -   `namespace`: Namespace of the container.
    -   `pod`: Pod name of the container.
    -   `container`: Container name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the container.
-   `podPolicies` (optional): Specific retention policies for pods.
    -   `namespace`: Namespace of the pod.
    -   `pod`: Pod name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the pod.
-   `namespacePolicies` (optional): Specific retention policies for namespaces.
    -   `namespace`: Namespace name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the namespace.

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
