#!/usr/bin/env bash
# Provisions the schedulable nodes for the local k3s-local cluster: on macOS/laptop dev these
# are the *only* form of worker capacity there is (the podman-VM node itself stays tainted
# no-workload — see install.sh), so the same nodes serve both interactive dev and the portable
# e2e suite. Each is its own container (own kubelet/containerd/CNI netns — see localdev/lib/node.sh's
# doc comment for why not a bare `k3s agent` process sharing the VM's netns).
#
# Sized to run the portable suite's concurrent scenarios fast rather than merely fit: default
# NODE_COUNT=4/NODE_CPUS=4/NODE_MEMORY=6g comfortably covers the portable pytest suite's default
# parallel batch (tests/e2e, `pytest -m "not exclusive and not slow and not hardware"`). Override
# via env for a smaller laptop.
#
# Idempotent: safe to re-run against an already-provisioned cluster (running node containers
# are left alone, stopped ones are rebuilt). Tear down with dev-nodes-down.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${DIR}/../lib/node.sh"

HOST_ADDRESS="$(cat "${DIR}/../.host-address" 2>/dev/null || true)"
: "${HOST_ADDRESS:?run \`make render-settings\` (or \`make controlplane-up\`) first -- ${DIR}/../.host-address is missing}"
export HOST_ADDRESS

CONTEXT_NAME="k3s-local"
NODE_COUNT="${NODE_COUNT:-4}"
NODE_CPUS="${NODE_CPUS:-8}"
NODE_MEMORY="${NODE_MEMORY:-8g}"
K3S_VERSION="$(kubectl --context "${CONTEXT_NAME}" version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"].replace("+k3s", "-k3s"))')"
IMAGE="docker.io/rancher/k3s:${K3S_VERSION}"
# H100 x2: distributed-jobs.sh needs two distinct hosts of the same accelerator type (anti-affinity).
# H200 dropped to keep 4 node slots total. Kept in sync with k3s-tenstorrent's dev-nodes-up.sh.
ACCELERATOR_TYPES=(H100:NVIDIA-H100-80GB-HBM3 H100:NVIDIA-H100-80GB-HBM3 L40:NVIDIA-L40 A100:NVIDIA-A100-80GB-PCIE)

if [[ "$(uname)" == "Darwin" ]]; then
  # host.containers.internal resolves to the Mac under rootful podman machine, not the VM the
  # server actually runs in — reach it over the VM's own address instead.
  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
  vm() { ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no -o ConnectTimeout=10 core@localhost "$@"; }
  SERVER_IP="$(vm "ip -4 addr show eth0" | awk '/inet /{print $2}' | cut -d/ -f1)"
  TOKEN="$(vm "sudo cat /var/lib/rancher/k3s/server/node-token")"
else
  SERVER_IP="127.0.0.1"
  TOKEN="$(sudo cat /var/lib/rancher/k3s/server/node-token)"
fi
SERVER_URL="https://${SERVER_IP}:6443"

lib_pin_system_pods "${CONTEXT_NAME}"

echo "==> Provisioning ${NODE_COUNT} dev/test node(s) (${NODE_CPUS} cpu / ${NODE_MEMORY} each)..."
CPU_ALLOC_MILLI=$(( NODE_CPUS * 1000 * 80 / 100 ))
MEM_ALLOC_KI=$(( ${NODE_MEMORY%[gG]} * 1000000 * 80 / 100 ))
for i in $(seq 1 "${NODE_COUNT}"); do
  entry="${ACCELERATOR_TYPES[$(( (i - 1) % ${#ACCELERATOR_TYPES[@]} ))]}"
  # Suffixed by index (not just the type name) so NODE_COUNT beyond the 4 listed types never
  # collides two nodes onto the same container name.
  # Lowercased via tr: ${type,,} needs bash 4, and macOS ships 3.2.
  type="${entry%%:*}" label="${entry#*:}"
  name="fake-$(printf '%s' "${type}" | tr '[:upper:]' '[:lower:]')-${i}"

  # A stopped node gets a new address when started and its k3s agent then shuts down, so it is
  # rebuilt, as is one whose Node object is gone.
  if podman inspect "${name}" &>/dev/null && {
    [[ "$(podman inspect -f '{{.State.Running}}' "${name}")" != true ]] ||
      ! kubectl --context "${CONTEXT_NAME}" get node "${name}" &>/dev/null
  }; then
    lib_destroy_node "${CONTEXT_NAME}" "${name}"
  fi
  # A node whose container is gone leaves its Node and node password behind, which refuse the
  # agent of a new container by that name.
  if ! podman inspect "${name}" &>/dev/null; then
    lib_destroy_node "${CONTEXT_NAME}" "${name}"
    lib_create_node "${CONTEXT_NAME}" "${name}" "${IMAGE}" "${SERVER_URL}" "${TOKEN}" "${NODE_CPUS}" "${NODE_MEMORY}"
  fi
  lib_wait_node_ready "${CONTEXT_NAME}" "${name}"

  lib_fake_gpu "${CONTEXT_NAME}" "${name}" 8 "${label}" "${CPU_ALLOC_MILLI}" "${MEM_ALLOC_KI}"
done

echo "==> Nodes ready:"
kubectl --context "${CONTEXT_NAME}" get nodes -L nvidia.com/gpu.product
