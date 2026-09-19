#!/usr/bin/env bash
# Full teardown: uninstalls k3s, which takes the tt-operator Helm release down with it (see
# comment below). Does NOT touch the host's tt-kmd install (apt/DKMS-managed, outside k3s's
# control — see install.sh's comment on why tt-k8s-driver-manager is disabled) or the
# Tenstorrent hardware/firmware in any way.
set -euo pipefail

CONTEXT_NAME="k3s-tt"

DESTROYING_CLUSTER=1 bash "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/dev-nodes-down.sh"

# No explicit `helm uninstall` here: the tt-operator release (like every other cluster object)
# lives entirely in k3s's own datastore (/var/lib/rancher/k3s), which k3s-uninstall.sh below
# deletes wholesale. tt-k8s-driver-manager — the one chart component that would touch anything
# outside k3s (host kernel driver) — is disabled in install.sh, so there's nothing this chart
# leaves behind on the host to separately clean up.
echo "==> Uninstalling k3s..."
sudo /usr/local/bin/k3s-uninstall.sh 2>/dev/null || true

kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
# rename-context (in install.sh) only renames the context, not the cluster/user objects
# underneath — those are still literally named "default", so deleting them under CONTEXT_NAME
# would silently match nothing. Delete the names that actually exist.
kubectl config delete-cluster default 2>/dev/null || true
kubectl config delete-user    default 2>/dev/null || true

echo "==> Tenstorrent cluster destroyed. Host tt-kmd install is untouched."
