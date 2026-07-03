# Restoring a Pod from a checkpoint

The operator can restore a checkpointed container as an ordinary Pod, using the
container runtime's **direct `.tar` restore path** (skipping the slow conversion
of the checkpoint into an OCI image). This is implemented with two pieces:

1. A **`PodRestore` custom resource** and its controller (in the operator).
2. A per-node **CRI proxy** host service that performs the actual image rewrite.

## How it works

![PodRestore checkpoint restore flow](images/podrestore-flow.svg)

The kubelet drives its normal sequence: `RunPodSandbox`, then
`CreateContainer`, then `StartContainer`. It never knows a restore is happening.
The CRI proxy is the only component that does: it forwards every CRI call
unchanged **except** `CreateContainer`, where, if the Pod sandbox carries
`restore.criu.org/checkpoint-path.<container>` for the container being created,
it rewrites that container's image to the local `.tar` path so the runtime takes
its CRIU restore path.

Because the rewrite happens entirely at the CRI layer, this works the same for
**containerd** and **CRI-O**.

## Using `PodRestore`

```yaml
apiVersion: criu.org/v1
kind: PodRestore
metadata:
  name: redis-restore
spec:
  targetNode: worker-1                 # node that holds the checkpoint .tar
  checkpoints:
    - container: redis
      path: /var/lib/kubelet/checkpoints/checkpoint-redis_default-redis-<ts>.tar
  template:                            # the restored workload
    spec:
      containers:
        - name: redis
          # image optional: derived from the checkpoint when omitted
```

The controller pins the source checkpoint (a `.keep` marker, see
[retention_policy.md](retention_policy.md)) so it is not garbage-collected during
the restore when the archive is reachable from the operator. Cross-node, it
reports `CheckpointsPinned=False` and you must retain the archive on the target
node yourself. Progress is reported through status conditions: `Ready` is the
summary (`True` once the restored Pod runs), and its `reason` carries the detail:
`Restoring`, `RenderFailed`, `NodeNotFound`, `InvalidSpec`, `PodConflict`,
`PodFailed`, `PodMissing`, or `Restored`. `CheckpointsPinned` reports retention
pinning. There is no `phase` field; read the conditions:

```sh
kubectl get podrestore redis-restore \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
```

Notes:

- A checkpoint is per **container**; only the containers listed in `checkpoints`
  are restored. Other containers in the template start normally.
- The image only satisfies the kubelet image-pull gate; it plays no role in the
  restore. Set it explicitly if the operator cannot read the archive to discover
  the base image itself.
- The checkpoint `.tar` must already exist on `targetNode`.
- `targetNode`, `checkpoints`, and `template` are immutable. Create a new
  `PodRestore` when you need to change the restored workload.

## Deploying the CRI proxy

Install the CRI proxy as a host service, then point kubelet at the proxy socket:

```sh
make build-cri-proxy
sudo deploy/cri-proxy/install-systemd.sh
curl -fsS http://127.0.0.1:18080/readyz
```

The installer copies the proxy to `/usr/local/bin/cri-proxy`, installs
`cri-restore-proxy.service`, installs `cri-restore-proxy.socket`, and installs a
kubelet drop-in so kubelet starts after systemd has created the proxy socket on
boot. The default upstream is containerd (`/run/containerd/containerd.sock`).
For CRI-O, set `CRI_PROXY_UPSTREAM=/run/crio/crio.sock` when running the
installer, or edit `/etc/default/cri-restore-proxy`.

For kubelet to use the proxy, set its runtime endpoint to the proxy socket and
restart it on each node:

```sh
--container-runtime-endpoint=unix:///run/cr-restore-proxy/cri-proxy.sock
--image-service-endpoint=unix:///run/cr-restore-proxy/cri-proxy.sock
```

The proxy sits in the critical kubelet-to-runtime path. Roll it out to a canary
node first. Running the proxy as a normal DaemonSet is useful for development,
but is not reboot-safe once kubelet is configured to depend on the proxy socket.
The systemd deployment uses socket activation, so the kubelet can connect to the
CRI socket even if the proxy process is still starting or has just restarted;
systemd starts the service and queues the connection until the proxy accepts it.

The admission policy that reserves the restore annotations for the PodRestore
controller is part of `config/default`, so both `make deploy` and a manual
`kubectl apply --server-side -k config/default` install it. There is no separate
step to forget. (Server-side apply is required: the PodRestore CRD embeds a full
`PodTemplateSpec` and exceeds the 256 KiB limit of client-side apply's
last-applied-configuration annotation, which is also why the Makefile targets
use `--server-side`.)

## Security

The proxy speaks to the runtime socket and the restore lets a Pod run from
arbitrary checkpoint state, so:

- Treat the proxy socket as sensitive as the runtime socket (it is created
  `0600`).
- Restrict who may create `PodRestore` resources (RBAC), since a restore runs a
  frozen process image as a Pod.
- Keep the default `ValidatingAdmissionPolicy` installed. It denies direct use
  of `restore.criu.org/checkpoint-path.*` on ordinary Pods and allows the
  operator service account to create the annotated restore Pod. If you customize
  the deployment namespace, name prefix, or service account name, update the
  hardcoded username in the policy expression in
  `config/admission/restore_annotation_policy.yaml` to match (see the comment at
  the top of that file).
- Checkpoint paths are confined to a single directory, the one the kubelet
  checkpoint API writes to (`/var/lib/kubelet/checkpoints` by default). Both the
  controller (`--restore-checkpoint-dir`) and the proxy (`--checkpoint-dir`)
  enforce this independently, so a forged annotation cannot point the runtime at
  a `.tar` staged elsewhere on the node (for example through a `hostPath` or
  `emptyDir` mount). If you customize the kubelet checkpoint directory, set both
  flags to match.
- Restores are namespace-confined. The controller checks the archive filename
  against the PodRestore's namespace (a coarse gate; the kubelet naming
  convention is only prefix-checkable); the proxy performs the authoritative
  check by comparing the namespace recorded *inside* the archive by the runtime
  at checkpoint time with the namespace of the pod sandbox being created, and
  fails closed when the archive is unreadable or records no namespace. To permit
  deliberate cross-namespace restores, a cluster admin must set **both** the
  operator's `--restore-allow-cross-namespace` and the proxy's
  `--allow-cross-namespace` flags.
- The CRI proxy validates all of the above independently before handing the
  path to the runtime — it does not trust the annotation — but it cannot tell an
  authorized annotation from a forged one within the same namespace and
  directory; that is the admission policy's job. Do not rely on the proxy alone.
