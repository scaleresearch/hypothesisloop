#!/usr/bin/env bash
# Generic, cross-platform k3s node lifecycle primitives and wait_for, sourced by the localdev
# scripts.
#
#   - lib_create_node / lib_destroy_node: spin up a brand-new container node.
#   - lib_attach_node / lib_detach_node: taint/untaint an already-existing node (a hardware host's).
#   - lib_fake_gpu: advertise fake nvidia.com/gpu capacity on a registered node.

LIB_NODE_TAINT_KEY="hypothesisloop.io/no-workload"
LIB_NODE_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
. "${LIB_NODE_REPO}/localdev/lib/clusters.sh"

# lib_host_node <context>: the node of the hardware host the kube context reaches: the node field
# of the OWNED_COMPUTE_CLUSTERS entry at that context (lib_owned_cluster_at), else the cluster's
# k3s server node, which a host's install.sh makes the host itself.
lib_host_node() {
  local cluster node
  cluster="$(lib_owned_cluster_at "$1")" || return 1
  node="$(lib_cluster_field node "${cluster}")"
  if [[ -z "${node}" ]]; then
    node="$(kubectl --context "$1" get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  fi
  printf '%s' "${node}"
}

# Polls until a command succeeds; exits the calling script when the attempts run out.
# Usage: wait_for <max_attempts> <sleep_secs> <description> <command...>
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

# NODE_PODMAN (default: podman) runs the node container itself — override to "sudo podman" on
# hosts where rootless podman can't create containerd's own nested cgroups for pods.
# --kubelet-arg=cgroups-per-qos=false (+enforce-node-allocatable=) skips cgroup subtrees these
# fake nodes don't need, since they only advertise capacity, not enforce real QoS limits.
# Usage: lib_create_node <context> <name> <image> <server_url> <token> <cpus> <memory> [network] [ip]
#
# HOST_ADDRESS (required, env) is the one address every node pulls images through — the
# lib_host_address `make render-settings` writes for object_store.endpoint. k3s reads
# registries.yaml at agent-process start, not on a later reload, so it must exist inside the
# container's filesystem before `agent` runs. It is copied in between create and start: a bind
# mount of a host temp file disappears with it once removed (macOS virtiofs), and the node then
# pulls over HTTPS.
lib_create_node() {
  local context="$1" name="$2" image="$3" server_url="$4" token="$5" cpus="$6" memory="$7"
  local network="${8:-}" ip="${9:-}"
  local podman_cmd="${NODE_PODMAN:-podman}"

  : "${HOST_ADDRESS:?lib_create_node: HOST_ADDRESS must be set (see Makefile render-settings target)}"
  local config_dir
  config_dir="$(mktemp -d)"
  mkdir -p "${config_dir}/rancher/k3s"
  lib_registries_yaml "$HOST_ADDRESS" > "${config_dir}/rancher/k3s/registries.yaml"

  # Expanded below as ${net_args[@]+"${net_args[@]}"}: under `set -u`, macOS's bash 3.2 treats
  # an unset-because-empty array as an unbound variable.
  local net_args=()
  [[ -n "$network" ]] && net_args+=(--network "$network")
  [[ -n "$ip" ]] && net_args+=(--ip "$ip")

  ${podman_cmd} create --name "$name" --hostname "$name" \
    --privileged --cgroupns=host \
    --cpus="$cpus" --memory="$memory" \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /var/run \
    ${net_args[@]+"${net_args[@]}"} \
    -e K3S_URL="$server_url" -e K3S_TOKEN="$token" -e K3S_NODE_NAME="$name" \
    "$image" agent --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%" \
    --kubelet-arg=cgroups-per-qos=false --kubelet-arg=enforce-node-allocatable= \
    >/dev/null
  ${podman_cmd} cp "${config_dir}/rancher" "${name}:/etc/"
  rm -rf "${config_dir}"
  ${podman_cmd} start "$name" >/dev/null

  lib_wait_node_ready "$context" "$name"
}

# lib_ensure_helm installs helm on a provisioning host that lacks it (get-helm uses sudo itself
# when not root).
lib_ensure_helm() {
  command -v helm &>/dev/null && return
  echo "==> Installing Helm..."
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
}

# lib_host_ip prints the source address of this host's default route. The UDP connect sends no packet.
lib_host_ip() {
  python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.connect(("1.1.1.1", 80)); print(s.getsockname()[0])'
}

# lib_host_address prints the name a k3s node, job pod or container reaches this host's published
# ports on. On macOS every one of them runs in the podman machine, where gvproxy answers
# host.containers.internal with a fixed address leading to the Mac's loopback, so it holds when
# the Mac changes networks. A native Linux k3s has no such name; there it is the LAN address.
lib_host_address() {
  if [[ "$(uname)" == "Darwin" ]]; then echo host.containers.internal; else lib_host_ip; fi
}

# lib_registry_publish_ip prints the host address the registry publishes on: the loopback
# host.containers.internal leads to on macOS, the LAN address on Linux.
lib_registry_publish_ip() {
  if [[ "$(uname)" == "Darwin" ]]; then echo 127.0.0.1; else lib_host_ip; fi
}

