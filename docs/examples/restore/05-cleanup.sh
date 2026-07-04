#!/usr/bin/env bash
# Delete what this example created and revert the kubelet (the operator stays).
set -e
cd "$(dirname "$0")/../../.."        # repo root
KCFG=/var/lib/kubelet/config.yaml

kubectl delete podrestore cr-demo-restore --ignore-not-found
kubectl delete checkpointschedule cr-demo --ignore-not-found
kubectl delete pod cr-demo cr-demo-restore --ignore-not-found

# Point the kubelet back at the real runtime, then remove the proxy.
if [ -f "$KCFG.bak-crrestore" ]; then
  sudo mv "$KCFG.bak-crrestore" "$KCFG"
  sudo systemctl restart kubelet
fi
sudo systemctl disable --now cri-restore-proxy.service 2>/dev/null || true
sudo systemctl disable --now cri-restore-proxy.socket 2>/dev/null || true
sudo rm -f /etc/systemd/system/cri-restore-proxy.service
sudo rm -f /etc/systemd/system/cri-restore-proxy.socket
sudo rm -f /etc/systemd/system/kubelet.service.d/20-cri-restore-proxy.conf
sudo rm -f /etc/default/cri-restore-proxy
sudo rm -f /usr/local/bin/cri-proxy
sudo systemctl daemon-reload
kubectl delete -f deploy/cri-proxy/daemonset.yaml --ignore-not-found
