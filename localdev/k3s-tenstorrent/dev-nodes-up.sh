#!/usr/bin/env bash
# Provisions fake-accelerator container nodes (same as localdev/k3s-macos/dev-nodes-up.sh).
# NODE_PODMAN=sudo podman: rootless podman can't create nested containerd cgroups for pods on
# these nodes; see lib_create_node's comment in node.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
export NODE_PODMAN="sudo podman"
HOST_ADDRESS="$(lib_host_address)"
export HOST_ADDRESS

CONTEXT="${TT_CONTEXT:-k3s-tt}"
NODE="$(lib_host_node "${CONTEXT}")"

kubectl --context "${CONTEXT}" get node "${NODE}" >/dev/null 2>&1 \
  || { echo "ERROR: ${CONTEXT} has no node ${NODE} — run install.sh first" >&2; exit 2; }

NODE_COUNT="${NODE_COUNT:-4}"
NODE_CPUS="${NODE_CPUS:-8}"
NODE_MEMORY="${NODE_MEMORY:-8g}"
K3S_VERSION="$(kubectl --context "${CONTEXT}" version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"].replace("+k3s", "-k3s"))')"
IMAGE="docker.io/rancher/k3s:${K3S_VERSION}"
# H100 x2: distributed-jobs.sh needs two distinct hosts of the same accelerator type (anti-affinity).
# H200 dropped to keep 4 node slots total (other scenarios tolerate zero H200 capacity).
ACCELERATOR_TYPES=(H100:NVIDIA-H100-80GB-HBM3 H100:NVIDIA-H100-80GB-HBM3 L40:NVIDIA-L40 A100:NVIDIA-A100-80GB-PCIE)

SERVER_URL="https://${HOST_ADDRESS}:6443"
TOKEN="$(sudo cat /var/lib/rancher/k3s/server/node-token)"

echo "==> Provisioning ${NODE_COUNT} fake accelerator node(s) (${NODE_CPUS} cpu / ${NODE_MEMORY} each)..."
CPU_ALLOC_MILLI=$(( NODE_CPUS * 1000 * 80 / 100 ))
MEM_ALLOC_KI=$(( ${NODE_MEMORY%[gG]} * 1000000 * 80 / 100 ))
for i in $(seq 1 "${NODE_COUNT}"); do
  entry="${ACCELERATOR_TYPES[$(( (i - 1) % ${#ACCELERATOR_TYPES[@]} ))]}"
  type="${entry%%:*}" label="${entry#*:}" name="fake-${type,,}-${i}"

  if sudo podman inspect "${name}" &>/dev/null && ! kubectl --context "${CONTEXT}" get node "${name}" &>/dev/null; then
    lib_destroy_node "${CONTEXT}" "${name}"
  fi
  if ! sudo podman inspect "${name}" &>/dev/null; then
    lib_create_node "${CONTEXT}" "${name}" "${IMAGE}" "${SERVER_URL}" "${TOKEN}" "${NODE_CPUS}" "${NODE_MEMORY}"
  elif [[ "$(sudo podman inspect -f '{{.State.Running}}' "${name}")" != true ]]; then
    sudo podman start "${name}" >/dev/null
  fi
  lib_wait_node_ready "${CONTEXT}" "${name}"

  lib_fake_gpu "${CONTEXT}" "${name}" 8 "${label}" "${CPU_ALLOC_MILLI}" "${MEM_ALLOC_KI}"
done

echo "==> Nodes ready:"
kubectl --context "${CONTEXT}" get nodes -L nvidia.com/gpu.product
