#!/bin/bash
# Create a single-node Kubernetes cluster with kubeadm and CRI-O on the
# local host, for end-to-end testing against the CRI-O runtime.
#
# Unlike kind, the kubelet runs directly on the host: the checkpoint
# directory is a native host path (no bind mounts, inotify works), and
# CRIU support is enabled in CRI-O so real checkpoint and restore
# operations are possible.
#
# Usage: setup_kubeadm_crio.sh <k8s-stream>
# where <k8s-stream> is a pkgs.k8s.io minor version stream, e.g. v1.30.
set -euo pipefail

K8S_STREAM="$1"
REGISTRY_PORT="${REGISTRY_PORT:-5000}"
UBUNTU_CODENAME="$(
	awk -F= '$1 == "VERSION_CODENAME" { print $2 }' /etc/os-release
)"

# kubeadm preflight requirements.
sudo swapoff -a
sudo modprobe br_netfilter
cat <<EOF | sudo tee /etc/sysctl.d/99-kubernetes.conf
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
sudo sysctl --system >/dev/null

# Kubernetes, CRI-O, and CRIU package sources for the requested minor version.
# External package servers occasionally reject requests when all matrix lanes
# fetch at the same time, so retry the downloads.
fetch() {
	curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "$1"
}
sudo mkdir -p /etc/apt/keyrings
fetch "https://pkgs.k8s.io/core:/stable:/${K8S_STREAM}/deb/Release.key" |
	sudo gpg --dearmor --batch --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${K8S_STREAM}/deb/ /" |
	sudo tee /etc/apt/sources.list.d/kubernetes.list
fetch "https://download.opensuse.org/repositories/isv:/cri-o:/stable:/${K8S_STREAM}/deb/Release.key" |
	sudo gpg --dearmor --batch --yes -o /etc/apt/keyrings/cri-o-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/cri-o-apt-keyring.gpg] https://download.opensuse.org/repositories/isv:/cri-o:/stable:/${K8S_STREAM}/deb/ /" |
	sudo tee /etc/apt/sources.list.d/cri-o.list
fetch "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x4E2A48715C45AEEC077B48169B29EEC9246B6CE2" |
	sudo gpg --dearmor --batch --yes -o /etc/apt/keyrings/criu-ppa-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/criu-ppa-apt-keyring.gpg] https://ppa.launchpadcontent.net/criu/ppa/ubuntu ${UBUNTU_CODENAME} main" |
	sudo tee /etc/apt/sources.list.d/criu-ppa.list

sudo apt-get -o Acquire::Retries=5 update
sudo apt-get -o Acquire::Retries=5 install -y cri-o kubelet kubeadm kubectl criu
sudo apt-mark hold cri-o kubelet kubeadm kubectl criu

# Enable CRIU support so the kubelet checkpoint API works for real.
sudo mkdir -p /etc/crio/crio.conf.d
cat <<EOF | sudo tee /etc/crio/crio.conf.d/10-criu.conf
[crio.runtime]
enable_criu_support = true
EOF

# The CI registry runs on localhost without TLS.
sudo mkdir -p /etc/containers/registries.conf.d
cat <<EOF | sudo tee /etc/containers/registries.conf.d/50-insecure-localhost.conf
[[registry]]
location = "localhost:${REGISTRY_PORT}"
insecure = true
EOF

sudo systemctl enable --now crio

sudo kubeadm init \
	--cri-socket unix:///var/run/crio/crio.sock \
	--pod-network-cidr 10.244.0.0/16

mkdir -p "${HOME}/.kube"
sudo cp /etc/kubernetes/admin.conf "${HOME}/.kube/config"
sudo chown "$(id -u):$(id -g)" "${HOME}/.kube/config"

# Install a pod network and allow workloads on the control-plane node.
kubectl apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
kubectl taint nodes --all node-role.kubernetes.io/control-plane- || true

kubectl wait --for=condition=Ready node --all --timeout=300s
