#!/usr/bin/env bash
# Runs tests/e2e/test_nvidia_hardware.py against this host's k3s-nvidia context (install.sh
# must have been run already) plus its bare-node leg. Same role as
# k3s-tenstorrent/run-e2e.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${API_URL:?API_URL must be the control-plane API base URL the e2e suite runs against}"
: "${HL_TOKEN:?HL_TOKEN must be a staff token of that control plane (make staff-token)}"

(cd "${DIR}/../../tests" && NVIDIA_K3S_CONTEXT="k3s-nvidia" uv run pytest e2e/test_nvidia_hardware.py -m hardware -v "$@")
