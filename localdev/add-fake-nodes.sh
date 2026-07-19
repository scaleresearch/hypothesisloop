#!/usr/bin/env bash
# Adds extra simulated k3s nodes to the local dev cluster, each labeled with a different
# fake GPU type — so acceptable_gpu_types / node-affinity variability can actually be
# exercised locally (a real multi-vendor/multi-model cluster has heterogeneous nodes; a
# fresh `make k3s-up` only has the one node the podman VM (or bare host) itself is).
#
# Each extra node is a `rancher/k3s` container running `k3s agent`, joining the same
# server as a real additional node would over the network. This used to be a plain
# `k3s agent` process sharing the VM's root network namespace with the server — cheaper,
# but every extra agent's flannel/CNI tried to manage the same global `cni0`/`flannel.1`
# interfaces (there's only one of each per network namespace), so 2+ of them fought each
# other constantly: pod sandboxes killed and recreated in a loop, agents evicted before
# they could do useful work. A container gets its own network namespace for free, so each
# fake node's CNI stack is fully isolated — no more collisions.
#
# Getting a k3s agent to actually run inside a container needs a specific incantation
# (undocumented in the failure modes, well-documented once you know it):
#   --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw
# for cgroup v2 delegation (a plain --privileged alone still 'Error: failed to find cpuset
# cgroup (v2)', and without --cgroupns=host the kubelet's cgroup mkdir gets a permission
# denied even under a rootful podman machine), plus
#   --kubelet-arg=feature-gates=KubeletInUserNamespace=true
# so the kubelet doesn't insist on /dev/kmsg (blocked under any userns remapping) for its
# OOM watcher. This is a different problem from the k3d/kind cgroup-cpuset failure
# documented in localdev/install.sh — that's specific to k3d/kind's own nested-Docker
# layering, not to running a bare k3s agent in one container.
#
# Idempotent: safe to call from install.sh on every `make k3s-up`. Re-running with the same
# EXTRA_NODES count is a no-op (checked by container name, so a VM restart that stopped the
# containers gets them recreated rather than silently skipped); raising EXTRA_NODES only
# adds the new ones; lowering it leaves the excess containers running (tear down with
# localdev/destroy.sh, or `podman rm -f` the container names printed at the end).
set -euo pipefail

CONTEXT_NAME="k3s-local"
EXTRA_NODES="${EXTRA_NODES:-3}"
# Matches localdev/install.sh's K3S_VERSION (v1.36.2+k3s1), reformatted for the Docker Hub
# tag scheme (rancher/k3s uses a hyphen before the k3s suffix, not a plus).
K3S_IMAGE_TAG="v1.36.2-k3s1"
FAKE_NODE_CPUS="${FAKE_NODE_CPUS:-2}"
FAKE_NODE_MEMORY="${FAKE_NODE_MEMORY:-2g}"

if [[ ! "${EXTRA_NODES}" =~ ^[0-9]+$ ]]; then
  echo "ERROR: EXTRA_NODES must be a non-negative integer, got '${EXTRA_NODES}'"; exit 1
fi
if [[ "${EXTRA_NODES}" -eq 0 ]]; then
  echo "==> EXTRA_NODES=0, skipping fake multi-GPU-type nodes."
  exit 0
fi

# (type, node_label_value) pairs cycled across the extra nodes. All NVIDIA here (same
# resource_name/taint_key/node_label_key as the default node) — see openresearch.yaml's
# commented MI300X entry for the AMD (different vendor plumbing) case, exercised
# separately since it needs its own resource/taint/label keys, not just a different value.
GPU_TYPES=(L40:NVIDIA-L40 A100:NVIDIA-A100-80GB-PCIe H100:NVIDIA-H100-80GB-HBM3 H200:NVIDIA-H200-141GB-HBM3)

vm() {
  ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -o ConnectTimeout=10 \
    core@localhost "$@"
}

