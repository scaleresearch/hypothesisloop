#!/usr/bin/env bash
# Runs tests/scenarios/nvidia-hardware.sh against this host's k3s-nvidia context (install.sh
# must have been run already) plus its bare-node leg. Same role as
# k3s-tenstorrent-qb2/run-e2e.sh.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

NVIDIA_K3S_CONTEXT="k3s-nvidia" RUN_HARDWARE_TESTS=1 bash "${DIR}/../../tests/run.sh" nvidia-hardware "$@"
