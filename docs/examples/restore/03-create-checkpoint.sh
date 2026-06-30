#!/usr/bin/env bash
# Run a demo pod and have the operator checkpoint it via a CheckpointSchedule.
set -e

# Demo workload. The label app=cr-demo is what the schedule selects.
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: cr-demo
  labels: { app: cr-demo }
spec:
  restartPolicy: Never
  containers:
    - name: cr-demo
      image: busybox
      command: ["sh", "-c", "i=0; while true; do i=$((i+1)); echo count=$i; sleep 1; done"]
EOF
kubectl wait --for=condition=Ready pod/cr-demo --timeout=60s

# Checkpoint on demand: a schedule that fires when the pod is annotated.
kubectl apply -f - <<'EOF'
apiVersion: criu.org/v1
kind: CheckpointSchedule
metadata:
  name: cr-demo
spec:
  namespace: default
  selector: { matchLabels: { app: cr-demo } }
  triggers: { onAnnotation: true }
EOF
kubectl annotate pod cr-demo checkpoint.criu.org/trigger=true --overwrite

# Wait for the archive (the operator polls ~30s), then print it.
# The checkpoints dir is root-owned, so list with sudo and grep (not a glob).
echo "waiting for the checkpoint..."
for _ in $(seq 30); do
  sudo ls /var/lib/kubelet/checkpoints/ | grep -q 'checkpoint-cr-demo_default-cr-demo-' && break
  sleep 5
done
sudo ls -t /var/lib/kubelet/checkpoints/ | grep -m1 'checkpoint-cr-demo_default-cr-demo-'