if [[ "$(uname)" == "Darwin" ]]; then
  if ! podman machine list --format '{{.Running}}' 2>/dev/null | grep -q "true"; then
    echo "ERROR: podman machine is not running — run localdev/install.sh first."; exit 1
  fi
  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"
  # The fake-node containers reach the k3s server over the podman network, not over
  # loopback (they're not colocated with it the way the old same-netns processes were) —
  # same as how a real additional node reaches the server over the network. k3s's own
  # apiserver already listens on 0.0.0.0:6443, so the VM's own address on the podman
  # bridge's route works; host.containers.internal is not it under a rootful podman
  # machine (it resolves to the libkrun host-forwarding address, i.e. the Mac, not the VM).
  SERVER_IP="$(vm "ip -4 addr show eth0" | awk '/inet /{print $2}' | cut -d/ -f1)"
else
  SERVER_IP="127.0.0.1"
fi

if ! kubectl --context "${CONTEXT_NAME}" get nodes &>/dev/null; then
  echo "ERROR: cluster context '${CONTEXT_NAME}' is not reachable — run localdev/install.sh first."
  exit 1
fi

echo "==> Fetching k3s node token..."
TOKEN="$(vm "sudo cat /var/lib/rancher/k3s/server/node-token")"
if [[ -z "${TOKEN}" ]]; then
  echo "ERROR: could not read k3s node-token (is this host running the k3s server?)"; exit 1
fi

echo "==> Ensuring ${EXTRA_NODES} extra simulated k3s node container(s) are running..."
for i in $(seq 1 "${EXTRA_NODES}"); do
  NODE_NAME="fake-gpu-node-${i}"
  if podman container exists "${NODE_NAME}" 2>/dev/null; then
    if [[ "$(podman inspect -f '{{.State.Running}}' "${NODE_NAME}" 2>/dev/null)" == "true" ]]; then
      echo "    ${NODE_NAME}: container already running, skipping"
      continue
    fi
    echo "    ${NODE_NAME}: container exists but stopped, removing before recreate"
    podman rm -f "${NODE_NAME}" >/dev/null
  fi
  echo "    ${NODE_NAME}: starting container"
  podman run -d --name "${NODE_NAME}" --hostname "${NODE_NAME}" \
    --privileged --cgroupns=host \
    --cpus="${FAKE_NODE_CPUS}" --memory="${FAKE_NODE_MEMORY}" \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    --tmpfs /run --tmpfs /var/run \
    -e K3S_URL="https://${SERVER_IP}:6443" \
    -e K3S_TOKEN="${TOKEN}" \
    -e K3S_NODE_NAME="${NODE_NAME}" \
    "docker.io/rancher/k3s:${K3S_IMAGE_TAG}" agent \
    --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%" \
    >/dev/null
done

echo "==> Waiting for extra nodes to register..."
for i in $(seq 1 "${EXTRA_NODES}"); do
  NODE_NAME="fake-gpu-node-${i}"
  for attempt in $(seq 1 30); do
    if kubectl --context "${CONTEXT_NAME}" get node "${NODE_NAME}" &>/dev/null; then
      break
    fi
    if [[ "${attempt}" -eq 30 ]]; then
      echo "ERROR: timed out waiting for node ${NODE_NAME} to register"; exit 1
    fi
    sleep 3
  done
  kubectl --context "${CONTEXT_NAME}" wait node "${NODE_NAME}" \
    --for=condition=Ready --timeout=60s >/dev/null
done

# Each fake node is its own container with its own containerd — workload images built by
# `make images` only ever land in the podman machine's own store, not inside these nested
# containerd instances, so import them explicitly the same way install.sh does for the
# main node (just via `podman save`/`cp`/`ctr import` instead of an SSH pipe, since these
# are containers rather than the VM itself).
echo "==> Importing workload images into fake node containers..."
for i in $(seq 1 "${EXTRA_NODES}"); do
  NODE_NAME="fake-gpu-node-${i}"
  for img in openresearch-node-agent openresearch-cluster-agent openresearch-workload openresearch-robotics-workload; do
    if ! podman image exists "localhost/${img}:latest" 2>/dev/null; then
      continue
    fi
    TARBALL="/tmp/${img}-${NODE_NAME}.tar"
    podman save "localhost/${img}:latest" -o "${TARBALL}"
    podman cp "${TARBALL}" "${NODE_NAME}:/tmp/image.tar"
    # Must target k3s's embedded containerd socket explicitly — bare `ctr` (and the
    # `k3s ctr` subcommand) default to /run/containerd/containerd.sock, while the k3s
    # agent's kubelet reads images from /run/k3s/containerd/containerd.sock. Importing
    # via the wrong socket succeeds silently and leaves the kubelet permanently
    # ImagePullBackOff-ing.
    podman exec "${NODE_NAME}" ctr --address /run/k3s/containerd/containerd.sock -n k8s.io images import /tmp/image.tar >/dev/null
    rm -f "${TARBALL}"
  done
  echo "    ${NODE_NAME}: images imported"
