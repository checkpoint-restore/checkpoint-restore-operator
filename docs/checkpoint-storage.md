# Storing checkpoints in object storage

Checkpoint archives normally stay on the node that took them, so a
`PodRestore` must run on that node and a checkpoint is lost with it. With
checkpoint storage configured, every archive is also copied to S3, Google
Cloud Storage or Azure Blob Storage, and a `PodRestore` can restore it on any
node.

## Components

- The **checkpoint-store agent**, a DaemonSet, runs on every node. It uploads
  each new archive in the kubelet checkpoint directory once the file has
  stopped changing. When a `PodRestore` that targets its node names a stored
  snapshot, it downloads the archive into the checkpoint directory and sets
  the `CheckpointsDownloaded` condition.
- The **operator** creates the restore Pod only after that condition is true,
  and deletes stored snapshots beyond the configured retention.

## Configuration

Storage is set on the `CheckpointRestoreOperator`:

```yaml
apiVersion: criu.org/v1
kind: CheckpointRestoreOperator
metadata:
  name: config
spec:
  storage:
    location: s3://my-bucket/checkpoints?region=us-west-2
    maxSnapshotsPerContainer: 5
    maxSnapshotAge: 168h
```

`location` takes one of these forms:

| Provider | Location | Credentials |
|---|---|---|
| S3 or compatible | `s3://bucket/root?region=us-west-2`, plus `&endpoint=https://minio:9000` for an S3-compatible server | the AWS default chain, such as IRSA or EKS Pod Identity |
| Google Cloud Storage | `gs://bucket/root` | Application Default Credentials, such as GKE Workload Identity |
| Azure Blob Storage | `azblob://container/root?account=name` | `DefaultAzureCredential`, such as Azure Workload Identity |

Enable the agent in the chart, and give both the agent's and the manager's
service accounts access to the bucket:

```sh
helm upgrade --install cro charts/checkpoint-restore-operator \
  --set checkpointStoreAgent.enabled=true \
  --set checkpointStoreAgent.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::123:role/checkpoints \
  --set operator.serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::123:role/checkpoints
```

The agent needs to read, write, list and delete objects; the manager needs
to read, list and delete them.

## Layout

Each archive is one snapshot, filed under the workload that owned the pod:

```
<root>/<namespace>/<kind>/<workload>/<pod>/<container>/<snapshot-id>/manifest.json
<root>/<namespace>/<kind>/<workload>/<pod>/<container>/<snapshot-id>/data/<archive>.tar
```

The manifest is written last and deleted first, so a snapshot without one is
incomplete and ignored. It records the archive's size and SHA-256, which are
checked on download. An upload that fails deletes the objects it stored; one
cut off by its node dying leaves data without a manifest, which nothing reads
and a bucket lifecycle rule on the prefix can expire. The
`<root>/.../<snapshot-id>` path is the snapshot's **storage path**.

## Restoring a stored snapshot

Name the snapshot's storage path instead of an on-node `path`. `targetNode`
can be any node that runs the agent:

```yaml
apiVersion: criu.org/v1
kind: PodRestore
metadata:
  name: redis-restore
spec:
  targetNode: worker-7
  checkpoints:
    - container: redis
      storagePath: checkpoints/default/Deployment/redis/redis-7d9f-x2k/redis/20260601T120000.000000000Z-1a2b3c4d
  template:
    spec:
      containers:
        - name: redis
          image: redis:7.2 # optional: the manifest records the base image
```

A snapshot taken in another namespace is refused unless cross-namespace
restores are allowed (`operator.restore.allowCrossNamespace`). The
downloaded archive is removed from the node when the `PodRestore` is deleted.

## Retention

`maxSnapshotsPerContainer` keeps the newest snapshots of each container.
`maxSnapshotAge` deletes older ones, but never a container's newest. A
snapshot named by an existing `PodRestore` is kept regardless. With neither
limit set, nothing is deleted; a bucket lifecycle rule is the alternative.
Retention runs every ten minutes.
