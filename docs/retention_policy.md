
# Checkpoint Retention Policy Documentation

## Overview

The checkpoint retention policy in the CheckpointRestoreOperator allows users to manage and configure how checkpoints are retained and cleaned up within a Kubernetes cluster. This document provides an overview of how to configure these policies, the hierarchy of their application, and details about specific fields.

## Applying a Retention Policy

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
    maxCheckpointSize: 4  # Maximum size of a single checkpoint in MB
    maxTotalSizePerNamespace: 1000  # Maximum total size of checkpoints per namespace in MB
    maxTotalSizePerPod: 500  # Maximum total size of checkpoints per pod in MB
    maxTotalSizePerContainer: 100  # Maximum total size of checkpoints per container in MB
  # containerPolicies:
  #   - namespace: namespace
  #     pod: pod_name
  #     container: container_name
  #     maxCheckpoints: 5
  #     maxCheckpointSize: 6  # Maximum size of a single checkpoint in MB
  #     maxTotalSize: 20  # Maximum total size of checkpoints for the container in MB
  # podPolicies:
  #   - namespace: namespace
  #     pod: pod_name
  #     maxCheckpoints: 10
  #     maxCheckpointSize: 8  # Maximum size of a single checkpoint in MB
  #     maxTotalSize: 50  # Maximum total size of checkpoints for the pod in MB
  # namespacePolicies:
  #   - namespace: namespace
  #     maxCheckpoints: 15
  #     maxCheckpointSize: 10  # Maximum size of a single checkpoint in MB
  #     maxTotalSize: 200  # Maximum total size of checkpoints for the namespace in MB` 
```

A sample configuration file is available under `./config/samples/_v1_checkpointrestoreoperator.yaml`.

## Understanding Policy Fields

-   `checkpointDirectory`: Specifies the directory where checkpoints are stored.
-   `applyPoliciesImmediately`: If set to `true`, the policies are applied immediately. If `false` (default value), they are applied after new checkpoint creation.
-   `globalPolicy`: Defines global checkpoint retention limits.
    -   `maxCheckpointsPerNamespace`: Maximum number of checkpoints per namespace.
    -   `maxCheckpointsPerPod`: Maximum number of checkpoints per pod.
    -   `maxCheckpointsPerContainer`: Maximum number of checkpoints per container.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSizePerNamespace`: Maximum total size of checkpoints per namespace in MB.
    -   `maxTotalSizePerPod`: Maximum total size of checkpoints per pod in MB.
    -   `maxTotalSizePerContainer`: Maximum total size of checkpoints per container in MB.
-   `containerPolicies`: (Optional) Specific retention policies for containers.
    -   `namespace`: Namespace of the container.
    -   `pod`: Pod name of the container.
    -   `container`: Container name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the container.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the container in MB.
-   `podPolicies`: (Optional) Specific retention policies for pods.
    -   `namespace`: Namespace of the pod.
    -   `pod`: Pod name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the pod.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the pod in MB.
-   `namespacePolicies`: (Optional) Specific retention policies for namespaces.
    -   `namespace`: Namespace name.
    -   `maxCheckpoints`: Maximum number of checkpoints for the namespace.
    -   `maxCheckpointSize`: Maximum size of a single checkpoint in MB.
    -   `maxTotalSize`: Maximum total size of checkpoints for the namespace in MB.

## Policy Hierarchy and Specificity

The CheckpointRestoreOperator uses a hierarchical approach to apply retention policies. Policies can be defined at different levels of specificity:

1.  **Global Policy:** Applies to all namespaces, pods, and containers if no more specific policy is defined.
2.  **Namespace Policy:** Applies to all pods and containers within a specific namespace.
3.  **Pod Policy:** Applies to all containers within a specific pod.
4.  **Container Policy:** Applies to a specific container within a specific pod and namespace.

### Policy Application

-   **Global Policy:** If no other policies are defined, the global policy will be applied. In the example above, the global policy limits checkpoints to 50 per namespace, 30 per pod, 10 per container, with additional constraints on checkpoint size and total size.
-   **Namespace Policy:** If a namespace policy is defined, it overrides the global policy for that specific namespace.
-   **Pod Policy:** If a pod policy is defined, it overrides both the namespace and global policies for that specific pod.
-   **Container Policy:** If a container policy is defined, it is the most specific and overrides pod, namespace, and global policies for that specific container.

### Example

If a pod has a defined pod policy, but one of its containers has a defined container policy, the container policy will take precedence for that container. The pod policy will apply to the remaining containers within the pod.

## Policy Deletion Mechanism

When deleting checkpoints, the CheckpointRestoreOperator always **removes the oldest** checkpoints first. This ensures that the most recent checkpoints are retained, allowing for the most recent state of the resource to be restored if needed.

## Conclusion

By leveraging the hierarchical policy system, users can fine-tune checkpoint retention to meet their specific needs. More specific policies will always override less specific ones, ensuring that the most granular control is applied where needed.

For more details, refer to the sample configuration file and experiment with different policy combinations to see how they interact within your Kubernetes cluster.