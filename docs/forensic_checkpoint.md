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

Once the chain reaches Completed, the configured postSnapshotAction runs exactly once. Checkpoint archives remain on the node even when DeletePod is configured; a post-snapshot action failure is logged but does not move the chain to Failed.

If the integrity spec is specified, we can form a forensic chain of snapshots with each snapshot having a parent and a self hash. For now, sha256 is only supported
for the baseline.
    

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

- `hashAlgorithm` (string): algorithm to be used to generate and create a forensic snapshot chain for each checkpoint

- Hashes are computed via a short-lived helper pod on the target node with read-only access to /var/lib/kubelet/checkpoints

- Each entry in snapshotChainRecords includes sha256Hash, checkpointPath, and previousSHA256Hash (link to the prior snapshot’s hash)

- IntegrityVerified condition reports hash success/failure

- Hash failures do not fail the chain — capture continues; only the condition reflects the problem
    

## Snapshot Chain Lifecycle

A `ForensicSnapshotChain` progresses through the following phases:

```
Pending
   ↓
Running
   ↓
Completed
   ↓
Postsnapshot action (optional)
```

or

```
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
    
### Post-Snapshot Action

- `postSnapshotAction` (string): an action to execute once, after the snapshot chain reaches a terminal state (`Completed`). 
  This does not run after each individual snapshot only once, after the entire chain finishes.

  Supported values:
  - `None` (default): no action is taken.
  - `DeletePod`: deletes all matching pods after the chain completes. If the 
    pods are managed by a Deployment, ReplicaSet, or similar controller, 
    Kubernetes will automatically create replacement pods.

  This is intended for incident-response workflows where forensic evidence 
  must be preserved via checkpointing before the compromised workload is 
  terminated as a containment measure.

### Failed

The operator encountered an error while creating checkpoints.
For example;
- Unsupported hashAlgorithm → Failed
- Kubelet/checkpoint creation errors → Failed
- postSnapshotAction does not run on Failed

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

The status conditions indicate whether the chain is Pending, Running, Completed, or Failed.

## Considerations

The current implementation creates full checkpoints for every capture operation.

The following features are planned for future releases:

- Additional hash algorithms beyond SHA-256
    
- Incremental checkpointing
    
- Checkpoint chain verification and analysis workflows

- postSnapshot Action currently supports only DeletePod. Additional actions as per need may be in future releases.
