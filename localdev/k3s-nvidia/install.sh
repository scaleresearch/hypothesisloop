#!/usr/bin/env bash
# Bootstraps a k3s cluster on this host with real NVIDIA GPU access, so the platform's
# k3s/device-plugin path (runtime/k8s/internal/k8sexec) can be tested against real hardware —
# the NVIDIA counterpart to localdev/k3s-tenstorrent-qb2/install.sh. Run on an actual host with
# an NVIDIA GPU (a vast.ai KVM VM, a bare-metal box — anything with nvidia-smi and a driver),
# not a dev laptop.
#
# Stack installed:
#   - nvidia-container-toolkit, docker default-runtime=nvidia (plain --device is NOT enough —
#     see runtime/bare-metal/internal/podexec/container.go's DeviceRequests comment).
#   - k3s, native systemd service, --docker (so pods get the nvidia runtime docker was just
#     configured with).
#   - NVIDIA device plugin (advertises the nvidia.com/gpu extended resource) + GPU Feature
#     Discovery (labels the node nvidia.com/gpu.product=...). GFD's own DaemonSet only writes
#     labels to a local file for real Node Feature Discovery to merge in — NFD isn't installed
#     here, so this script applies GFD's output to the Node object directly instead of pulling
#     in all of NFD for one label.
#   - cluster-agent bundle (runtime/k8s/infra/install.sh).
#
# Confirmed working end-to-end against a real RTX 4090 (see tests/scenarios/nvidia-hardware.sh's
# k3s leg): 164+ TFLOPS fp16 matmul via a real k8s-scheduled pod, real device-plugin GPU
# passthrough, real metrics.
#
# Required env vars (no defaults — this platform always runs the control plane on a different
# host than the GPU node itself, so there's no "just use localhost" fallback):
#   CONTROLPLANE_URL, REGISTRY_URL, METRICS_URL — reachable from this host (see
#   localdev/tunnels/ if the control plane is local and this is a remote rented box).
#
# Idempotent: safe to re-run.
set -euo pipefail

CONTEXT_NAME="k3s-nvidia"
CLUSTER_NAME="k3s-nvidia"

: "${CONTROLPLANE_URL:?set CONTROLPLANE_URL (e.g. a localdev/tunnels/ URL reaching :8082)}"
: "${REGISTRY_URL:?set REGISTRY_URL (e.g. a localdev/tunnels/ URL reaching :8083)}"
: "${METRICS_URL:?set METRICS_URL (e.g. a localdev/tunnels/ URL reaching :8084)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

wait_for() {
  local max="$1" delay="$2" desc="$3"; shift 3
  for i in $(seq 1 "${max}"); do
    if "$@" &>/dev/null; then return 0; fi
    [[ "${i}" -eq "${max}" ]] && { echo "ERROR: timed out waiting for ${desc}"; exit 1; }
    sleep "${delay}"
  done
}

if [[ "$(uname)" != "Linux" ]]; then
  echo "ERROR: this installs k3s natively against a real NVIDIA GPU — Linux only."
  exit 1
fi

echo "==> Checking for an NVIDIA GPU..."
command -v nvidia-smi >/dev/null 2>&1 || { echo "ERROR: nvidia-smi not found — no NVIDIA driver on this host."; exit 1; }
nvidia-smi -L || { echo "ERROR: nvidia-smi found no GPU."; exit 1; }

echo "==> Installing nvidia-container-toolkit..."
if ! command -v nvidia-ctk >/dev/null 2>&1; then
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    | tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit
else
  echo "    already installed."
fi

echo "==> Configuring docker for nvidia runtime..."
mkdir -p /etc/docker
python3 - <<'PYEOF'
import json, os
path = "/etc/docker/daemon.json"
d = json.load(open(path)) if os.path.exists(path) else {}
d.setdefault("runtimes", {})["nvidia"] = {"path": "nvidia-container-runtime", "args": []}
d["default-runtime"] = "nvidia"
json.dump(d, open(path, "w"), indent=2)
PYEOF
systemctl restart docker
docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L \
  || { echo "ERROR: docker still cannot pass the GPU through after configuring the nvidia runtime."; exit 1; }

echo "==> Ensuring k3s is installed and running (--docker)..."
if ! command -v k3s &>/dev/null; then
  curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--docker" sh -
else
  echo "    already installed."
  systemctl is-active --quiet k3s || systemctl start k3s
fi

mkdir -p "${HOME}/.kube"
if [[ -f "${HOME}/.kube/config" ]]; then
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-cluster default 2>/dev/null || true
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-context default 2>/dev/null || true
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-user default 2>/dev/null || true
fi
KUBECONFIG="${HOME}/.kube/config:/etc/rancher/k3s/k3s.yaml" kubectl config view --flatten > /tmp/kube-merged.yaml
mv /tmp/kube-merged.yaml "${HOME}/.kube/config"
chmod 600 "${HOME}/.kube/config"
kubectl config rename-context default "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config use-context "${CONTEXT_NAME}"

