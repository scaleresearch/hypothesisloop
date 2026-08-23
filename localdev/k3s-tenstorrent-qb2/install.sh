#!/usr/bin/env bash
# Bootstraps a k3s cluster on this host and installs Tenstorrent's official
# tt-operator Helm stack onto it, so the cluster recognizes real Tenstorrent
# accelerator cards (via PCIe, vendor 1e52) and exposes them as schedulable
# Kubernetes resources.
#
# This is the real-hardware counterpart to localdev/k3s-macos/install.sh (which spins up
# k3s with simulated/fake accelerator nodes for laptop dev). Run this one on
# an actual Tenstorrent host — a QuietBox, a Galaxy node, anything with
# /dev/tenstorrent devices — not on a dev laptop.
#
# Stack installed (see https://docs.tenstorrent.com/tt-operator/latest/ for
# the upstream reference this mirrors):
#   - k3s itself, native systemd service (no podman/VM layer needed — this
#     script assumes it's running directly on Linux bare metal).
#   - Node Feature Discovery: labels nodes carrying a Tenstorrent PCI device.
#   - tt-dra-driver: publishes each card as a schedulable device via k8s
#     Dynamic Resource Allocation (DRA), so pods request them with a
#     ResourceClaim instead of a vendor-specific extended resource.
#   - tt-fabric-manager: resolves inter-card/inter-host fabric topology;
#     tt-dra-driver depends on it to actually publish devices.
#   - tt-telemetry: per-device health/metrics on a Prometheus endpoint.
#   - tt-k8s-driver-manager: left DISABLED. This host already has tt-kmd
#     installed via apt/DKMS (tenstorrent-dkms package) — driver-manager
#     auto-detects that as "host-managed" and idles even when enabled, but
#     disabling it outright avoids installing a controller/CRDs for a job
#     this host's package manager already does.
#   - jobset / kubepmix: left DISABLED. Both exist for multi-node distributed
#     training (JobSet lifecycle + PMIx-aware MPI wiring) and kubepmix also
#     requires cert-manager pre-installed. A single QuietBox is one node with
#     N local cards, not a multi-host job — enable both (and install
#     cert-manager first) if this host later joins a multi-node cluster.
#
# Idempotent: safe to re-run. k3s install is skipped if the pinned version is
# already active; the Helm release is reconciled with `helm upgrade --install`
# every time, so re-running after editing this file's --set flags converges
# the cluster to match.
set -euo pipefail

CONTEXT_NAME="k3s-tt"

# Same host-LAN-IP heuristic Makefile's render-settings uses for data_store.endpoint -- one
# detection, reused everywhere an address for this host needs to reach a pod's network
# namespace. k3s's own containerd needs it too: it pulls workload images from the local registry
# (docker-compose.yml) over this address, never localhost.
REGISTRY_HOST_IP="$(ip -4 addr show 2>/dev/null | awk '/inet /{print $2}' | \
  grep -vE '^(127\.|10\.88\.|172\.1[6-9]\.|172\.2[0-9]\.|172\.3[01]\.)' | head -1 | cut -d/ -f1)"
if [[ -z "${REGISTRY_HOST_IP}" ]]; then
  echo "ERROR: could not detect a host LAN address for the local registry." >&2
  exit 1
fi

# Matches localdev/k3s-macos/install.sh's pin so both profiles track the same tested k8s
# minor version — deliberately not "latest". DRA (resource.k8s.io/v1) is GA
# from k8s 1.34; this is comfortably past that, so no DRA feature-gate flags
# are needed on the apiserver/kubelet (they would be required at 1.33, per
# tt-dra-driver's own stated minimum).
K3S_VERSION="v1.36.2+k3s1"

TT_OPERATOR_NAMESPACE="${TT_OPERATOR_NAMESPACE:-tt-operator-system}"
TT_OPERATOR_CHART="oci://ghcr.io/tenstorrent/helm/tt-operator"
# Pin the chart version once a known-good one is confirmed working here, e.g.
# TT_OPERATOR_CHART_VERSION="0.1.0"; empty tracks whatever "latest" resolves to.
TT_OPERATOR_CHART_VERSION="${TT_OPERATOR_CHART_VERSION:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

if [[ "$(uname)" != "Linux" ]]; then
  echo "ERROR: this script installs k3s natively and expects real Tenstorrent"
  echo "       PCIe hardware — it only makes sense on a Linux TT host. For"
  echo "       simulated local dev on macOS/Linux, use localdev/k3s-macos/install.sh instead."
  exit 1
fi

# ---- Preflight: make sure there's actually Tenstorrent hardware here --------
echo "==> Checking for Tenstorrent hardware..."
if ! command -v lspci &>/dev/null || ! lspci -d 1e52: &>/dev/null; then
  echo "ERROR: no Tenstorrent PCIe device found (lspci -d 1e52:). This script is"
  echo "       for real Tenstorrent hosts — use localdev/k3s-macos/install.sh for simulated nodes."
  exit 1
