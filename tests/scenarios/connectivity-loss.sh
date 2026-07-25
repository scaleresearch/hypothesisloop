#!/usr/bin/env bash
# Cluster loses connectivity to the control plane (cluster-agent stops reporting). Two phases
# on one running stack, sharing the disconnect/verify setup:
#   1. restore: reconnects after a short outage — a new submission that failed closed while
#      disconnected must get admitted once capacity reporting resumes.
#   2. permanent: a second outage that is never restored before assertions run — a job submitted
#      during this outage must sit durably QUEUED for as long as it lasts, and an already-RUNNING
#      job keeps running independently. Its workload metrics and node-agent heartbeats are direct
#      liveness signals and do not stop merely because cluster-agent capacity reporting is down.
# In phase 1 (short outage, well under the silence window), an already-RUNNING job must be
# completely undisturbed (job pods don't depend on cluster-agent's liveness). CLUSTER_EXCLUSIVE.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

# Safety net: if this script is killed (outer timeout) or exits early on an assertion failure
# while cluster-agent is scaled to 0, it must not stay down for the rest of the suite — that
# would zero out RAM/storage/accelerator capacity reporting for every scenario after this one.
# Combined into one trap (not a second `trap ... EXIT`) since common.sh already installs its own
# for $TMPDIR_T cleanup, and bash trap only keeps the latest handler per signal.
restore_cluster_agent() {
  rm -rf "$TMPDIR_T"
  kubectl -n "${CLUSTER_NS:-hypothesisloop}" scale deployment/hypothesisloop-cluster-agent --replicas=1 >/dev/null 2>&1 || return
  kubectl -n "${CLUSTER_NS:-hypothesisloop}" rollout status deployment/hypothesisloop-cluster-agent --timeout=60s >/dev/null 2>&1 || true
}
trap restore_cluster_agent EXIT

cluster_absent_from_live_metrics() {
  curl -sf "$SCHED_URL/internal/clusters/" \
    | py "import sys,json; print(all(c['cluster_name'] != sys.argv[1] for c in json.load(sys.stdin)['clusters']))" "$CLUSTER_NAME" \
    | grep -qx True
}

CLUSTER_NAME=$(kubectl -n "$CLUSTER_NS" get deployment/hypothesisloop-cluster-agent \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="cluster-agent")].env[?(@.name=="CLUSTER_NAME")].value}')
[[ -n "$CLUSTER_NAME" ]] || { echo "cluster-agent deployment has no CLUSTER_NAME" >&2; exit 1; }

AGENT="agent-connloss-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "connectivity-loss-${RUN_ID}" 1.0 1)
signup_and_start "$PE_ID" "$AGENT"

echo "=========================================================="
echo "Phase 1: disconnect, then restore"
echo "=========================================================="
RUNNING_JOB_HOURS="0.03"
RUNNING_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$RUNNING_JOB_HOURS")
S=$(wait_for_status "$RUNNING_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING before disconnect (status=$S) — cannot exercise connectivity loss"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "job reached RUNNING before disconnect"

disconnect_cluster_agent
wait_until "cluster-agent reports disconnected" 15 1 cluster_agent_disconnected \
	|| fail "cluster-agent never became disconnected; outage assertions would be invalid"

assert_stable_status "$RUNNING_JOB" "RUNNING,COMPLETED" 10 \
  "already-running job undisturbed by connectivity loss (should not depend on cluster-agent liveness)"

QUEUED_JOB_HOURS="0.02"
QUEUED_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$QUEUED_JOB_HOURS")
S3=$(wait_for_status "$QUEUED_JOB" "RUNNING,COMPLETED" 10 || true)
[[ "$S3" == "RUNNING" || "$S3" == "COMPLETED" ]] \
  && fail "new job was admitted (status=$S3) while cluster-agent is disconnected — capacity should fail closed" \
  || pass "new job correctly did not run while capacity reporting is down (status=$(get_status "$QUEUED_JOB"))"

reconnect_cluster_agent
wait_until "cluster-agent reports connected" 30 1 cluster_agent_connected \
  && pass "cluster-agent reconnected" \
  || fail "cluster-agent did not report connected within timeout after reconnect"

S4=$(wait_for_status "$QUEUED_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S4" == "RUNNING" || "$S4" == "COMPLETED" ]] \
  && pass "queued job admitted once capacity reporting resumed (status=$S4)" \
  || fail "queued job never got admitted after reconnect (status=$S4)"

