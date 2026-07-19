#!/usr/bin/env bash
# Pause the Tenstorrent k3s stack without tearing anything down: stops k3s
# (and with it every pod, including tt-operator's components). Cluster state,
# the Helm release, and CRDs are preserved on disk — start.sh brings it back.
# For a full teardown that removes the Helm release/cluster, use destroy.sh.
set -euo pipefail

echo "==> Stopping k3s..."
sudo systemctl stop k3s

echo "==> Tenstorrent stack stopped. Run localdev/k3s-tenstorrent-qb2/start.sh to bring it back."
