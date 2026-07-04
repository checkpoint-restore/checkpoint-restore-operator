# Checkpoint/restore example

Runnable scripts showing the full flow. Run them in order, on a cluster node.

```
./01-deploy-operator.sh     # build + install + deploy the operator
./02-install-cri-proxy.sh   # install the host CRI proxy and point kubelet at it
./03-create-checkpoint.sh   # demo pod creates a CheckpointSchedule and checkpoint .tar
./04-restore-pod.sh         # PodRestore resumes the pod from the checkpoint
./05-cleanup.sh             # delete the demo, revert the kubelet, remove the proxy
```

`02` is what makes a restore actually resume the process: it points the kubelet
at the host-managed CRI proxy so a `CreateContainer` for a checkpoint pod is
restored via CRIU instead of started fresh. See [../../restore.md](../../restore.md).

> **Warning:** `02` reconfigures and restarts the kubelet. On a single-node or
> control-plane cluster a failure disrupts the node. The script backs up the
> kubelet config and rolls back automatically if the node does not return Ready,
> and `05` reverts it. Prefer a disposable/canary node.

Requirements: a checkpoint-capable runtime (containerd 2.x or CRI-O) with CRIU
on the node, the `ContainerCheckpoint` feature gate (default since Kubernetes
1.30), and `kubectl`/`docker`/`curl`/`sudo`. The scripts assume the `default`
namespace, the kubelet checkpoint dir `/var/lib/kubelet/checkpoints`, and
containerd; edit them if yours differ.
