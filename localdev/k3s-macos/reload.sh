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
fi

# Native host (and, on macOS, the fake worker containers, which podman lists either way).
# This block used to sit inside the Darwin branch above, so on a Linux host the whole
# image-import step was skipped in silence: reload printed "Reload complete" having synced
# nothing, every agent kept running the previous image, and the first symptom was the control
# plane rejecting a stale agent's reports minutes later.

# One image per archive, deliberately. `podman save a b c -o one.tar` writes a multi-image
# docker-archive whose tags all collapse onto a single digest when containerd imports it — so
# every tag resolved to whichever image happened to win, job pods ran the cluster-agent binary and
# died with "NODE_NAME is required", nothing ever reported a metric, and settlement then failed for
# want of a metrics table. Three unrelated-looking symptoms, one archive.

# A native k3s server keeps its own containerd store, and the cluster-agent runs there — so
# the server needs the images too, not only the fake worker containers.
if command -v k3s >/dev/null 2>&1; then
  echo "==> Importing images into the k3s server..."
  SERVER_TARBALL="/tmp/reload-images-server.tar"
  for img in "${IMAGES[@]}"; do
    podman save "localhost/${img}:latest" -o "${SERVER_TARBALL}"
    sudo k3s ctr images import "${SERVER_TARBALL}" >/dev/null
  done
  rm -f "${SERVER_TARBALL}"
  echo "    k3s server: synced"
fi

# Only fake nodes that actually exist right now — install.sh/dev-nodes-up.sh own
# creating them; this script's job is just keeping whatever's already running in sync.
#
# Probed under BOTH rootless and rootful podman, and this is not defensive plumbing: the nodes
# are created rootful on a native Linux host, so a rootless-only probe found nothing, took the
# "no nodes to sync" path, and left every agent running the previous image while reporting a
# successful reload. The failure then surfaced minutes later as the control plane rejecting the
# stale agent's reports — a symptom that says nothing about its cause.
# (`mapfile`/`readarray` isn't available under macOS's stock bash 3.2, hence the loop.)
PODMAN_CMD="podman"
FAKE_NODES=()
while IFS= read -r node; do
  [[ -n "${node}" ]] && FAKE_NODES+=("${node}")
done < <(podman ps --format '{{.Names}}' --filter name=fake-)
if [[ "${#FAKE_NODES[@]}" -eq 0 ]]; then
  while IFS= read -r node; do
    [[ -n "${node}" ]] && FAKE_NODES+=("${node}")
  done < <(sudo podman ps --format '{{.Names}}' --filter name=fake- 2>/dev/null)
  [[ "${#FAKE_NODES[@]}" -gt 0 ]] && PODMAN_CMD="sudo podman"
fi

# A fake node registered with k3s but with no container behind it under either podman is a
# broken host, not an empty one — and silently syncing nothing is exactly how the whole reload
# came to be a no-op. Say so instead.
REGISTERED_FAKE=$(kubectl get nodes -o name 2>/dev/null | grep -c 'node/fake-' || true)
if [[ "${#FAKE_NODES[@]}" -eq 0 && "${REGISTERED_FAKE}" -gt 0 ]]; then
  echo "reload: k3s has ${REGISTERED_FAKE} fake node(s) but no matching container under rootless or rootful podman." >&2
  echo "  Their kubelets would keep running the previous image. Refusing to report a successful reload." >&2
  exit 1
fi

if [[ "${#FAKE_NODES[@]}" -gt 0 ]]; then
  echo "==> Importing images into ${#FAKE_NODES[@]} fake node container(s) (${PODMAN_CMD})..."
  TARBALL="/tmp/reload-images.tar"
  for node in "${FAKE_NODES[@]}"; do
    for img in "${IMAGES[@]}"; do
      podman save "localhost/${img}:latest" -o "${TARBALL}"
      ${PODMAN_CMD} cp "${TARBALL}" "${node}:/tmp/reload-image.tar"
      # Explicit --address: bare `ctr` (and `k3s ctr`) default to the container's own
      # containerd socket, not the k3s-embedded one the kubelet actually reads from.
      ${PODMAN_CMD} exec "${node}" ctr --address /run/k3s/containerd/containerd.sock -n k8s.io images import /tmp/reload-image.tar >/dev/null
    done
    ${PODMAN_CMD} exec "${node}" rm -f /tmp/reload-image.tar
    echo "    ${node}: synced"
  done
  rm -f "${TARBALL}"
fi


echo "==> Recreating control-plane containers..."
bash "${SCRIPT_DIR}/../../controlplane/infra/podman.sh" reload >/dev/null
wait_for 20 1 "control-service to accept connections" \
curl -sf -o /dev/null "http://localhost:8081/health"

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
