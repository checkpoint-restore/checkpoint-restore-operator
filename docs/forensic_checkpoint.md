# Forensic Snapshot Chains CR with ForensicSnapshotChain

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

- 30 checkpoints have been created, or
    
- 5 minutes have elapsed since the chain started.
    

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
    
- `maxSnapshots` (int): maximum number of checkpoints to create before completing the chain.
    
- `maxDuration` (duration): maximum time the snapshot chain is allowed to run before completing.
    

### Integrity

- `hashAlgorithm` (string): reserved for future integrity verification support. Yet to be added.
    

## Snapshot Chain Lifecycle

A `ForensicSnapshotChain` progresses through the following phases:

```text
Pending
   ↓
Running
   ↓
Completed
```

or

```text
Pending
   ↓
Running
   ↓
Failed
```

### Pending

The chain has been created and is waiting to begin execution.

### Running

The operator is actively creating checkpoints according to the configured capture settings.

### Completed

The chain has successfully finished because one of the configured completion conditions was reached:

- `maxSnapshots` reached
    
- `maxDuration` reached
    

### Failed

The operator encountered an error while creating checkpoints.

## Observing

View the current status:

```bash
kubectl get forensicsnapshotchain <name> -o yaml
```

Example:

```yaml
status:
  phase: Completed
  snapshotCount: 30
  startTime: "2026-06-07T06:45:27Z"
  completionTime: "2026-06-07T06:50:27Z"
```

The status conditions indicate whether the chain is Pending, Running, Completed, or Failed.

## Considerations

The current implementation creates full checkpoints for every capture operation.

The following features are planned for future releases:

- Integrity verification and hash validation
    
- Incremental checkpointing
    
- Checkpoint chain verification and analysis workflows