fi
TT_CARD_COUNT="$(lspci -d 1e52: | wc -l)"
echo "    Found ${TT_CARD_COUNT} Tenstorrent PCIe device(s):"
lspci -d 1e52: | sed 's/^/      /'

# Checked via /sys rather than `lsmod | grep` — under `set -o pipefail`, grep -q
# exits as soon as it finds a match, SIGPIPE-ing lsmod, which pipefail then
# reports as a pipeline failure even though the module *is* loaded.
if [[ ! -d /sys/module/tenstorrent ]]; then
  echo "ERROR: tt-kmd (tenstorrent kernel module) is not loaded. Install it first:"
  echo "         sudo apt install tenstorrent-dkms tenstorrent-tools"
  echo "       then reboot or 'sudo modprobe tenstorrent'."
  exit 1
fi
echo "    tt-kmd loaded ($(cat /sys/module/tenstorrent/version 2>/dev/null || echo unknown) )"

if [[ ! -e /dev/tenstorrent/0 ]]; then
  echo "ERROR: tt-kmd is loaded but /dev/tenstorrent/0 doesn't exist. Check dmesg"
  echo "       for probe failures and that the 50-tenstorrent.rules udev rule ran."
  exit 1
fi

# ---- k3s: native systemd service, same version pin as localdev --------------
STAGE_T0=$(date +%s)
echo "==> Writing registries.yaml (registry at ${REGISTRY_HOST_IP}:5000)..."
sudo mkdir -p /etc/rancher/k3s
cat <<EOF | sudo tee /etc/rancher/k3s/registries.yaml >/dev/null
mirrors:
  "${REGISTRY_HOST_IP}:5000":
    endpoint:
      - "http://${REGISTRY_HOST_IP}:5000"
configs:
  "${REGISTRY_HOST_IP}:5000":
    tls:
      insecure_skip_verify: true
EOF
sudo systemctl is-active k3s >/dev/null 2>&1 && sudo systemctl restart k3s || true

echo "==> Ensuring k3s ${K3S_VERSION} is installed and running..."
if ! command -v k3s &>/dev/null || ! k3s --version | grep -q "${K3S_VERSION}"; then
  echo "    Installing/upgrading k3s..."
  sudo systemctl stop k3s 2>/dev/null || true
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="${K3S_VERSION}" \
    INSTALL_K3S_EXEC="--disable traefik --write-kubeconfig-mode 644" sh -
else
  echo "    k3s ${K3S_VERSION} already installed."
  sudo systemctl is-active --quiet k3s || sudo systemctl start k3s
fi

mkdir -p "${HOME}/.kube"

# k3s.yaml always names its objects "default" — after a previous install we
# renamed the *context* to CONTEXT_NAME but its cluster/user objects are
# still literally named "default" underneath (rename-context only renames
# the context, not what it points to). A fresh k3s install generates a new
# CA, also under the name "default"; kubectl's KUBECONFIG merge keeps
# whichever same-named object it finds in the earlier file, so a leftover
# "default" cluster in this file would win over the new one and leave a
# stale-CA entry that fails TLS verification. Drop any leftover "default"
# objects before merging so the fresh ones always win.
if [[ -f "${HOME}/.kube/config" ]]; then
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-cluster default 2>/dev/null || true
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-context default 2>/dev/null || true
  KUBECONFIG="${HOME}/.kube/config" kubectl config delete-user    default 2>/dev/null || true
fi

KUBECONFIG="${HOME}/.kube/config:/etc/rancher/k3s/k3s.yaml" \
  kubectl config view --flatten > /tmp/kube-merged.yaml
mv /tmp/kube-merged.yaml "${HOME}/.kube/config"
chmod 600 "${HOME}/.kube/config"
kubectl config rename-context default "${CONTEXT_NAME}" 2>/dev/null || true
kubectl config use-context "${CONTEXT_NAME}"

echo "==> Waiting for node..."
wait_for 40 3 "node to register" kubectl --context "${CONTEXT_NAME}" get nodes
kubectl --context "${CONTEXT_NAME}" wait node --all --for=condition=Ready --timeout=120s
echo "==> k3s stage: $(( $(date +%s) - STAGE_T0 ))s"

source "${SCRIPT_DIR}/../lib/node.sh"

# ---- helm ---------------------------------------------------------------
STAGE_T0=$(date +%s)
if ! command -v helm &>/dev/null; then
  echo "==> Installing Helm..."
  curl -sfL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
    | sudo bash
else
  echo "==> Helm already installed ($(helm version --short))."
fi

# tt-operator's umbrella chart depends on the upstream NFD chart by repository
# URL (not OCI) — helm needs that repo registered locally to resolve the
# dependency even though the umbrella chart itself is pulled from an OCI
# registry. See docs.tenstorrent.com/tt-operator/latest/installation.html.
echo "==> Ensuring node-feature-discovery Helm repo is registered..."
helm repo add node-feature-discovery https://kubernetes-sigs.github.io/node-feature-discovery/charts >/dev/null 2>&1 || true
helm repo update node-feature-discovery >/dev/null

