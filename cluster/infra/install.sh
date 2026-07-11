#!/usr/bin/env bash
# Install the OpenResearch "cluster agent" bundle onto a target Kubernetes
# cluster where training jobs actually run: the openresearch-node-agent
# DaemonSet (per-node CPU metrics) and the openresearch-cluster-agent
# Deployment (the only component with credentials to this cluster's API —
# it long-polls the control plane for work and pushes status back; the
# control plane never dials into this cluster directly). No external
# queueing operator — admission, priority, and preemption are native
# Kubernetes (Job.spec + PriorityClass), applied locally by cluster-agent.
#
# Run this once per target cluster. It is idempotent — safe to re-run against
# a cluster that already has the bundle installed (e.g. after an upgrade).
#
# Parameters (env vars, all optional):
#   CLUSTER_NAME      — label identifying this target cluster (default: local)
#   CONTROLPLANE_URL  — outbound URL cluster-agent polls (default: http://host.docker.internal:8082)
#   REGISTRY_URL      — outbound URL training pods push metrics to, must be reachable from
#                       *inside* a pod in this cluster, not just from cluster-agent
#                       (default: http://host.docker.internal:8083)
#   KUBECONFIG_PATH   — kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT      — kubectl context to target (default: current-context)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-local}"
CONTROLPLANE_URL="${CONTROLPLANE_URL:-http://host.docker.internal:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://host.docker.internal:8083}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${HOME}/.kube/config}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export KUBECONFIG="${KUBECONFIG_PATH}"

# Resolve the context to use: explicit KUBE_CONTEXT, else whatever kubectl
# currently points at.
if [[ -z "${KUBE_CONTEXT}" ]]; then
  KUBE_CONTEXT="$(kubectl config current-context)"
fi

kctl() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

echo "==> Installing cluster-agent bundle on cluster '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT})"

# The openresearch-jobs namespace and PriorityClasses are created idempotently by
# cluster-agent itself on startup (it has in-cluster credentials; nothing to apply here).

# ---- node-agent DaemonSet -----------------------------------------------------
# Deploy the node-agent DaemonSet (reads cgroup v2 CPU stats, pushes to
# metric-controller). The image must already be built and imported into the
# cluster's container runtime.
echo "==> Applying node-agent DaemonSet..."
sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g" "${SCRIPT_DIR}/node-agent-daemonset.yaml" \
  | kctl apply -f -

# ---- cluster-agent Deployment (RBAC + the actual reconciler) ------------------
# The only component in this cluster with real k8s credentials. It long-polls
# CONTROLPLANE_URL for create/delete-workload commands and pushes job status
# back — the control plane itself never opens a connection into this cluster.
echo "==> Applying cluster-agent Deployment (control plane: ${CONTROLPLANE_URL}, registry: ${REGISTRY_URL})..."
sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g; s|__CONTROLPLANE_URL__|${CONTROLPLANE_URL}|g; s|__REGISTRY_URL__|${REGISTRY_URL}|g" \
  "${SCRIPT_DIR}/cluster-agent-deployment.yaml" \
  | kctl apply -f -

echo "==> Cluster agent bundle installed on '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT})."
