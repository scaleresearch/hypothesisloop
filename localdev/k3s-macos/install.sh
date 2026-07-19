#!/usr/bin/env bash
# Set up a k3s cluster inside the podman machine VM (macOS) or natively (Linux)
# and expose its API via an SSH port-forward to localhost:6443.
#
# Why not k3d/kind: both fail on macOS with libkrun podman VMs due to cgroup
# v2 cpuset not being exposed to nested containers. Running k3s natively inside
# the VM avoids this entirely.
set -euo pipefail

CONTEXT_NAME="k3s-local"

# Pinned to a k3s release bundling k8s 1.36, which introduced native (alpha) gang scheduling —
# the Job controller auto-creates Workload/PodGroup objects and the in-tree scheduler admits/
# binds the whole gang atomically for parallelism==completions Indexed Jobs, which is exactly
# the shape workload_client.go:BuildJob already produces for multi-node distributed jobs. No
# control-plane code change needed to use it — just these cluster-level feature gates. See
# cluster/docs/execution-layer.md.
#
# Three gates, not two: GenericWorkload (base Workload API) and WorkloadWithJob (Job controller
# auto-creates Workload/PodGroup for qualifying Jobs) only get the objects created — the actual
# atomic gang admission/binding in kube-scheduler is gated separately by GangScheduling. Without
# it the PodGroup exists but the scheduler still admits its pods one at a time.
K3S_VERSION="v1.36.2+k3s1"
K3S_GANG_SCHEDULING_FLAGS="--kube-apiserver-arg=feature-gates=GenericWorkload=true,WorkloadWithJob=true,GangScheduling=true --kube-apiserver-arg=runtime-config=scheduling.k8s.io/v1alpha2=true --kube-controller-manager-arg=feature-gates=GenericWorkload=true,WorkloadWithJob=true,GangScheduling=true --kube-scheduler-arg=feature-gates=GenericWorkload=true,WorkloadWithJob=true,GangScheduling=true"

# This VM's root disk is shared with whatever else podman/podman-machine is running on the
# host (other unrelated dev stacks, other images) — it can sit well above kubelet's default
# 80/85% image-GC watermarks for reasons that have nothing to do with this cluster's own
# images. Without this, kubelet periodically garbage-collects any locally-imported image
# with no currently-running container (workload/robotics-workload/cluster-agent/node-agent
# between test runs) straight out from under us — see localdev/k3s-macos/add-fake-nodes.sh's identical
# override on the fake accelerator nodes for the same reason.
# Escaped \< : this value is re-parsed by at least one more shell layer downstream (the SSH
# command string on macOS, or the piped installer script's own arg handling) before it reaches
# kubelet, so an unescaped < would be consumed as shell input redirection instead of surviving
# as literal flag text.
K3S_KUBELET_IMAGE_GC_FLAGS="--kubelet-arg=eviction-hard=imagefs.available\\<1%,nodefs.available\\<1%"

# Poll until a condition succeeds or the attempt limit is reached.
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

