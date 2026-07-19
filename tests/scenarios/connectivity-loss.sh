#!/usr/bin/env bash
# Cluster loses connectivity to the control plane (cluster-agent stops reporting). Two phases
# on one running stack, sharing the disconnect/verify setup:
#   1. restore: reconnects after a short outage — a new submission that failed closed while
#      disconnected must get admitted once capacity reporting resumes.
#   2. permanent: a second outage that is never restored before assertions run — a job submitted
#      during this outage must sit durably QUEUED for as long as it lasts, and an already-RUNNING
#      job must survive a SHORT outage undisturbed but is correctly evicted (reason=silent) once
#      the outage exceeds the silence window (silence_multiplier * report_interval, floored by
#      min_silence_window_seconds — 90s by default): with cluster-agent gone, the control plane
#      has no way to distinguish "cluster disconnected" from "job silently died" once metrics
#      stop arriving, so failing safe by evicting is the correct, intentional design, not a bug
#      to route around.
# In phase 1 (short outage, well under the silence window), an already-RUNNING job must be
# completely undisturbed (job pods don't depend on cluster-agent's liveness). CLUSTER_EXCLUSIVE.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

AGENT="agent-connloss-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "connectivity-loss-${RUN_ID}" 1.0 1)
signup_and_start "$PE_ID" "$AGENT"

echo "=========================================================="
echo "Phase 1: disconnect, then restore"
echo "=========================================================="
RUNNING_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.03")
S=$(wait_for_status "$RUNNING_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING before disconnect (status=$S) — cannot exercise connectivity loss"
  close_platform_experiment "$PE_ID"
  finish
fi
pass "job reached RUNNING before disconnect"

disconnect_cluster_agent
wait_until "cluster-agent reports disconnected" 15 1 cluster_agent_disconnected || true

assert_stable_status "$RUNNING_JOB" "RUNNING,COMPLETED" 10 \
  "already-running job undisturbed by connectivity loss (should not depend on cluster-agent liveness)"

QUEUED_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02")
S3=$(wait_for_status "$QUEUED_JOB" "RUNNING,COMPLETED" 10 || true)
[[ "$S3" == "RUNNING" || "$S3" == "COMPLETED" ]] \
  && fail "new job was admitted (status=$S3) while cluster-agent is disconnected — capacity should fail closed" \
  || pass "new job correctly did not run while capacity reporting is down (status=$(get_status "$QUEUED_JOB"))"

reconnect_cluster_agent
wait_until "cluster-agent reports connected" 30 1 cluster_agent_connected \
  && pass "cluster-agent reconnected" \
  || fail "cluster-agent did not report connected within timeout after reconnect"

S4=$(wait_for_status "$QUEUED_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" 45 || true)
[[ "$S4" == "RUNNING" || "$S4" == "COMPLETED" ]] \
  && pass "queued job admitted once capacity reporting resumed (status=$S4)" \
  || fail "queued job never got admitted after reconnect (status=$S4)"

for J in "$RUNNING_JOB" "$QUEUED_JOB"; do
  [[ "$(wait_for_status "$J" "COMPLETED,FAILED,EVICTED" 90 || true)" == "COMPLETED" ]] && file_finding "$J"
done

echo ""
echo "=========================================================="
echo "Phase 2: disconnect again, never restore before asserting"
echo "=========================================================="
RUNNING_JOB2=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.03")
S=$(wait_for_status "$RUNNING_JOB2" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job never reached RUNNING before second disconnect (status=$S)"
else
  pass "job reached RUNNING before second (permanent) disconnect"
  disconnect_cluster_agent
  wait_until "cluster-agent reports disconnected" 15 1 cluster_agent_disconnected || true

  # Outlives the silence window (90s default: silence_multiplier=3.0 * report_interval=30s) —
  # long enough for the control plane's fail-safe eviction to fire, since with cluster-agent gone
  # it has no signal at all to tell "cluster disconnected" apart from "job silently died". A job
  # this large (~110s estimated at 0.03h) racing to COMPLETED before that window elapses would
  # only be possible if the pod runs faster than estimated; the deterministic, designed outcome
  # for a truly permanent outage is eviction, not survival.
  S2=$(wait_for_status "$RUNNING_JOB2" "COMPLETED,FAILED,EVICTED" 150 || true)
  if [[ "$S2" == "EVICTED" ]]; then
    [[ "$(get_field "$RUNNING_JOB2" eviction_reason)" == "silent" ]] \
      && pass "already-running job correctly fails safe: evicted (reason=silent) once the outage outlasted the silence window" \
      || fail "already-running job evicted during outage but with unexpected reason: $(get_field "$RUNNING_JOB2" eviction_reason)"
  elif [[ "$S2" == "COMPLETED" ]]; then
    pass "already-running job completed before the silence window elapsed (also acceptable — not a race the job itself should lose either way)"
  else
    fail "already-running job ended as $S2 during a permanent connectivity outage (expected EVICTED reason=silent, or COMPLETED if it raced ahead of the silence window)"
  fi
  [[ "$S2" == "COMPLETED" ]] && file_finding "$RUNNING_JOB2"

  STUCK_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02")
  for i in 1 2 3; do
    sleep 5
    S3=$(get_status "$STUCK_JOB")
    [[ "$S3" != "QUEUED" ]] && { fail "job submitted during a permanent outage left QUEUED unexpectedly (status=$S3) after ${i}x5s"; break; }
  done
  [[ "$(get_status "$STUCK_JOB")" == "QUEUED" ]] \
    && pass "job submitted during a permanent outage stays durably QUEUED, no crash or false admission"

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

close_platform_experiment "$PE_ID"
finish
