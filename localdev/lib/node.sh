#!/usr/bin/env bash
# Generic, cross-platform k3s node lifecycle primitives, sourced by the per-platform
# dev-nodes-{up,down}.sh scripts.
#
#   - lib_create_node / lib_destroy_node: spin up a brand-new container node.
#   - lib_attach_node / lib_detach_node: taint/untaint an already-existing node (tt-quietbox).

LIB_NODE_TAINT_KEY="hypothesisloop.io/no-workload"

# NODE_PODMAN (default: podman) runs the node container itself — override to "sudo podman" on
# hosts where rootless podman can't create containerd's own nested cgroups for pods.
# --kubelet-arg=cgroups-per-qos=false (+enforce-node-allocatable=) skips cgroup subtrees these
# fake nodes don't need, since they only advertise capacity, not enforce real QoS limits.
# Usage: lib_create_node <context> <name> <image> <server_url> <token> <cpus> <memory> [network] [ip]
lib_create_node() {
  local context="$1" name="$2" image="$3" server_url="$4" token="$5" cpus="$6" memory="$7"
  local network="${8:-}" ip="${9:-}"
  local podman_cmd="${NODE_PODMAN:-podman}"

  local net_args=()
  [[ -n "$network" ]] && net_args+=(--network "$network")
  [[ -n "$ip" ]] && net_args+=(--ip "$ip")

  ${podman_cmd} run -d --name "$name" --hostname "$name" \
    --privileged --cgroupns=host \
    --cpus="$cpus" --memory="$memory" \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /var/run \
    "${net_args[@]}" \
    -e K3S_URL="$server_url" -e K3S_TOKEN="$token" -e K3S_NODE_NAME="$name" \
    "$image" agent --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%" \
    --kubelet-arg=cgroups-per-qos=false --kubelet-arg=enforce-node-allocatable= \
    >/dev/null

  lib_wait_node_ready "$context" "$name"
}

# Removes the container, its Node object, and its stale node-password Secret (else a later
# container reusing the same name gets rejected: "Node password rejected, duplicate hostname").
lib_destroy_node() {
  local context="$1" name="$2"
  local podman_cmd="${NODE_PODMAN:-podman}"
  if ${podman_cmd} container exists "$name"; then
    ${podman_cmd} rm -f "$name" >/dev/null
  else
    local rc=$?
    (( rc == 1 )) || return "$rc"
  fi
  [[ "${DESTROYING_CLUSTER:-0}" == 1 ]] && return
  kubectl --context "$context" delete node "$name" --ignore-not-found &>/dev/null
  kubectl --context "$context" -n kube-system delete secret "${name}.node-password.k3s" --ignore-not-found &>/dev/null
}

# lib_stop_node powers a node off: cordon, drain, stop the container, then remove the Node
# object. dev-nodes-up.sh brings it back (it recreates a container whose Node object is gone).
#
# Removing the Node object is the point, not an afterthought. A stopped node otherwise lingers in
# the API as NotReady while still advertising its full Allocatable, so anything reading capacity
# keeps counting hardware that is switched off — the fake accelerator nodes here would go on
# advertising GPUs that were never real to begin with.
#
# The container is stopped rather than restarted in place because a k3s agent cannot resume after
# its address changes: it comes back up and immediately shuts down with "failed to find interface
# with specified node ip". Recreating is the only reliable way back, so power-on is a rebuild.
lib_stop_node() {
  local context="$1" name="$2"
  local podman_cmd="${NODE_PODMAN:-podman}"
  if kubectl --context "$context" get node "$name" &>/dev/null; then
    kubectl --context "$context" cordon "$name" &>/dev/null
    kubectl --context "$context" drain "$name" --ignore-daemonsets --delete-emptydir-data \
      --force --timeout=60s &>/dev/null || true
  fi
  if ${podman_cmd} container exists "$name"; then
    ${podman_cmd} stop "$name" >/dev/null
  fi
  kubectl --context "$context" delete node "$name" --ignore-not-found &>/dev/null
  kubectl --context "$context" -n kube-system delete secret "${name}.node-password.k3s" --ignore-not-found &>/dev/null
}

# lib_wait_node_ready polls until <name> registers and reaches Ready.
lib_wait_node_ready() {
  local context="$1" name="$2" timeout="${3:-120}"
  for _ in $(seq 1 30); do
    kubectl --context "$context" get "node/$name" &>/dev/null && break
    sleep 2
  done
  kubectl --context "$context" wait "node/$name" --for=condition=Ready --timeout="${timeout}s" >/dev/null
}

# Imports prebuilt images into node <name>'s own containerd (always via plain podman: images
# built by `make images` live in the rootless store, not a separate rootful NODE_PODMAN one).
lib_import_images() {
  local name="$1"; shift
  local img
  # A node that was just started (rather than freshly created) reaches Ready slightly before its
  # containerd socket is accepting connections; without this the first import races and fails.
  for _ in $(seq 1 30); do
    ${NODE_PODMAN:-podman} exec "$name" test -S /run/k3s/containerd/containerd.sock &>/dev/null && break
    sleep 2
  done
  for img in "$@"; do
    podman image inspect "localhost/${img}:latest" &>/dev/null || continue
    podman save "localhost/${img}:latest" | ${NODE_PODMAN:-podman} exec -i "$name" \
      ctr --address /run/k3s/containerd/containerd.sock -n k8s.io images import - >/dev/null
  done
}

# lib_attach_node makes an already-registered node (typically the control-plane's own host)
# schedulable by removing the default no-workload taint. Idempotent.
lib_attach_node() {
  local context="$1" name="$2"
  kubectl --context "$context" taint node "$name" "${LIB_NODE_TAINT_KEY}:NoSchedule-" &>/dev/null || true
}

# lib_detach_node re-applies the no-workload taint, returning a node to "control-plane only, no
# capacity donated to workloads." Idempotent.
lib_detach_node() {
  local context="$1" name="$2"
  kubectl --context "$context" taint node "$name" "${LIB_NODE_TAINT_KEY}:NoSchedule" --overwrite &>/dev/null
}
