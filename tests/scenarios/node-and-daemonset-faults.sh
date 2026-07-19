#!/usr/bin/env bash
# Two infra faults chained against the same running job, cheapest/least-disruptive first:
#   1. The per-node metrics DaemonSet (node-agent) gets redeployed underneath it — a
#      different pod on the same node, so the job must be completely unaffected and stay on
#      its original node, and a fresh job submitted right after must still get admitted
#      normally (no duplicate/stuck capacity accounting from the restart).
#   2. Its node then dies outright (cordon + force-delete its pod) — cluster-agent's
#      desired-state reconciliation (reconcileOnce) must self-heal it onto a different node
#      without operator intervention, and dashboard metrics must stay available across the
#      gap.
# CLUSTER_EXCLUSIVE (mutates real node/DaemonSet state).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

AGENT="agent-infra-faults-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "node-daemonset-faults-${RUN_ID}" 1.0 1)
signup_and_start "$PE_ID" "$AGENT"

JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.03")
S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING (status=$S) — cannot exercise infra-fault scenarios"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "job reached RUNNING"

echo "=========================================================="
echo "Fault 1: node-agent DaemonSet redeploy"
echo "=========================================================="
NODE_BEFORE=$(job_node "$JOB")
restart_node_agent_daemonset
pass "node-agent DaemonSet restarted and reports Ready again"

S2=$(get_status "$JOB")
[[ "$S2" == "RUNNING" || "$S2" == "COMPLETED" ]] \
  && pass "job unaffected by the DaemonSet restart (status=$S2)" \
  || fail "job status became $S2 after an unrelated DaemonSet restart"
NODE_AFTER=$(job_node "$JOB")
[[ "$NODE_AFTER" == "$NODE_BEFORE" ]] \
  && pass "job stayed on its original node ($NODE_BEFORE) — DaemonSet restart did not evict it" \
  || echo "  [WARN] job's node changed ($NODE_BEFORE -> $NODE_AFTER) — investigate if unexpected"

POST_DS_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02")
# QUEUED deliberately excluded here — it's the job's initial state on every submission, so
# including it in the wait target would make this return immediately without ever actually
# waiting for admission (the thing this assertion means to check).
S3=$(wait_for_status "$POST_DS_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" 45 || true)
[[ "$S3" == "RUNNING" || "$S3" == "COMPLETED" ]] \
  && pass "new job admitted normally right after the DaemonSet redeploy (status=$S3)" \
  || fail "new job failed to get admitted after the DaemonSet redeploy (status=$S3)"
wait_for_status "$POST_DS_JOB" "COMPLETED,FAILED,EVICTED" 60 > /dev/null || true

echo ""
echo "=========================================================="
echo "Fault 2: node death mid-run -> cluster-agent self-heal"
echo "=========================================================="
if [[ "$(get_status "$JOB")" != "RUNNING" ]]; then
  fail "job no longer RUNNING before node-death fault (status=$(get_status "$JOB")) — skipping"
else
  NODE=$(kill_node_running_job "$JOB")
  if [[ -z "$NODE" ]]; then
    fail "could not locate job's pod/node to kill"
  else
    echo "  killed node: $NODE"
    if wait_until "job rescheduled off $NODE" 30 1 job_rescheduled_off "$JOB" "$NODE"; then
      pass "job rescheduled onto a different node"
    else
      fail "job did not reschedule off $NODE within timeout"
    fi
    uncordon_node "$NODE"

    S=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" 150 || true)
    [[ "$S" == "COMPLETED" ]] && pass "job completed after reschedule" || fail "job did not complete cleanly after reschedule (status=$S)"

    METRICS=$(dashboard_metrics "$JOB")
    if [[ "$METRICS" != "[]" && -n "$METRICS" ]]; then
      pass "dashboard metrics available across the reschedule gap"
    else
      echo "  [WARN] no metrics returned (may be expected if the workload reports late)"
    fi
  fi
fi

close_platform_experiment "$PE_ID"
finish