echo "==> Waiting for node..."
wait_for 40 3 "node to register" kubectl --context "${CONTEXT_NAME}" get nodes
kubectl --context "${CONTEXT_NAME}" wait node --all --for=condition=Ready --timeout=120s
NODE_NAME="$(kubectl --context "${CONTEXT_NAME}" get nodes -o jsonpath='{.items[0].metadata.name}')"

echo "==> Installing NVIDIA device plugin..."
kubectl --context "${CONTEXT_NAME}" apply -f \
  https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.0/deployments/static/nvidia-device-plugin.yml
wait_for 30 2 "device plugin ready" \
  bash -c "kubectl --context '${CONTEXT_NAME}' -n kube-system get pods -l name=nvidia-device-plugin-ds -o jsonpath='{.items[0].status.phase}' | grep -q Running"

echo "==> Installing GPU Feature Discovery..."
# GFD's own affinity requires the NFD PCI-presence label, which nothing here supplies (no NFD
# installed) — apply it directly, since it's simply true.
kubectl --context "${CONTEXT_NAME}" label node "${NODE_NAME}" feature.node.kubernetes.io/pci-10de.present=true --overwrite
kubectl --context "${CONTEXT_NAME}" -n kube-system delete daemonset gpu-feature-discovery 2>/dev/null || true
kubectl --context "${CONTEXT_NAME}" -n kube-system apply -f \
  https://raw.githubusercontent.com/NVIDIA/gpu-feature-discovery/v0.8.2/deployments/static/gpu-feature-discovery-daemonset.yaml
wait_for 30 2 "GFD pod ready" \
  bash -c "kubectl --context '${CONTEXT_NAME}' -n kube-system get pods -l app.kubernetes.io/name=gpu-feature-discovery -o jsonpath='{.items[0].status.phase}' | grep -q Running"

echo "==> Applying GFD's discovered labels to the Node object (see script header: no NFD to merge them automatically)..."
GFD_POD="$(kubectl --context "${CONTEXT_NAME}" -n kube-system get pods -l app.kubernetes.io/name=gpu-feature-discovery -o jsonpath='{.items[0].metadata.name}')"
wait_for 15 2 "GFD output file" \
  kubectl --context "${CONTEXT_NAME}" -n kube-system exec "${GFD_POD}" -- test -f /etc/kubernetes/node-feature-discovery/features.d/gfd
GFD_LABELS="$(kubectl --context "${CONTEXT_NAME}" -n kube-system exec "${GFD_POD}" -- cat /etc/kubernetes/node-feature-discovery/features.d/gfd)"
# Applied one at a time: some GFD-emitted values (e.g. gpu.machine, which can contain
# "(", ")", ",") aren't valid k8s label values — skip those rather than letting one bad
# label abort every other (useful) one, including gpu.product itself.
while IFS='=' read -r k v; do
  [[ -z "$k" ]] && continue
  kubectl --context "${CONTEXT_NAME}" label node "${NODE_NAME}" "${k}=${v}" --overwrite 2>/dev/null \
    || echo "    skipping invalid label ${k}=${v}"
done <<< "${GFD_LABELS}"
GPU_PRODUCT="$(kubectl --context "${CONTEXT_NAME}" get node "${NODE_NAME}" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')"
echo "    node labeled nvidia.com/gpu.product=${GPU_PRODUCT}"

grep -qFi "nvidia.com/gpu.product=${GPU_PRODUCT}" "${ROOT}/controlplane/settings/hypothesisloop.yaml" \
  || echo "    WARNING: ${GPU_PRODUCT} has no acch_rate in controlplane/settings/hypothesisloop.yaml — add one or jobs of this type can never be admitted (matching is case-insensitive, see domain.AcceleratorType.MatchesLabels, so exact casing here doesn't matter)."

echo "==> Building cluster-agent/node-agent images (this host, --docker)..."
for img in cluster-agent node-agent; do
  docker build -f "${ROOT}/runtime/k8s/build/Dockerfile.${img}" -t "localhost/hypothesisloop-${img}:latest" "${ROOT}" >/dev/null
done

echo "==> Installing cluster-agent bundle (control plane: ${CONTROLPLANE_URL})..."
CLUSTER_NAME="${CLUSTER_NAME}" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  CONTROLPLANE_URL="${CONTROLPLANE_URL}" REGISTRY_URL="${REGISTRY_URL}" METRICS_URL="${METRICS_URL}" \
  bash "${ROOT}/runtime/k8s/infra/install.sh"

echo "==> Cluster ready. Context: ${CONTEXT_NAME}  Cluster name: ${CLUSTER_NAME}"
echo
bash "${SCRIPT_DIR}/status.sh"
