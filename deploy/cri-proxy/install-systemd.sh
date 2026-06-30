#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"

bin_src="${BIN_SRC:-$repo_root/bin/cri-proxy}"
bin_dst="${BIN_DST:-/usr/local/bin/cri-proxy}"
unit_dst="${UNIT_DST:-/etc/systemd/system/cri-restore-proxy.service}"
socket_dst="${SOCKET_DST:-/etc/systemd/system/cri-restore-proxy.socket}"
kubelet_dropin_dir="${KUBELET_DROPIN_DIR:-/etc/systemd/system/kubelet.service.d}"
env_dst="${ENV_DST:-/etc/default/cri-restore-proxy}"
upstream="${CRI_PROXY_UPSTREAM:-/run/containerd/containerd.sock}"
listen="${CRI_PROXY_LISTEN:-/run/cr-restore-proxy/cri-proxy.sock}"
health="${CRI_PROXY_HEALTH:-127.0.0.1:18080}"

if [[ ! -x "$bin_src" ]]; then
  echo "missing $bin_src; run 'make build-cri-proxy' first" >&2
  exit 1
fi

install -d -m 0755 "$(dirname "$bin_dst")"
install -d -m 0755 "$(dirname "$unit_dst")"
install -d -m 0755 "$(dirname "$socket_dst")"
install -d -m 0755 "$(dirname "$env_dst")"
install -m 0755 "$bin_src" "$bin_dst"
install -m 0644 "$repo_root/deploy/cri-proxy/systemd/cri-restore-proxy.service" "$unit_dst"
awk -v listen="$listen" '
  /^ListenStream=/ { print "ListenStream=" listen; next }
  { print }
' "$repo_root/deploy/cri-proxy/systemd/cri-restore-proxy.socket" >"$socket_dst"
chmod 0644 "$socket_dst"
install -d -m 0755 "$kubelet_dropin_dir"
install -m 0644 "$repo_root/deploy/cri-proxy/systemd/kubelet-cri-restore-proxy.conf" \
  "$kubelet_dropin_dir/20-cri-restore-proxy.conf"

cat >"$env_dst" <<EOF
CRI_PROXY_UPSTREAM=$upstream
CRI_PROXY_LISTEN=$listen
CRI_PROXY_HEALTH=$health
EOF

systemctl daemon-reload
systemctl enable --now cri-restore-proxy.socket
systemctl enable cri-restore-proxy.service
systemctl restart cri-restore-proxy.service
systemctl is-active --quiet cri-restore-proxy.socket
systemctl is-active --quiet cri-restore-proxy.service