# lib_reachable_address prints LOCAL_REACHABLE_ADDRESS from .env (.env.example), the address
# other machines and the pods of their clusters reach this host at, or nothing when it is unset.
# It is refused when it is a URL, carries a port, or names this host as only this host reaches it.
lib_reachable_address() {
  local address
  [[ -f "${LIB_NODE_REPO}/.env" ]] || return 0
  address="$(set +u; . "${LIB_NODE_REPO}/.env"; printf '%s' "${LOCAL_REACHABLE_ADDRESS:-}")"
  [[ -n "${address}" ]] || return 0
  if [[ ! "${address}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]]; then
    echo "LOCAL_REACHABLE_ADDRESS='${address}' must be an IPv4 address or a host name, with no scheme, port or path" >&2
    return 1
  fi
  case "${address}" in
    localhost | 127.* | 0.0.0.0 | host.containers.internal | host.docker.internal)
      echo "LOCAL_REACHABLE_ADDRESS='${address}' is an address only this machine reaches: name one other machines and their pods reach, such as this machine's tailnet IP" >&2
      return 1
      ;;
  esac
  printf '%s' "${address}"
}

# lib_advertised_address <host address>: the address the rendered settings hand to every cluster,
# agent and job: LOCAL_REACHABLE_ADDRESS when .env sets it, else the host address.
lib_advertised_address() {
  local reachable
  reachable="$(lib_reachable_address)" || return 1
  printf '%s' "${reachable:-$1}"
}

# lib_registry_reachable_ip prints the IPv4 address the registry publishes on beside
# lib_registry_publish_ip, so other machines pull from it at LOCAL_REACHABLE_ADDRESS. Nothing when
# that is unset or is the address the registry publishes on anyway.
lib_registry_reachable_ip() {
  local reachable ip
  reachable="$(lib_reachable_address)" || return 1
  [[ -n "${reachable}" ]] || return 0
  ip="$(python3 -c 'import socket, sys; print(socket.gethostbyname(sys.argv[1]))' "${reachable}" 2>/dev/null)" || {
    echo "LOCAL_REACHABLE_ADDRESS='${reachable}' does not resolve on this machine" >&2
    return 1
  }
  [[ "${ip}" == "$(lib_registry_publish_ip)" ]] || printf '%s' "${ip}"
}

# Prints a k3s registries.yaml that mirrors the local registry at <registry_host>:5000
# as plain HTTP with no TLS verification -- matches docker-compose.yml's registry service, which
# is dev-only and never TLS-terminated. With LOCAL_REACHABLE_ADDRESS set, the settings name the
# images under that address, and a local node pulls those through <registry_host> as well, so it
# never depends on the other network. A remote cluster trusts the registry through the chart's
# insecureRegistry (deploy/charts/hypothesisloop-cluster-agent/values.yaml).
lib_registries_yaml() {
  local registry_host="$1" reachable
  reachable="$(lib_reachable_address)" || return 1
  cat <<EOF
mirrors:
  "${registry_host}:5000":
    endpoint:
      - "http://${registry_host}:5000"
EOF
  if [[ -n "${reachable}" && "${reachable}" != "${registry_host}" ]]; then
    cat <<EOF
  "${reachable}:5000":
    endpoint:
      - "http://${registry_host}:5000"
EOF
  fi
  cat <<EOF
configs:
  "${registry_host}:5000":
    tls:
      insecure_skip_verify: true
EOF
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

# lib_wait_node_ready polls until <name> registers and reaches Ready.
lib_wait_node_ready() {
  local context="$1" name="$2" timeout="${3:-120}"
  for _ in $(seq 1 30); do
    kubectl --context "$context" get "node/$name" &>/dev/null && break
    sleep 2
  done
  kubectl --context "$context" wait "node/$name" --for=condition=Ready --timeout="${timeout}s" >/dev/null
}

# lib_fake_gpu <context> <node> <gpu-count> <product-label> [cpu-alloc-milli] [mem-alloc-ki]: patches
# the nvidia.com/gpu extended resource onto an already-registered node's status and labels its
# product. The last two cap allocatable cpu/memory to a container node's cgroup quota, since
# cAdvisor reports the host's full size. "add" on an existing key replaces it, so a rerun is safe.
lib_fake_gpu() {
  local context="$1" node="$2" count="$3" label="$4" cpu="${5:-}" mem="${6:-}"
  local patch="[{\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"${count}\"},{\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"${count}\"}"
  [[ -n "${cpu}" ]] && patch+=",{\"op\":\"add\",\"path\":\"/status/allocatable/cpu\",\"value\":\"${cpu}m\"}"
  [[ -n "${mem}" ]] && patch+=",{\"op\":\"add\",\"path\":\"/status/allocatable/memory\",\"value\":\"${mem}Ki\"}"
  patch+="]"
  kubectl --context "${context}" patch node "${node}" --subresource=status --type=json -p "${patch}" >/dev/null
  kubectl --context "${context}" label node "${node}" "nvidia.com/gpu.product=${label}" --overwrite >/dev/null
  echo "    ${node}: nvidia.com/gpu=${count} (${label})"
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