# ---- macOS: k3s inside the podman machine VM --------------------------------
if [[ "$(uname)" == "Darwin" ]]; then
  if ! podman machine list --format '{{.Running}}' 2>/dev/null | grep -q "true"; then
    echo "==> Starting podman machine..."
    podman machine start
  fi

  SSH_KEY="$(podman machine inspect --format '{{.SSHConfig.IdentityPath}}')"
  SSH_PORT="$(podman machine inspect --format '{{.SSHConfig.Port}}')"

  vm() {
    ssh -i "${SSH_KEY}" -p "${SSH_PORT}" \
      -o StrictHostKeyChecking=no -o ConnectTimeout=10 \
      core@localhost "$@"
  }

  if ! vm "test -f /usr/local/bin/k3s" || ! vm "/usr/local/bin/k3s --version" 2>/dev/null | grep -q "${K3S_VERSION}"; then
    echo "==> Installing/upgrading k3s to ${K3S_VERSION}..."
    vm "sudo systemctl stop k3s" 2>/dev/null || true
    # Fedora CoreOS (the podman machine's OS) is rpm-ostree based and rejects the
    # k3s installer's SELinux RPM step (sandboxed rpm-ostree daemon), so skip it.
    # That leaves the downloaded k3s binary labelled user_tmp_t, which systemd's
    # confined k3s.service is denied from executing under enforcing SELinux —
    # relabel it to bin_t so the service can actually start. INSTALL_K3S_SKIP_START
    # keeps the installer from auto-starting the service on its own right after
    # install (with the wrong label still on the binary): letting that happen burns
    # through systemd's restart rate limit in a few seconds (5 failed "Permission
    # denied" execs), leaving the unit in a failed/rate-limited state that a later
    # plain `systemctl start` doesn't clear — so relabel first, start once, clean.
    vm "curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_SELINUX_RPM=true INSTALL_K3S_SKIP_START=true INSTALL_K3S_VERSION=${K3S_VERSION} sudo -E sh -s - --disable traefik --write-kubeconfig-mode 644 ${K3S_GANG_SCHEDULING_FLAGS} ${K3S_KUBELET_IMAGE_GC_FLAGS}"
    vm "sudo chcon -t bin_t /usr/local/bin/k3s"
  fi

  if ! vm "sudo systemctl is-active k3s >/dev/null"; then
    echo "==> Starting k3s..."
    vm "sudo systemctl reset-failed k3s" 2>/dev/null || true
    vm "sudo systemctl start k3s"
  fi

  echo "==> Waiting for k3s API..."
  wait_for 60 3 "k3s kubeconfig" vm "test -f /etc/rancher/k3s/k3s.yaml"

  pkill -f "ssh.*6443:localhost:6443" 2>/dev/null || true
  ssh -N -f -i "${SSH_KEY}" -p "${SSH_PORT}" \
    -o StrictHostKeyChecking=no -L 6443:localhost:6443 core@localhost
  sleep 1

  # Build host kubeconfig and a container-friendly copy (host.docker.internal).
  # k3s TLS covers localhost/127.0.0.1 but not host.docker.internal, so the
  # container copy skips TLS verification.
  TMPKUBE=$(mktemp)
  trap 'rm -f "${TMPKUBE}"' EXIT
  vm "sudo cat /etc/rancher/k3s/k3s.yaml" \
    | sed "s|name: default|name: ${CONTEXT_NAME}|g;
           s|cluster: default|cluster: ${CONTEXT_NAME}|g;
           s|user: default|user: ${CONTEXT_NAME}|g;
           s|current-context: default|current-context: ${CONTEXT_NAME}|g" \
    > "${TMPKUBE}"

  sed "s|https://127.0.0.1:6443|https://host.docker.internal:6443|;
       s|current-context: default|current-context: ${CONTEXT_NAME}|g" "${TMPKUBE}" \
    | python3 -c "
import sys, re
sys.stdout.write(re.sub(
  r'    certificate-authority-data: [^\n]+\n',
  '    insecure-skip-tls-verify: true\n',
  sys.stdin.read()))
" > "${HOME}/.kube/k3s-container.yaml"

  kubectl config delete-context "${CONTEXT_NAME}" 2>/dev/null || true
  kubectl config delete-cluster  "${CONTEXT_NAME}" 2>/dev/null || true
  kubectl config delete-user     "${CONTEXT_NAME}" 2>/dev/null || true
  KUBECONFIG="${HOME}/.kube/config:${TMPKUBE}" kubectl config view --flatten \
    > /tmp/kube-merged.yaml
  mv /tmp/kube-merged.yaml "${HOME}/.kube/config"

  kubectl config use-context "${CONTEXT_NAME}"

# ---- Linux: k3s native ------------------------------------------------------
else
  if ! command -v k3s &>/dev/null || ! k3s --version | grep -q "${K3S_VERSION}"; then
    echo "==> Installing/upgrading k3s to ${K3S_VERSION}..."
    sudo systemctl stop k3s 2>/dev/null || true
    curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="${K3S_VERSION}" INSTALL_K3S_EXEC="--disable traefik ${K3S_GANG_SCHEDULING_FLAGS} ${K3S_KUBELET_IMAGE_GC_FLAGS}" sh -
  fi

  mkdir -p "${HOME}/.kube"
  KUBECONFIG="${HOME}/.kube/config:/etc/rancher/k3s/k3s.yaml" \
    kubectl config view --flatten > /tmp/kube-merged.yaml
  mv /tmp/kube-merged.yaml "${HOME}/.kube/config"
  chmod 600 "${HOME}/.kube/config"
  kubectl config rename-context default "${CONTEXT_NAME}" 2>/dev/null || true
  kubectl config use-context "${CONTEXT_NAME}"

  # Build container-friendly kubeconfig (host.docker.internal instead of
  # 127.0.0.1 so compose services can reach the API server; TLS skipped because
  # the k3s cert covers 127.0.0.1 but not host.docker.internal).
  sudo cat /etc/rancher/k3s/k3s.yaml \
    | sed "s|name: default|name: ${CONTEXT_NAME}|g;
           s|cluster: default|cluster: ${CONTEXT_NAME}|g;
           s|user: default|user: ${CONTEXT_NAME}|g;
           s|current-context: default|current-context: ${CONTEXT_NAME}|g;
           s|https://127.0.0.1:6443|https://host.docker.internal:6443|" \
    | python3 -c "
