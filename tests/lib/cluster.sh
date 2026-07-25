#!/usr/bin/env bash
# kubectl-facing helpers for fault-injection scenarios. Requires common.sh already sourced.
# All of these mutate cluster-wide state (nodes, the cluster-agent Deployment, the node-agent
# DaemonSet) — scenarios using them must run in the CLUSTER_EXCLUSIVE group in run.sh, not
# concurrently with each other.

job_pod() { kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true; }
job_node() { kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$1" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true; }
job_pod_count() { kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$1" --no-headers 2>/dev/null | wc -l | tr -d ' '; }
job_distinct_node_count() { kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$1" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sed '/^$/d' | sort -u | wc -l | tr -d ' '; }
job_uid() { kubectl -n "$JOB_NS" get jobs -l "hypothesisloop.io/experiment-id=$1" -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null || true; }
job_resource_exists() { [[ -n "$(job_uid "$1")" ]]; }
job_resource_absent() { ! job_resource_exists "$1"; }
job_recreated_with_new_uid() { local current; current=$(job_uid "$1"); [[ -n "$current" && "$current" != "$2" ]]; }
delete_job_resource() { kubectl -n "$JOB_NS" delete jobs -l "hypothesisloop.io/experiment-id=$1" --wait=true >/dev/null; }
corrupt_job_desired_hash() {
  kubectl -n "$JOB_NS" annotate jobs -l "hypothesisloop.io/experiment-id=$1" \
    hypothesisloop.io/desired-spec-hash=externally-mutated --overwrite >/dev/null
}
corrupt_service_desired_hash() {
  kubectl -n "$JOB_NS" annotate service -l "hypothesisloop.io/experiment-id=$1" \
    hypothesisloop.io/desired-spec-hash=externally-mutated --overwrite >/dev/null
}

# kill_node_running_job JOB_ID -> cordons the node running JOB_ID's pod and force-deletes the
# pod (simulates real infra loss). Prints the killed node's name on stdout.
kill_node_running_job() {
  local job_id="$1" pod node
  read -r pod node <<< "$(kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$job_id" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}{" "}{.items[0].spec.nodeName}' 2>/dev/null || true)"
  [[ -z "$pod" || -z "$node" ]] && return 1
  kubectl cordon "$node" > /dev/null
  kubectl -n "$JOB_NS" delete pod "$pod" --grace-period=0 --force > /dev/null 2>&1 || true
  echo "$node"
}

uncordon_node() { kubectl uncordon "$1" > /dev/null 2>&1 || true; }

# job_rescheduled_off JOB_ID OLD_NODE -> true once the job has a running pod on a node != OLD_NODE.
job_rescheduled_off() {
  local job_id="$1" old_node="$2" new_node
  new_node=$(kubectl -n "$JOB_NS" get pods -l "hypothesisloop.io/experiment-id=$job_id" \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)
  [[ -n "$new_node" && "$new_node" != "$old_node" ]]
}

# --- cluster<->controlplane connectivity loss -------------------------------------------
# Scaling the cluster-agent Deployment to 0 stops all outbound reports/reconciliation without
# touching the control plane or any job pods already running — the cleanest available proxy
# for "this cluster lost network reachability to the control plane" in a single-cluster local
# dev setup (no separate network segment to partition).
disconnect_cluster_agent() {
  kubectl -n "$CLUSTER_NS" scale deployment/hypothesisloop-cluster-agent --replicas=0 > /dev/null
  kubectl -n "$CLUSTER_NS" wait --for=delete pod -l app=hypothesisloop-cluster-agent --timeout=30s > /dev/null
}

reconnect_cluster_agent() {
  kubectl -n "$CLUSTER_NS" scale deployment/hypothesisloop-cluster-agent --replicas=1 > /dev/null
  kubectl -n "$CLUSTER_NS" rollout status deployment/hypothesisloop-cluster-agent --timeout=60s > /dev/null
}

cluster_agent_connected() {
  [[ "$(kubectl -n "$CLUSTER_NS" get deployment/hypothesisloop-cluster-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" == "1" ]]
}
cluster_agent_disconnected() { ! cluster_agent_connected; }

# restart_node_agent_daemonset -> rolling-restarts the metrics DaemonSet and waits for it to
# come back, simulating a redeploy of the per-node metrics agent.
restart_node_agent_daemonset() {
  kubectl -n "$CLUSTER_NS" rollout restart daemonset/hypothesisloop-node-agent > /dev/null
  kubectl -n "$CLUSTER_NS" rollout status daemonset/hypothesisloop-node-agent --timeout=60s > /dev/null
}