# Both jobs are already confirmed RUNNING/admitted by this point (S/S4 above), so this is a
# completion-only wait: budget comes from each job's own hours, not one shared hand-tuned number.
declare -A PHASE1_JOB_HOURS=(["$RUNNING_JOB"]="$RUNNING_JOB_HOURS" ["$QUEUED_JOB"]="$QUEUED_JOB_HOURS")
for J in "$RUNNING_JOB" "$QUEUED_JOB"; do
  [[ "$(wait_for_status "$J" "COMPLETED,FAILED,EVICTED" "$(completion_wait_tries "${PHASE1_JOB_HOURS[$J]}")" || true)" == "COMPLETED" ]] \
    && file_finding "$J"
done

echo ""
echo "=========================================================="
echo "Phase 2: disconnect again, never restore before asserting"
echo "=========================================================="
# 0.15h (540s) keeps the workload actively posting improving metrics through the full
# disconnect-detect plus heartbeat expiry, so it can't finish and flatline early and
# spuriously trip metric-decline (declineWindow=0.3*hours) before the silence path applies.
RUNNING_JOB2=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.15")
S=$(wait_for_status "$RUNNING_JOB2" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING before second disconnect (status=$S)"
else
  pass "job reached RUNNING before second (permanent) disconnect"
	disconnect_cluster_agent
	wait_until "cluster-agent reports disconnected" 15 1 cluster_agent_disconnected \
		|| fail "cluster-agent never became disconnected in permanent-outage phase"

  wait_until "cluster capacity ages out of live metrics" 45 1 cluster_absent_from_live_metrics \
    || fail "cluster remained active after its heartbeat freshness window elapsed"

  S2=$(get_status "$RUNNING_JOB2")
  if [[ "$S2" == "COMPLETED" ]]; then
    pass "already-running job completed while cluster-agent capacity reporting was disconnected"
  elif [[ "$S2" == "RUNNING" ]]; then
    pass "already-running job remains live during cluster-agent outage (metrics/node heartbeat still available)"
  else
    fail "already-running job ended as $S2 during a cluster-agent outage"
  fi
  [[ "$S2" == "COMPLETED" ]] && file_finding "$RUNNING_JOB2"

  STUCK_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02")
  assert_stable_status "$STUCK_JOB" "QUEUED" 15 \
    "job submitted after cluster capacity became stale stays durably queued"

  reconnect_cluster_agent
  # Must actually assert this, not just attempt it and move on: tests/run.sh runs the other
  # CLUSTER_EXCLUSIVE scenario right after this one exits, and it needs a healthy cluster-agent
  # from its very first submission — leaving reconnection unverified here silently poisons
  # that next scenario's result instead of attributing the failure to where it belongs.
  wait_until "cluster-agent reports connected" 45 1 cluster_agent_connected \
    && pass "cluster-agent reconnected after the permanent-outage phase" \
    || fail "cluster-agent did not reconnect after the permanent-outage phase — later CLUSTER_EXCLUSIVE scenarios may fail as a result"
  wait_for_status "$STUCK_JOB" "COMPLETED,FAILED,EVICTED,RUNNING" 45 > /dev/null || true
fi

echo ""
echo "=========================================================="
echo "Phase 3: desired deletion converges after reconciliation was unavailable"
echo "=========================================================="
DELETE_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.08")
DELETE_STATUS=$(wait_for_status "$DELETE_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$DELETE_STATUS" != "RUNNING" ]]; then
  fail "delete-convergence job did not reach RUNNING (status=$DELETE_STATUS)"
else
	disconnect_cluster_agent
	wait_until "cluster-agent reports disconnected" 15 1 cluster_agent_disconnected \
		|| fail "cluster-agent never became disconnected in delete-convergence phase"
  curl -sf -X POST "$SCHED_URL/experiments/${DELETE_JOB}/cancel" > /dev/null \
    || fail "failed to persist cancellation while cluster-agent was disconnected"
  [[ "$(get_status "$DELETE_JOB")" == "EVICTED" ]] \
    && pass "PostgreSQL desired lifecycle changed to EVICTED while reconciliation was unavailable" \
    || fail "cancelled job did not become EVICTED in PostgreSQL"
  job_resource_exists "$DELETE_JOB" \
    && pass "actual Job remained while cluster-agent was unavailable, proving deletion was not pushed" \
    || fail "actual Job disappeared before cluster-agent reconnected"
  reconnect_cluster_agent
  wait_until "cluster-agent reports connected" 45 1 cluster_agent_connected \
    || fail "cluster-agent did not reconnect for delete convergence"
  wait_until "cluster-agent removes no-longer-desired Job" 30 1 job_resource_absent "$DELETE_JOB" \
    && pass "fresh reconciliation removed the stale actual Job after reconnect" \
    || fail "actual Job remained after PostgreSQL no longer desired it"
fi

close_platform_experiment "$PE_ID"
finish