import sys, re
sys.stdout.write(re.sub(
  r'    certificate-authority-data: [^\n]+\n',
  '    insecure-skip-tls-verify: true\n',
  sys.stdin.read()))
" > "${HOME}/.kube/k3s-container.yaml"
  chmod 600 "${HOME}/.kube/k3s-container.yaml"

  # Import workload images into k3s containerd (pre-built by `make images`).
  for img in openresearch-node-agent openresearch-cluster-agent openresearch-workload openresearch-robotics-workload; do
    if command -v podman &>/dev/null && podman image exists "${img}:latest" 2>/dev/null; then
      echo "==> Importing ${img} into k3s..."
      podman save "${img}:latest" | sudo k3s ctr images import -
    fi
  done
fi

# Wait for node registration then readiness (fresh installs: node object appears
# a few seconds after the API responds).
echo "==> Waiting for node..."
wait_for 40 3 "node to register" \
  kubectl --context "${CONTEXT_NAME}" get nodes
kubectl --context "${CONTEXT_NAME}" wait node --all \
  --for=condition=Ready --timeout=120s

# Patch fake accelerator capacity onto the node — there's no real accelerator hardware in local dev, so this
# just gives the (currently static-config-based) capacity accounting something to point at.
NODE=$(kubectl --context "${CONTEXT_NAME}" get nodes -o jsonpath='{.items[0].metadata.name}')
kubectl --context "${CONTEXT_NAME}" patch node "${NODE}" --subresource=status \
  --type=json -p '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"},{"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"8"}]' \
  >/dev/null

# Real accelerator nodes carry this label too (set by the NVIDIA GPU Feature Discovery add-on) —
# the control plane's node affinity requires it for every accelerator type a job can request (see
# openresearch.yaml node_label_value), so local dev must fake this label as well, not just
# the resource capacity, or every accelerator-requesting job becomes unschedulable here.
kubectl --context "${CONTEXT_NAME}" label node "${NODE}" nvidia.com/gpu.product=NVIDIA-T4 --overwrite \
  >/dev/null

# Import workload images (node-agent, workload) into k3s so the cluster-agent
# bundle installed below can actually schedule them.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$(uname)" == "Darwin" ]]; then
  # On macOS k3s runs inside the podman VM; import workload images via stdin.
  for img in openresearch-node-agent openresearch-cluster-agent openresearch-workload openresearch-robotics-workload; do
    if podman image exists "${img}:latest" 2>/dev/null; then
      echo "==> Importing ${img} into k3s..."
      podman save "${img}:latest" \
        | ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
            "sudo k3s ctr images import -"
    fi
  done
fi

echo "==> Cluster ready. Context: ${CONTEXT_NAME}"
kubectl --context "${CONTEXT_NAME}" get nodes

# Install the cluster-agent bundle (node-agent DaemonSet + cluster-agent Deployment — no
# external queueing operator) onto the freshly bootstrapped local cluster, so `make k3s-up`
# produces a fully working local dev environment in one command.
echo "==> Installing cluster-agent bundle onto local cluster..."
CLUSTER_NAME="local" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  bash "${SCRIPT_DIR}/../../cluster/infra/install.sh"

# Add extra simulated nodes labeled with different fake accelerator types, so acceptable_accelerator_types/
# node-affinity variability has more than one type to actually land on locally by default
# (see add-fake-nodes.sh for why this is separate k3s agent processes, not k3d/kind). Set
# EXTRA_NODES=0 to skip. Idempotent — safe on every `make k3s-up`, including re-runs against
# an already-provisioned cluster.
echo "==> Adding fake multi-accelerator-type nodes (EXTRA_NODES=${EXTRA_NODES:-3})..."
EXTRA_NODES="${EXTRA_NODES:-3}" bash "${SCRIPT_DIR}/add-fake-nodes.sh"
