#!/usr/bin/env bash
# Full teardown: uninstalls k3s (takes the device plugin/GFD/cluster-agent objects down with
# it, same reasoning as k3s-tenstorrent-qb2/destroy.sh). Does NOT touch the host's NVIDIA
# driver, nvidia-container-toolkit, or docker's default-runtime setting.
set -euo pipefail

CONTEXT_NAME="k3s-nvidia"

echo "==> Uninstalling k3s..."
sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true

kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config delete-cluster default 2>/dev/null || true
kubectl config delete-user default 2>/dev/null || true

echo "==> k3s-nvidia cluster destroyed. Host NVIDIA driver/toolkit untouched."
