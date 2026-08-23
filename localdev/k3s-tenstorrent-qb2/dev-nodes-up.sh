#!/usr/bin/env bash
# Provisions fake-accelerator container nodes (same as localdev/k3s-macos/dev-nodes-up.sh).
# Does NOT attach tt-quietbox as a general worker — it hosts the control plane and cluster-agent,
# and sharing it with test workloads starves those under load. tt-quietbox stays reserved for
# tests/e2e/test_tenstorrent_hardware.py, which attaches/detaches it itself.
# NODE_PODMAN=sudo podman: rootless podman can't create nested containerd cgroups for pods on
# these nodes; see lib_create_node's comment in node.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"
export NODE_PODMAN="sudo podman"

CONTEXT="${TT_CONTEXT:-k3s-tt}"
NODE="${TT_NODE:-tt-quietbox}"

kubectl --context "${CONTEXT}" get node "${NODE}" >/dev/null 2>&1 \
  || { echo "ERROR: ${CONTEXT} has no node ${NODE} — run install.sh first" >&2; exit 2; }

NODE_COUNT="${NODE_COUNT:-4}"
NODE_CPUS="${NODE_CPUS:-8}"
NODE_MEMORY="${NODE_MEMORY:-8g}"
K3S_VERSION="$(kubectl --context "${CONTEXT}" version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"].replace("+k3s", "-k3s"))')"
IMAGE="docker.io/rancher/k3s:${K3S_VERSION}"
# H100 x2: distributed-jobs.sh needs two distinct hosts of the same flavor (anti-affinity).
# H200 dropped to keep 4 node slots total (other scenarios tolerate zero H200 capacity).
ACCELERATOR_TYPES=(H100:NVIDIA-H100-80GB-HBM3 H100:NVIDIA-H100-80GB-HBM3 L40:NVIDIA-L40 A100:NVIDIA-A100-80GB-PCIe)

HOST_IP="$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if ($i=="src") print $(i+1); exit}')"
SERVER_URL="https://${HOST_IP}:6443"
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

  # node-agent/cluster-agent/workload all run with imagePullPolicy: Never for local dev, so each
  # node's own containerd needs the image pushed in directly -- the registries.yaml mirror
  # lib_create_node wrote only matters to pods that actually pull. Run `make images` beforehand so
  # the local store has current builds to import. This list is every image the e2e suite's own
  # scenarios reference (not every hypothesisloop-* image on the host -- some of those are
  # multi-GB experiment workloads unrelated to e2e and would blow up import time/disk per node).
  lib_import_images "${name}" \
    localhost/hypothesisloop-node-agent:latest \
    localhost/hypothesisloop-cluster-agent:latest \
    localhost/hypothesisloop-workload:latest
  bash "${DIR}/../lib/fake-gpu-node.sh" "${CONTEXT}" "${name}" 8 "${label}" "${CPU_ALLOC_MILLI}" "${MEM_ALLOC_KI}"
done

echo "==> Nodes ready:"
kubectl --context "${CONTEXT}" get nodes -L nvidia.com/gpu.product
