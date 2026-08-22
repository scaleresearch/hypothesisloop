#!/usr/bin/env bash
# Two infra faults chained against the same running job, cheapest/least-disruptive first:
#   1. The per-node metrics DaemonSet (node-agent) gets redeployed underneath it — a
#      different pod on the same node, so the job must be completely unaffected and stay on
#      its original node, and a fresh job submitted right after must still get admitted
#      normally (no duplicate/stuck capacity accounting from the restart).
#   2. Its desired-spec identity is externally corrupted — the next stateless pass detects
#      drift and recreates the Job from PostgreSQL.
#   3. Its Kubernetes Job is deleted externally while PostgreSQL still desires it — the next
#      stateless cluster-agent pass recreates it with a new UID.
#   4. Its node then dies outright (cordon + force-delete its pod) — cluster-agent's
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

JOB_HOURS="0.02"
PROBE_JOB_HOURS="0.003"
# H100 has two distinct local worker nodes; pinning this fault target makes genuine rescheduling
# possible after one node is cordoned instead of relying on an interchangeable single-node type.
JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3" "1")
S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
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
  || fail "job's node changed after unrelated DaemonSet restart ($NODE_BEFORE -> $NODE_AFTER)"

POST_DS_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$PROBE_JOB_HOURS")
# QUEUED deliberately excluded here — it's the job's initial state on every submission, so
# including it in the wait target would make this return immediately without ever actually
# waiting for admission (the thing this assertion means to check).
S3=$(wait_for_status "$POST_DS_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S3" == "RUNNING" || "$S3" == "COMPLETED" ]] \
  && pass "new job admitted normally right after the DaemonSet redeploy (status=$S3)" \
  || fail "new job failed to get admitted after the DaemonSet redeploy (status=$S3)"
POST_DS_FINAL=$(wait_for_status "$POST_DS_JOB" "COMPLETED,FAILED,EVICTED" "$(completion_wait_tries "$PROBE_JOB_HOURS")" || true)
[[ "$POST_DS_FINAL" == "COMPLETED" ]] \
  && file_finding "$POST_DS_JOB" \
  || fail "post-DaemonSet job did not complete cleanly (status=$POST_DS_FINAL)"

echo ""
echo "=========================================================="
echo "Fault 2: actual Job drift from PostgreSQL desired spec"
echo "=========================================================="
if [[ "$(get_status "$JOB")" != "RUNNING" ]]; then
  fail "job no longer RUNNING before desired-spec drift fault (status=$(get_status "$JOB"))"
else
  OLD_UID=$(job_uid "$JOB")
  [[ -n "$OLD_UID" ]] || fail "could not read original Kubernetes Job UID"
  corrupt_job_desired_hash "$JOB"
  wait_until "cluster-agent replaces drifted Job" 30 1 job_recreated_with_new_uid "$JOB" "$OLD_UID" \
    && pass "drifted actual Job replaced from current PostgreSQL desired state" \
    || fail "cluster-agent did not replace the drifted actual Job"
fi

echo ""
echo "=========================================================="
echo "Fault 3: actual Job deleted while PostgreSQL still desires it"
echo "=========================================================="
if [[ "$(get_status "$JOB")" != "RUNNING" ]]; then
  fail "job no longer RUNNING before external-delete fault (status=$(get_status "$JOB"))"
else
  OLD_UID=$(job_uid "$JOB")
  [[ -n "$OLD_UID" ]] || fail "could not read pre-delete Kubernetes Job UID"
  delete_job_resource "$JOB"
  wait_until "cluster-agent recreates desired Job" 30 1 job_recreated_with_new_uid "$JOB" "$OLD_UID" \
    && pass "externally deleted Job recreated from PostgreSQL desired state with a new UID" \
    || fail "cluster-agent did not recreate externally deleted desired Job"
  [[ "$(get_status "$JOB")" == "RUNNING" ]] \
    && pass "desired lifecycle remained RUNNING across actual-resource recreation" \
    || fail "desired lifecycle changed unexpectedly after actual Job recreation (status=$(get_status "$JOB"))"
