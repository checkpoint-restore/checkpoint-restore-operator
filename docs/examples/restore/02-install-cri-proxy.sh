#!/usr/bin/env bash
# Install the node-side CRI proxy and point the kubelet at it, so restores
# actually resume the process via CRIU.
#
# This reconfigures and restarts the kubelet. It backs up the config and rolls
# back automatically if the node does not return Ready.
# WARNING: on a single-node / control-plane cluster, a failure disrupts the node.
set -e
cd "$(dirname "$0")/../../.."        # repo root
PROXY_SOCK=/run/cr-restore-proxy/cri-proxy.sock
KCFG=/var/lib/kubelet/config.yaml

# Build and install the proxy as a host service. It must be up BEFORE we repoint
# the kubelet, otherwise the kubelet has no runtime to talk to.
make build-cri-proxy
sudo deploy/cri-proxy/install-systemd.sh
for _ in $(seq 30); do
  if curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS http://127.0.0.1:18080/readyz >/dev/null

# Point the kubelet's runtime endpoint at the proxy socket and restart it.
sudo cp "$KCFG" "$KCFG.bak-crrestore"
set_kubelet_endpoint() {
  key=$1
  value=$2
  if sudo grep -q "^${key}:" "$KCFG"; then
    sudo sed -i "s#^${key}:.*#${key}: ${value}#" "$KCFG"
  else
    printf '%s: %s\n' "$key" "$value" | sudo tee -a "$KCFG" >/dev/null
  fi
}
set_kubelet_endpoint containerRuntimeEndpoint "unix://$PROXY_SOCK"
set_kubelet_endpoint imageServiceEndpoint "unix://$PROXY_SOCK"
sudo systemctl restart kubelet

# Wait for the node to come back Ready; roll back if it does not.
node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
for _ in $(seq 24); do
  [ "$(kubectl get node "$node" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] && ok=1 && break
  sleep 5
done
if [ "${ok:-}" != 1 ]; then
  echo "node did not become Ready -- rolling back kubelet"
  sudo mv "$KCFG.bak-crrestore" "$KCFG"
  sudo rm -f /etc/systemd/system/kubelet.service.d/20-cri-restore-proxy.conf
  sudo systemctl disable --now cri-restore-proxy.service 2>/dev/null || true
  sudo systemctl disable --now cri-restore-proxy.socket 2>/dev/null || true
  sudo systemctl daemon-reload
  sudo systemctl restart kubelet
  exit 1
fi
echo "CRI proxy installed; kubelet now routes through it"