# ---- tt-operator ----------------------------------------------------------
echo "==> Installing/upgrading tt-operator into namespace ${TT_OPERATOR_NAMESPACE}..."
HELM_ARGS=(
  upgrade --install tt-operator "${TT_OPERATOR_CHART}"
  --kube-context "${CONTEXT_NAME}"
  --namespace "${TT_OPERATOR_NAMESPACE}" --create-namespace
  --set tt-k8s-driver-manager.enabled=false
  --set jobset.enabled=false
  --set kubepmix.enabled=false
  # Restrict the DRA driver to the real node — fake nodes share this host's kernel and would
  # otherwise falsely advertise the same physical Tenstorrent card.
  --set-json 'tt-dra-driver.kubeletPlugin.nodeSelector={"node-role.kubernetes.io/control-plane":"true"}'
  # tt-telemetry's aggregator Deployment polls each collector over
  # hostNetwork:8080 — the same port the per-node collector DaemonSet itself
  # listens on. On a multi-node cluster that's fine (they land on different
  # nodes); on this single-QuietBox cluster both would land on the one node
  # and collide (FailedScheduling: no free ports). Nothing to aggregate
  # across on a single host anyway, so disable the aggregator and keep just
  # the per-node collector.
  --set tt-telemetry.aggregator.enabled=false
  --wait --timeout 8m
)
if [[ -n "${TT_OPERATOR_CHART_VERSION}" ]]; then
  HELM_ARGS+=(--version "${TT_OPERATOR_CHART_VERSION}")
fi
helm "${HELM_ARGS[@]}"

echo "==> Waiting for Tenstorrent node label from NFD..."
wait_for 40 3 "NFD pci-1200_1e52 label" \
  kubectl --context "${CONTEXT_NAME}" get nodes -l feature.node.kubernetes.io/pci-1200_1e52.present=true -o name
wait_for 40 3 "Tenstorrent ResourceSlice" sh -c \
  "kubectl --context '${CONTEXT_NAME}' get resourceslices -o json | python3 -c 'import json,sys; assert any(x.get(\"spec\",{}).get(\"driver\")==\"tenstorrent.com\" for x in json.load(sys.stdin)[\"items\"])'"
echo "==> tt-operator stage: $(( $(date +%s) - STAGE_T0 ))s"

# ---- cluster-agent bundle -------------------------------------------------
# `make images` (Makefile's `tt-up: images` prerequisite) already pushed the images this cluster
# needs to the local registry; this node pulls them normally via the registries.yaml written
# above. Install the node-agent DaemonSet + cluster-agent Deployment so something is actually
# polling the control plane for work -- without this, jobs submitted to it stay QUEUED forever.
STAGE_T0=$(date +%s)
# host.containers.internal is a podman-machine hostname and is not available to native k3s pods
# convenience hostname that doesn't exist on native Linux — nothing on this host maps it to
# anything, so cluster-agent would poll a name that never resolves. The control plane's containers
# publish on 0.0.0.0 (confirmed via `docker port`), so the host's own real IP reaches them from
# any pod on this node.
HOST_IP="$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if ($i=="src") print $(i+1); exit}')"
CLUSTER_NAME="tt-quietbox" KUBECONFIG_PATH="${HOME}/.kube/config" KUBE_CONTEXT="${CONTEXT_NAME}" \
  API_URL="http://${HOST_IP}:8081" METRICS_URL="http://${HOST_IP}:8084" \
  REGISTRY_URL="${REGISTRY_HOST_IP}:5000" TAG="${TAG:-latest}" \
  bash "${SCRIPT_DIR}/../../runtime/k8s/infra/install.sh"
echo "${REGISTRY_HOST_IP}" > "${SCRIPT_DIR}/../.registry-host-ip"
echo "==> cluster-agent stage: $(( $(date +%s) - STAGE_T0 ))s"

# Donates zero capacity to workloads by default, same as localdev/k3s-macos/install.sh — applied
# only now, after tt-operator's own management pods (fabric-manager, NFD master/gc, coredns,
# metrics-server) have already landed on this one node while it was still schedulable. Tainting
# any earlier leaves those pods Pending forever (no toleration, no other node to land on),
# which is exactly what made this hang against helm's own --wait --timeout 8m. node-agent's
# DaemonSet still monitors this node afterward (tolerates every taint). dev-nodes-up.sh attaches
# it as a full worker when actually needed (dev work, the portable suite, or the real-hardware
# e2e); dev-nodes-down.sh detaches it so real training work sharing this box isn't competing with
# anything k3s scheduled onto it.
lib_detach_node "${CONTEXT_NAME}" tt-quietbox

echo "==> Cluster ready. Context: ${CONTEXT_NAME}"
echo
bash "${SCRIPT_DIR}/status.sh"
