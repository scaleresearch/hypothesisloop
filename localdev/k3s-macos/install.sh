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
# host, so it can sit well above kubelet's default 80/85% image-GC watermarks for unrelated
# reasons. Without this, kubelet periodically garbage-collects any locally-imported image
# with no currently-running container straight out from under us — see
# localdev/k3s-macos/dev-nodes-up.sh's identical override on the fake accelerator nodes.
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
STAGE_T0=$(date +%s)
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

  # Build host kubeconfig and a container-friendly copy (host.containers.internal).
  # k3s TLS covers localhost/127.0.0.1 but not host.containers.internal, so the
  # container copy skips TLS verification.
  TMPKUBE=$(mktemp)
  trap 'rm -f "${TMPKUBE}"' EXIT
  vm "sudo cat /etc/rancher/k3s/k3s.yaml" \
    | sed "s|name: default|name: ${CONTEXT_NAME}|g;
           s|cluster: default|cluster: ${CONTEXT_NAME}|g;
           s|user: default|user: ${CONTEXT_NAME}|g;
           s|current-context: default|current-context: ${CONTEXT_NAME}|g" \
    > "${TMPKUBE}"

  sed "s|https://127.0.0.1:6443|https://host.containers.internal:6443|;
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

  # Build container-friendly kubeconfig (host.containers.internal instead of
  # 127.0.0.1 so compose services can reach the API server; TLS skipped because
  # the k3s cert covers 127.0.0.1 but not host.containers.internal).
  sudo cat /etc/rancher/k3s/k3s.yaml \
    | sed "s|name: default|name: ${CONTEXT_NAME}|g;
           s|cluster: default|cluster: ${CONTEXT_NAME}|g;
           s|user: default|user: ${CONTEXT_NAME}|g;
           s|current-context: default|current-context: ${CONTEXT_NAME}|g;
           s|https://127.0.0.1:6443|https://host.containers.internal:6443|" \
    | python3 -c "
import sys, re
sys.stdout.write(re.sub(
  r'    certificate-authority-data: [^\n]+\n',
  '    insecure-skip-tls-verify: true\n',
  sys.stdin.read()))
" > "${HOME}/.kube/k3s-container.yaml"
  chmod 600 "${HOME}/.kube/k3s-container.yaml"

  # Import workload images into k3s containerd (pre-built by `make images`).
  for img in hypothesisloop-node-agent hypothesisloop-cluster-agent hypothesisloop-workload; do
    if podman image exists "${img}:latest" 2>/dev/null; then
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
echo "==> k3s stage: $(( $(date +%s) - STAGE_T0 ))s"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../lib/node.sh"

# The control-plane node donates zero capacity to workloads by default — it never runs
# training pods, but node-agent's DaemonSet tolerates every taint
# (cluster/infra/node-agent-daemonset.yaml) so it still monitors this node. Import just that
# image here; workload/cluster-agent images land on whatever nodes dev-nodes-up.sh attaches or
# creates below.
CONTROL_PLANE_NODE="$(kubectl --context "${CONTEXT_NAME}" get nodes -o jsonpath='{.items[0].metadata.name}')"
lib_detach_node "${CONTEXT_NAME}" "${CONTROL_PLANE_NODE}"
if [[ "$(uname)" == "Darwin" ]]; then
  # On macOS k3s runs inside the podman VM; import workload images via stdin.
  if podman image exists "hypothesisloop-node-agent:latest" 2>/dev/null; then
    echo "==> Importing hypothesisloop-node-agent into k3s..."
    podman save "hypothesisloop-node-agent:latest" \
      | ssh -i "${SSH_KEY}" -p "${SSH_PORT}" -o StrictHostKeyChecking=no core@localhost \
          "sudo k3s ctr images import -"
  fi
fi

echo "==> Cluster ready. Context: ${CONTEXT_NAME}"
kubectl --context "${CONTEXT_NAME}" get nodes

# Install the cluster-agent bundle (node-agent DaemonSet + cluster-agent Deployment — no
# external queueing operator) onto the freshly bootstrapped local cluster, so `make k3s-up`
# produces a fully working local dev environment in one command.
echo "==> Installing cluster-agent bundle onto local cluster..."
STAGE_T0=$(date +%s)
CLUSTER_NAME="local" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  bash "${SCRIPT_DIR}/../../cluster/infra/install.sh"
echo "==> cluster-agent stage: $(( $(date +%s) - STAGE_T0 ))s"

# Provision the schedulable dev/test nodes (see dev-nodes-up.sh) — on macOS/laptop dev they're the
# only worker capacity there is, since the control-plane node above stays tainted no-workload.
# Set NODE_COUNT=0 to leave the cluster control-plane-only. Idempotent — safe on every
# `make k3s-up`, including re-runs against an already-provisioned cluster.
STAGE_T0=$(date +%s)
bash "${SCRIPT_DIR}/dev-nodes-up.sh"
echo "==> dev-nodes-up stage: $(( $(date +%s) - STAGE_T0 ))s"
