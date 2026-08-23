#!/usr/bin/env bash
# Install the HypothesisLoop "cluster agent" bundle onto a target Kubernetes
# cluster where training jobs actually run: hypothesisloop-node-agent
# (DaemonSet, per-node CPU metrics) and hypothesisloop-cluster-agent
# (Deployment, the only component with credentials to this cluster's API —
# long-polls the control plane for work, pushes status back). No external
# queueing operator — admission, priority, and preemption are native
# Kubernetes (Job.spec + PriorityClass), applied locally by cluster-agent.
#
# Run this once per target cluster. It is idempotent — safe to re-run against
# a cluster that already has the bundle installed (e.g. after an upgrade).
#
# Parameters (env vars, all optional):
#   CLUSTER_NAME      — label identifying this target cluster (default: local)
#   API_URL           — control-plane API base URL. The cluster-agent polls it, and it is
#                       also handed to training pods to push metrics to, so it must be reachable from
#                       *inside* a pod in this cluster, not just from cluster-agent
#                       (default: http://host.containers.internal:8081)
#   METRICS_URL       — outbound URL node-agents push observations to
#                       (default: http://host.containers.internal:8084)
#   REGISTRY_URL      — registry host:port these images are pulled from. Must already be
#                       reachable from every node in this cluster — provisioning that
#                       reachability (a registry mirror, VPC peering, whatever the cluster's own
#                       network needs) is the cluster provisioner's job, not this script's; it
#                       only names the address, same as API_URL/METRICS_URL (default: localhost:5000,
#                       the local dev registry from localdev/controlplane/docker-compose.yml)
#   TAG               — image tag to deploy, normally the git SHA `make images` just pushed
#                       (default: latest)
#   KUBECONFIG_PATH   — kubeconfig file to use (default: $HOME/.kube/config)
#   KUBE_CONTEXT      — kubectl context to target (default: current-context)
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-local}"
API_URL="${API_URL:-http://host.containers.internal:8081}"
METRICS_URL="${METRICS_URL:-http://host.containers.internal:8084}"
REGISTRY_URL="${REGISTRY_URL:-localhost:5000}"
TAG="${TAG:-latest}"
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

# The hypothesisloop-jobs namespace and PriorityClasses are created idempotently by
# cluster-agent itself on startup (it has in-cluster credentials; nothing to apply here).

# ---- node-agent DaemonSet -----------------------------------------------------
# Deploy the node-agent DaemonSet (reads cgroup v2 CPU stats, pushes to
# metric-controller). The image must already be built and imported into the
# cluster's container runtime.
echo "==> Applying node-agent DaemonSet..."
sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g; s|__METRICS_URL__|${METRICS_URL}|g; \
     s|__REGISTRY__|${REGISTRY_URL}|g; s|__TAG__|${TAG}|g" "${SCRIPT_DIR}/node-agent-daemonset.yaml" \
  | kctl apply -f -

# ---- cluster-agent Deployment (RBAC + the actual reconciler) ------------------
# The only component in this cluster with real k8s credentials. It long-polls
# API_URL for create/delete-workload commands and pushes job status
# back — the control plane itself never opens a connection into this cluster.
echo "==> Applying cluster-agent Deployment (API: ${API_URL})..."
sed "s|__CLUSTER_NAME__|${CLUSTER_NAME}|g; s|__API_URL__|${API_URL}|g; \
     s|__REGISTRY__|${REGISTRY_URL}|g; s|__TAG__|${TAG}|g" \
  "${SCRIPT_DIR}/cluster-agent-deployment.yaml" \
  | kctl apply -f -

echo "==> Cluster agent bundle installed on '${CLUSTER_NAME}' (context: ${KUBE_CONTEXT})."
