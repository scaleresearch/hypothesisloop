#!/usr/bin/env bash
# kubectl-facing helpers for fault-injection scenarios. Requires common.sh already sourced.
# All of these mutate cluster-wide state (nodes, the cluster-agent Deployment, the node-agent
# DaemonSet) — scenarios using them must run in the CLUSTER_EXCLUSIVE group in run.sh, not
# concurrently with each other.

job_pod() { kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true; }
job_node() { kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$1" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true; }
job_pod_count() { kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$1" --no-headers 2>/dev/null | wc -l | tr -d ' '; }

# kill_node_running_job JOB_ID -> cordons the node running JOB_ID's pod and force-deletes the
# pod (simulates real infra loss). Prints the killed node's name on stdout.
kill_node_running_job() {
  local job_id="$1" pod node
  pod=$(job_pod "$job_id"); node=$(job_node "$job_id")
  [[ -z "$pod" || -z "$node" ]] && return 1
  kubectl cordon "$node" > /dev/null
  kubectl -n "$JOB_NS" delete pod "$pod" --grace-period=0 --force > /dev/null 2>&1 || true
  echo "$node"
}

uncordon_node() { kubectl uncordon "$1" > /dev/null 2>&1 || true; }

# job_rescheduled_off JOB_ID OLD_NODE -> true once the job has a running pod on a node != OLD_NODE.
job_rescheduled_off() {
  local job_id="$1" old_node="$2" new_node
  new_node=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$job_id" \
    --field-selector=status.phase!=Failed -o jsonpath='{.items[-1:].spec.nodeName}' 2>/dev/null || true)
  [[ -n "$new_node" && "$new_node" != "$old_node" ]]
}

# --- cluster<->controlplane connectivity loss -------------------------------------------
# Scaling the cluster-agent Deployment to 0 stops all outbound reports/reconciliation without
# touching the control plane or any job pods already running — the cleanest available proxy
# for "this cluster lost network reachability to the control plane" in a single-cluster local
# dev setup (no separate network segment to partition).
disconnect_cluster_agent() {
  kubectl -n "$CLUSTER_NS" scale deployment/openresearch-cluster-agent --replicas=0 > /dev/null
  kubectl -n "$CLUSTER_NS" wait --for=delete pod -l app=openresearch-cluster-agent --timeout=30s > /dev/null 2>&1 || true
}

reconnect_cluster_agent() {
  kubectl -n "$CLUSTER_NS" scale deployment/openresearch-cluster-agent --replicas=1 > /dev/null
  # No `|| true` would let a slow rollout kill the whole scenario via set -e right here,
  # leaving cluster-agent possibly still not Ready when the NEXT scenario in tests/run.sh's
  # CLUSTER_EXCLUSIVE sequence starts — callers must check the real readiness condition
  # themselves (e.g. wait_until ... cluster_agent_connected) rather than trust this alone.
  kubectl -n "$CLUSTER_NS" rollout status deployment/openresearch-cluster-agent --timeout=60s > /dev/null 2>&1 || true
}

cluster_agent_connected() {
  [[ "$(kubectl -n "$CLUSTER_NS" get deployment/openresearch-cluster-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" == "1" ]]
}
cluster_agent_disconnected() { ! cluster_agent_connected; }

# restart_node_agent_daemonset -> rolling-restarts the metrics DaemonSet and waits for it to
# come back, simulating a redeploy of the per-node metrics agent.
restart_node_agent_daemonset() {
  kubectl -n "$CLUSTER_NS" rollout restart daemonset/openresearch-node-agent > /dev/null
  kubectl -n "$CLUSTER_NS" rollout status daemonset/openresearch-node-agent --timeout=60s > /dev/null
}
