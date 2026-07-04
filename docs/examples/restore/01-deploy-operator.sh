#!/usr/bin/env bash
# Build the operator image, load it into the node's containerd, and deploy it.
set -e
cd "$(dirname "$0")/../../.."        # repo root
IMG=checkpoint-restore-operator:dev

docker build -t "$IMG" .
docker save "$IMG" | sudo ctr -n k8s.io images import -   # single-node containerd (for kind: kind load docker-image "$IMG")
make install                     # CRDs
make deploy IMG="$IMG"           # controller
kubectl -n checkpoint-restore-operator-system rollout status deploy/checkpoint-restore-operator-controller-manager