fi

echo ""
echo "=========================================================="
echo "Fault 4: node death mid-run -> cluster-agent self-heal"
echo "=========================================================="
if [[ "$(get_status "$JOB")" != "RUNNING" ]]; then
  fail "job no longer RUNNING before node-death fault (status=$(get_status "$JOB")) — skipping"
else
  # kill_node_running_job returns 1 when the job has no Running pod to kill. Without the `|| true`
  # `set -e` aborts the whole scenario on that path, so the guard below never runs and the failure
  # surfaces as a silent mid-run exit with no diagnostic at all.
  # The experiment is RUNNING, but that is the control plane's view: the pod behind it can be
  # mid-restart or mid-reschedule at this instant, and this fault needs a live one to kill. Retry
  # briefly for a pod rather than reporting the absence of one as an inability to run the test.
  NODE=""
  for _ in $(seq 1 30); do
    NODE=$(kill_node_running_job "$JOB" || true)
    [[ -n "$NODE" ]] && break
    sleep 2
  done
  if [[ -z "$NODE" ]]; then
    fail "could not locate job's pod/node to kill"
  else
    echo "  killed node: $NODE"
    if wait_until "job rescheduled off $NODE" 30 1 job_rescheduled_off "$JOB" "$NODE"; then
      pass "job rescheduled onto a different node"
    else
      fail "job did not reschedule off $NODE within timeout"
    fi
    # One H100 node remains schedulable and the running job consumes one of its eight devices.
    # An 8-device job therefore cannot fit until the cordoned node returns. This proves new
    # admission uses current cluster metrics, not configured/static capacity.
    SHRINK_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$PROBE_JOB_HOURS" "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3" "8")
    SHRINK_STATUS=$(wait_for_status "$SHRINK_JOB" "QUEUED,RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
    [[ "$SHRINK_STATUS" == "QUEUED" ]] \
      && pass "new admission stayed QUEUED while live H100 capacity was reduced" \
      || fail "8-device job became $SHRINK_STATUS while only seven H100 devices were free"
    # The reason is a code optionally followed by ": <detail>" naming what was short (see
    # notAdmittedReasonFor) — match the code, not the whole string, same as job-lifecycle.sh.
    [[ "$(get_field "$SHRINK_JOB" not_admitted_reason)" == capacity_unavailable* ]] \
      && pass "reduced-capacity decision is visible as capacity_unavailable" \
      || fail "reduced-capacity job has wrong not_admitted_reason=$(get_field "$SHRINK_JOB" not_admitted_reason)"

    uncordon_node "$NODE"
    SHRINK_STATUS=$(wait_for_status "$SHRINK_JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
    [[ "$SHRINK_STATUS" == "RUNNING" || "$SHRINK_STATUS" == "COMPLETED" ]] \
      && pass "queued job admitted promptly after real H100 capacity returned" \
      || fail "job did not admit after H100 node returned (status=$SHRINK_STATUS)"
    [[ -z "$(get_field "$SHRINK_JOB" not_admitted_reason)" ]] \
      && pass "capacity failure reason cleared when the job was admitted" \
      || fail "admitted job retained stale not_admitted_reason=$(get_field "$SHRINK_JOB" not_admitted_reason)"

    S=$(wait_for_completion_after_running "$JOB" "$JOB_HOURS" "$ADMISSION_BUDGET_SECONDS" || true)
    [[ "$S" == "COMPLETED" ]] && pass "job completed after reschedule" || fail "job did not complete cleanly after reschedule (status=$S)"

    METRICS=$(dashboard_metrics "$JOB")
    if [[ "$METRICS" != "[]" && -n "$METRICS" ]]; then
      pass "dashboard metrics available across the reschedule gap"
    else
      fail "no metrics returned for completed workload"
    fi
    SHRINK_FINAL=$(wait_for_status "$SHRINK_JOB" "COMPLETED,FAILED,EVICTED" "$(completion_wait_tries "$PROBE_JOB_HOURS")" || true)
    [[ "$SHRINK_FINAL" == "COMPLETED" ]] \
      && file_finding "$SHRINK_JOB" \
      || fail "capacity-growth probe did not complete cleanly (status=$SHRINK_FINAL)"
  fi
fi

echo ""
echo "=========================================================="
echo "Fault attribution: a dead node is not the agent's fault"
echo "=========================================================="
# The job above survived a DaemonSet redeploy, two external mutations of its workload and the
# outright death of its node. None of that was the agent's doing, and its record must not read as
# if it were.
#
# Deliberately NOT asserted here: the free requeue itself and the refund at its ceiling. Both need
# the control plane to actually raise an infrastructure-class eviction, and the two this scenario's
# faults can produce (workload_gone, cluster_unreachable) are only concluded after
# min_silence_window_seconds (300) of silence — longer than this scenario's whole ceiling, and
# lengthening it would buy one requeue where the ceiling needs three
# (scheduler.max_infrastructure_requeues). Those are pinned by the scheduler and settlement unit
# tests. What is observable here is the accounting that makes the requeue free, and the class
# breakdown the agent reads.
ATTEMPTS=$(get_field "$JOB" attempt_count)
INFRA_REQUEUES=$(get_field "$JOB" infra_requeue_count)
echo "  $JOB: attempt_count=${ATTEMPTS} infra_requeue_count=${INFRA_REQUEUES}"
if [[ -z "$ATTEMPTS" || -z "$INFRA_REQUEUES" ]]; then
  # An empty field is not a zero: a control plane serving only one counter cannot be charging the
  # agent for its own attempts alone, whatever that counter reads.
  fail "$JOB does not report attempt_count/infra_requeue_count — the retry budget and the generation counter are not tracked apart"
else
  # The agent's allowance is the difference of the two (domain.Experiment.RetriesUsed): every
  # requeue advances attempt_count, and an infrastructure one advances infra_requeue_count with
  # it. A job that lost its node must therefore show a difference of zero.
  [[ $(( ATTEMPTS - INFRA_REQUEUES )) -eq 0 ]] \
    && pass "losing its node cost the job none of the agent's retry allowance (retries_used=0)" \
    || fail "$JOB used $(( ATTEMPTS - INFRA_REQUEUES )) of the agent's retry allowance (attempt_count=$ATTEMPTS infra_requeue_count=$INFRA_REQUEUES) — a node death was charged to the agent"
fi

# Every job this agent submitted here either completed or is still running: not one of them failed
# by its own fault, so its workload-class count must be exactly zero — and it must be a zero the
# platform computed, not a missing field.
WORKLOAD_N=$(eviction_class_count "$PE_ID" "$AGENT" workload)
echo "  evictions by class: workload=${WORKLOAD_N} infrastructure=$(eviction_class_count "$PE_ID" "$AGENT" infrastructure) policy=$(eviction_class_count "$PE_ID" "$AGENT" policy)"
[[ "$WORKLOAD_N" == "0" ]] \
  && pass "no infrastructure fault landed in the agent's own failure count (workload=0)" \
  || fail "evictions_by_class.workload is '$WORKLOAD_N', expected a computed 0 — node and DaemonSet faults are the environment's, and a missing field is not a zero"

read -r CLASS_TOTAL REASON_TOTAL UNCLASSIFIED <<< "$(eviction_class_coverage "$PE_ID" "$AGENT")" || true
[[ "$CLASS_TOTAL" == "$REASON_TOTAL" && "$UNCLASSIFIED" == "0" ]] \
  && pass "every evicted job is accounted for by exactly one class ($CLASS_TOTAL of $REASON_TOTAL, none unclassified)" \
  || fail "class breakdown does not account for the evictions: by_class=$CLASS_TOTAL by_reason=$REASON_TOTAL unclassified=$UNCLASSIFIED"

close_platform_experiment "$PE_ID"
finish
