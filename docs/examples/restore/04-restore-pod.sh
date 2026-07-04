#!/usr/bin/env bash
# Restore the demo pod from its checkpoint with a PodRestore.
set -e

# Newest cr-demo checkpoint (root-owned dir: list with sudo, no glob).
CKPT=/var/lib/kubelet/checkpoints/$(sudo ls -t /var/lib/kubelet/checkpoints/ | grep -m1 'checkpoint-cr-demo_default-cr-demo-')
NODE=$(kubectl get pod cr-demo -o jsonpath='{.spec.nodeName}')
echo "restoring $CKPT on node $NODE"

kubectl apply -f - <<EOF
apiVersion: criu.org/v1
kind: PodRestore
metadata:
  name: cr-demo-restore
spec:
  targetNode: "$NODE"
  checkpoints:
    - container: cr-demo
      path: "$CKPT"
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: cr-demo        # image is taken from the checkpoint when omitted
EOF

kubectl wait --for=condition=Ready podrestore/cr-demo-restore --timeout=120s
kubectl get podrestore cr-demo-restore

# With the CRI proxy installed, the counter resumes from the checkpointed value
# (not from 1), proving the process state was restored.
echo "restored pod log (count continues from the checkpoint):"
kubectl logs cr-demo-restore --tail=3
