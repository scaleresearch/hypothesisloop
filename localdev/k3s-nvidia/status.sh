#!/usr/bin/env bash
# Quick health check for the stack installed by install.sh: node readiness, GPU labels, device
# plugin/GFD pods, and the extended-resource capacity actually advertised. Read-only.
set -euo pipefail

CONTEXT_NAME="k3s-nvidia"
kctl() { kubectl --context "${CONTEXT_NAME}" "$@"; }

if ! kctl get nodes &>/dev/null; then
  echo "ERROR: cluster context '${CONTEXT_NAME}' is not reachable — run localdev/k3s-nvidia/install.sh first."
  exit 1
fi

echo "==> Nodes:"
kctl get nodes -L nvidia.com/gpu.product,nvidia.com/gpu.count,nvidia.com/gpu.memory

echo
echo "==> Extended resource capacity:"
kctl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'

echo
echo "==> Device plugin / GFD pods (kube-system):"
kctl -n kube-system get pods -l 'name=nvidia-device-plugin-ds' 2>/dev/null
kctl -n kube-system get pods -l 'app.kubernetes.io/name=gpu-feature-discovery' 2>/dev/null

echo
echo "==> cluster-agent bundle (hypothesisloop namespace):"
kctl -n hypothesisloop get deploy,ds 2>/dev/null || echo "    (none found — did install.sh's cluster-agent step run?)"

echo
echo "==> Host-side nvidia-smi:"
nvidia-smi -L 2>/dev/null || echo "    nvidia-smi not found on this host."