done

echo "==> Labeling extra nodes with distinct fake GPU types (idempotent: patch/label --overwrite)..."
# The kubelet inside each fake-node container reports the *host's* full CPU/memory as node
# capacity/allocatable (cAdvisor reads the machine it's running on, not the --cpus/--memory
# cgroup quota podman applied to this specific container) — so a container hard-limited to
# FAKE_NODE_CPUS core(s) still advertises the podman VM's entire CPU count (e.g. 12) to the
# k8s scheduler. Under light load nothing catches this; run several scenarios concurrently
# (each submitting real workload pods) and the scheduler happily over-admits far past what
# the container can actually execute, so pods get severely CPU-throttled instead of properly
# queued — showing up as scheduling-tick delays, admission timeouts, and jobs missing their
# preemption/re-admission windows purely from wall-clock slowness, not real scheduler bugs.
# Patch capacity/allocatable down to what the container can truly deliver (matching the GPU
# capacity patch's own pattern below), reserving ~20% of allocatable for the kubelet/flannel/
# system pods that also run inside this same container, same as real kube-reserved sizing.
CPU_ALLOCATABLE_MILLI=$(( FAKE_NODE_CPUS * 1000 * 80 / 100 ))
MEM_CAPACITY_KI="${FAKE_NODE_MEMORY%[gG]}000000"
MEM_ALLOCATABLE_KI=$(( MEM_CAPACITY_KI * 80 / 100 ))
for i in $(seq 1 "${EXTRA_NODES}"); do
  NODE_NAME="fake-gpu-node-${i}"
  entry="${GPU_TYPES[$(( (i - 1) % ${#GPU_TYPES[@]} ))]}"
  GPU_TYPE="${entry%%:*}"
  LABEL_VALUE="${entry##*:}"

  # "add" on an existing capacity/allocatable key replaces its value (RFC 6902 semantics for
  # object members), so re-running this is safe — it doesn't error on already-patched nodes.
  kubectl --context "${CONTEXT_NAME}" patch node "${NODE_NAME}" --subresource=status \
    --type=json -p "[
      {\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"8\"},
      {\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"8\"},
      {\"op\":\"add\",\"path\":\"/status/capacity/cpu\",\"value\":\"${FAKE_NODE_CPUS}\"},
      {\"op\":\"add\",\"path\":\"/status/allocatable/cpu\",\"value\":\"${CPU_ALLOCATABLE_MILLI}m\"},
      {\"op\":\"add\",\"path\":\"/status/capacity/memory\",\"value\":\"${MEM_CAPACITY_KI}Ki\"},
      {\"op\":\"add\",\"path\":\"/status/allocatable/memory\",\"value\":\"${MEM_ALLOCATABLE_KI}Ki\"}
    ]" >/dev/null
  kubectl --context "${CONTEXT_NAME}" label node "${NODE_NAME}" \
    "nvidia.com/gpu.product=${LABEL_VALUE}" --overwrite >/dev/null
  echo "    ${NODE_NAME} -> ${GPU_TYPE} (${LABEL_VALUE}), cpu=${FAKE_NODE_CPUS} (${CPU_ALLOCATABLE_MILLI}m allocatable), mem=${MEM_CAPACITY_KI}Ki (${MEM_ALLOCATABLE_KI}Ki allocatable)"
done

echo "==> Cluster nodes:"
kubectl --context "${CONTEXT_NAME}" get nodes -L nvidia.com/gpu.product
echo "==> Done. Tear these down with localdev/destroy.sh, or"
echo "    'podman rm -f \$(podman ps -aq --filter name=fake-gpu-node)' to stop them without a full teardown."
