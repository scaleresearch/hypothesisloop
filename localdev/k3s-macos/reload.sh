#!/usr/bin/env bash
# Rebuilds every image from current source and pushes the result into every place that
# caches one: podman's local store, the k3s server's containerd, and each dev/test node
# container's own containerd (each is a separate container with its own containerd, so a
# `make images` alone never reaches them — see dev-nodes-up.sh's comment on this). Then
# force-recreates the control-plane containers and bounces the cluster-agent/node-agent
# pods so they actually pull the freshly-imported image instead of running stale code.
#
# Use this after any Go/Dockerfile change, in place of manually chaining `make images` +
# ad hoc `podman exec ... ctr images import` + pod deletes — every wait below polls for the
# actual condition instead of sleeping a fixed guess, so this finishes as soon as the
# cluster is actually ready rather than after some worst-case timeout.
set -euo pipefail

CONTEXT_NAME="k3s-local"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGES=(hypothesisloop-node-agent hypothesisloop-cluster-agent hypothesisloop-workload )

wait_for() {
  local max="$1" delay="$2" desc="$3"; shift 3
  for i in $(seq 1 "${max}"); do
    if "$@" &>/dev/null; then return 0; fi
    if [[ "${i}" -eq "${max}" ]]; then
      echo "ERROR: timed out waiting for ${desc}"; exit 1
    fi
    sleep "${delay}"
  done
}

echo "==> Rebuilding all images..."
(cd "${SCRIPT_DIR}/../.." && make images)

if [[ "$(uname)" == "Darwin" ]]; then
  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
  vm() { ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost "$@"; }

  echo "==> Importing images into k3s server..."
  for img in "${IMAGES[@]}"; do
    if podman image exists "localhost/${img}:latest" 2>/dev/null; then
      podman save "localhost/${img}:latest" | vm "sudo k3s ctr images import -"
    fi
  done

  # Only fake nodes that actually exist right now — install.sh/dev-nodes-up.sh own
  # creating them; this script's job is just keeping whatever's already running in sync.
  # (`mapfile`/`readarray` isn't available under macOS's stock bash 3.2, hence the loop.)
  FAKE_NODES=()
  while IFS= read -r node; do
    [[ -n "${node}" ]] && FAKE_NODES+=("${node}")
  done < <(podman ps --format '{{.Names}}' --filter name=fake-)
  if [[ "${#FAKE_NODES[@]}" -gt 0 ]]; then
    echo "==> Importing images into ${#FAKE_NODES[@]} fake node container(s)..."
    for node in "${FAKE_NODES[@]}"; do
      TARBALL="/tmp/reload-images-${node}.tar"
      podman save "${IMAGES[@]/#/localhost/}" -o "${TARBALL}"
      podman cp "${TARBALL}" "${node}:/tmp/reload-images.tar"
      # Explicit --address: bare `ctr` (and `k3s ctr`) default to the container's own
      # containerd socket, not the k3s-embedded one the kubelet actually reads from.
      podman exec "${node}" ctr --address /run/k3s/containerd/containerd.sock -n k8s.io images import /tmp/reload-images.tar >/dev/null
      podman exec "${node}" rm -f /tmp/reload-images.tar
      rm -f "${TARBALL}"
      echo "    ${node}: synced"
    done
  fi
fi

echo "==> Recreating control-plane containers..."
bash "${SCRIPT_DIR}/../../controlplane/infra/podman.sh" reload >/dev/null
wait_for 20 1 "control-service to accept connections" \
  curl -sf -o /dev/null "http://localhost:8082/experiments"

if kubectl --context "${CONTEXT_NAME}" get deploy/hypothesisloop-cluster-agent -n hypothesisloop &>/dev/null; then
  echo "==> Restarting cluster-agent/node-agent pods..."
  kubectl --context "${CONTEXT_NAME}" -n hypothesisloop delete pods --all >/dev/null
  wait_for 30 2 "cluster-agent pod ready" \
    kubectl --context "${CONTEXT_NAME}" -n hypothesisloop wait --for=condition=Ready \
      pod -l app=hypothesisloop-cluster-agent --timeout=1s
  wait_for 30 2 "node-agent pods ready" \
    kubectl --context "${CONTEXT_NAME}" -n hypothesisloop wait --for=condition=Ready \
      pod -l app=hypothesisloop-node-agent --timeout=1s
fi

echo "==> Reload complete."
kubectl --context "${CONTEXT_NAME}" -n hypothesisloop get pods 2>/dev/null || true
