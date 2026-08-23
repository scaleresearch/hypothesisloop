#!/usr/bin/env bash
# Runs tests/e2e/test_nvidia_hardware.py against this host's k3s-nvidia context (install.sh
# must have been run already) plus its bare-node leg. Same role as
# k3s-tenstorrent-qb2/run-e2e.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

(cd "${DIR}/../../tests" && NVIDIA_K3S_CONTEXT="k3s-nvidia" uv run pytest e2e/test_nvidia_hardware.py -m hardware -v "$@")
