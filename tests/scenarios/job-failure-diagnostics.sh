#!/usr/bin/env bash
# What the platform can tell an agent about a job that died. Both halves are about diagnosis
# surviving cleanup, which is where this used to fail silently:
#
#   1. A crashing job's own output must still be readable after its workload is gone. Terminal
#      status removes a job from desired state, and the cluster-agent's next reconcile pass deletes
#      the workload — pod, logs and container termination reason with it. The runtime captures and
#      pushes that output *before* deleting; without it, whether a failure was diagnosable came
#      down to whether a status push happened to land first, and an agent debugging a dead job saw
#      a bare FAILED with nothing attached.
#   2. A job that can never be scheduled (a bad image) is evicted early as `unschedulable` and
#      refunded, rather than sitting on a reservation until the generic stuck-pending timeout.
#
# API-only and parallel-safe: neither job consumes an accelerator for any real length of time, and
# neither touches shared cluster state.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-failure-diag-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "failure-diagnostics-${RUN_ID}" 10.0 1 5)
signup_and_start "$PE_ID" "$AGENT"

echo "  -- a crashing job's output outlives its workload --"
# A marker string this scenario can find unambiguously in the log tail, and a distinctive non-zero
# exit code. max_retries=0 so the job reaches FAILED on the first attempt instead of retrying three
# times inside the scenario's ceiling.
MARKER="HYPOTHESISLOOP-CRASH-MARKER-${RUN_ID}"
CRASH_OVERRIDE=$(py "
import json
print(json.dumps({
  'command': ['/bin/sh', '-c'],
  'args': ['echo \"$MARKER\" >&2; exit 42'],
  'max_retries': 0,
}))
")
CRASH_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "" "" "" "" "$CRASH_OVERRIDE")
echo "  ==> $CRASH_JOB submitted (exits 42 after printing its marker)"

CS=$(wait_for_status "$CRASH_JOB" "FAILED,EVICTED,COMPLETED" "$ADMISSION_BUDGET_SECONDS" || true)
# FAILED specifically: a job whose container exits non-zero failed. Accepting EVICTED too would let
# an unrelated policy eviction satisfy this scenario while the crash diagnosis went missing.
[[ "$CS" == "FAILED" ]] \
  && pass "crashing job reached FAILED" \
  || fail "crashing job ended as '$CS' — expected FAILED (exit 42 is a container failure, not a policy eviction)"

# The workload is deleted on a reconcile pass *after* the status transition, so poll rather than
# sampling once: the assertion is that the output is still there once cleanup has had time to run,
# not that it is there immediately.
# ?n well above the default 10: the tail is sectioned (failed attempt / current attempt) and the
# marker must not be pushed out of a short window by section headers.
job_logs() { curl -sf "$API_URL/experiments/$1/logs?n=200" || true; }
logs_have_marker() { job_logs "$1" | grep -q "$MARKER"; }
if wait_until "crashed job's log tail reaches the control plane" 30 1 logs_have_marker "$CRASH_JOB"; then
  pass "crashed job's own stderr is readable through the API"
else
  fail "crashed job's output never reached the control plane — its log tail died with its workload"
fi

# Deletion is what used to destroy this. Give the cluster-agent several reconcile passes to remove
# the workload, then re-read: the record must still explain the failure.
sleep 15
logs_have_marker "$CRASH_JOB" \
  && pass "log tail still readable after the workload was cleaned up" \
  || fail "log tail disappeared once the workload was deleted — the final capture did not survive cleanup"

# phase_detail is a nested object on the experiment ({reason, message, restart_count}), enriched
# from the metrics store on read — not a flat field.
phase_detail_field() {
  curl -sf "$API_URL/experiments/$1" \
    | py "import sys,json; print((json.load(sys.stdin).get('phase_detail') or {}).get('$2',''))"
}
REASON=$(phase_detail_field "$CRASH_JOB" reason)
MESSAGE=$(phase_detail_field "$CRASH_JOB" message)
echo "  phase_detail: reason='${REASON}' message='${MESSAGE}'"
# The specific reason, not merely a non-empty one: any nonblank string would otherwise satisfy this
# while the real termination detail was lost (domain.PhaseReasonContainerFailed).
[[ "$REASON" == "container_failed" ]] \
  && pass "terminal record names the container failure (reason=$REASON)" \
  || fail "phase_detail.reason is '$REASON', expected container_failed — the container's own termination detail did not survive"
# The runtime formats this as "container exited with code N" (see terminatedPhaseDetail), so the
# exit code the workload actually returned must be recoverable from the record.
[[ "$MESSAGE" == *"container exited with code 42"* ]] \
  && pass "terminal record carries the container's real exit code" \
  || fail "phase_detail.message is '$MESSAGE', expected it to contain 'container exited with code 42'"

echo "  -- an unschedulable job is evicted early and refunded, not left to time out --"
USED_BEFORE=$(quota_used_guaranteed "$PE_ID" "$AGENT")
BAD_IMAGE_OVERRIDE='{"image": "localhost/hypothesisloop-image-that-does-not-exist:nope", "max_retries": 0}'
BAD_JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "" "" "" "" "$BAD_IMAGE_OVERRIDE")
echo "  ==> $BAD_JOB submitted with an image that cannot be pulled"

# Two waits, not one. A job that cannot pull its image is only *detectable* as unschedulable
# once it has actually been placed on a node -- before that it is an ordinary job waiting for
# capacity, and the fast group's other scenarios can hold the accelerators for the whole
# admission budget. So the budget here is admission plus detection, not admission alone.
# Detection itself takes seconds (the runtime reports image_pull_failed on its first status
# push); the margin is generous so the assertion never turns into a measurement of cluster load.
# It stays far below the 300s generic stuck-pending timeout (scheduler.stuck_pending_timeout_
# seconds), which is what makes a terminal state here proof of the early, image-specific verdict
# rather than of waiting the job out.
UNSCHEDULABLE_BUDGET=$(( ADMISSION_BUDGET_SECONDS + 60 ))
BS=$(wait_for_status "$BAD_JOB" "EVICTED,FAILED,REJECTED" "$UNSCHEDULABLE_BUDGET" || true)
if [[ "$BS" == "EVICTED" || "$BS" == "FAILED" ]]; then
  pass "unpullable job was terminated rather than left pending ($BS)"
  BAD_REASON=$(get_field "$BAD_JOB" eviction_reason)
  echo "  bad-image phase_detail: reason='$(phase_detail_field "$BAD_JOB" reason)' message='$(phase_detail_field "$BAD_JOB" message)'"
  # unschedulable specifically: reaching a terminal state inside UNSCHEDULABLE_BUDGET, well
  # under the 300s generic stuck-pending timeout, can only be the early image-specific verdict —
  # which is the behaviour being tested: believe the runtime's phase detail instead of waiting
  # out a job that can never start.
  [[ "$BAD_REASON" == "unschedulable" ]] \
    && pass "evicted as unschedulable — the runtime's phase detail was believed, not waited out" \
    || fail "unpullable job ended with eviction_reason='$BAD_REASON', expected unschedulable"

  # It never ran, so it consumed nothing: its reservation must come back in full.
  refunded() {
    local now
    now=$(quota_used_guaranteed "$PE_ID" "$AGENT")
    py "import sys; sys.exit(0 if abs(float('$now' or 0) - float('$USED_BEFORE' or 0)) < 1e-6 else 1)"
  }
  if wait_until "unschedulable job's reservation returned" 30 1 refunded; then
    pass "reservation was returned in full — a job that never ran is billed nothing"
  else
    fail "used_guaranteed_acch is $(quota_used_guaranteed "$PE_ID" "$AGENT"), was $USED_BEFORE — an unschedulable job kept part of its reservation"
  fi
else
  fail "unpullable job is '$BS' after ${UNSCHEDULABLE_BUDGET}s — nothing terminated it"
fi

echo "  -- the agent's own failure is attributed to the agent --"
# A bad image reference is the submission's own doing (domain.EvictionUnschedulable is
# FaultWorkload), so none of the infrastructure-class consequences may apply to it: no free
# requeue, and it must not read as the environment's fault in the agent's record.
#
# No wait is needed for the "not requeued" half. infra_requeue_count only ever increments (see
# ExperimentsStore.RequeueInfrastructureFault), so reading 0 proves no free requeue has happened
# rather than that one has not happened yet — polling for a requeue that must never come would
# only spend budget to prove the same thing later.
if [[ "$BS" == "EVICTED" ]]; then
  BAD_ATTEMPTS=$(get_field "$BAD_JOB" attempt_count)
  BAD_INFRA=$(get_field "$BAD_JOB" infra_requeue_count)
  echo "  bad-image accounting: attempt_count=${BAD_ATTEMPTS} infra_requeue_count=${BAD_INFRA}"
  [[ "$BAD_INFRA" == "0" ]] \
    && pass "workload failure was not requeued for free (infra_requeue_count=0)" \
    || fail "unschedulable job has infra_requeue_count='$BAD_INFRA', expected 0 — its own bad image was treated as the environment's fault"
  # attempt_count is the generation counter every requeue advances, free or not; the agent's own
  # allowance is the difference (domain.Experiment.RetriesUsed). max_retries=0 here, so the job
  # ended on its first attempt and both counters must still be at zero.
  [[ "$BAD_ATTEMPTS" == "0" ]] \
    && pass "workload failure ended on its first attempt, as max_retries=0 requires" \
    || fail "unschedulable job has attempt_count='$BAD_ATTEMPTS', expected 0 — it was retried despite max_retries=0"
fi

# The breakdown an agent actually reads. `unschedulable` must land under workload, and nothing
# this scenario produced may land under infrastructure — if the classification were reverted or
# this reason moved class, these two flip together.
WORKLOAD_N=$(eviction_class_count "$PE_ID" "$AGENT" workload)
INFRA_N=$(eviction_class_count "$PE_ID" "$AGENT" infrastructure)
echo "  evictions by class: workload=${WORKLOAD_N} infrastructure=${INFRA_N}"
[[ "$WORKLOAD_N" != "-" && "$WORKLOAD_N" -ge 1 ]] \
  && pass "the agent's own failure is reported under the workload class" \
  || fail "evictions_by_class.workload is '$WORKLOAD_N' — a bad image reference is the agent's own fault and must be counted as one"
[[ "$INFRA_N" == "0" ]] \
  && pass "nothing this agent did was charged to the environment (infrastructure=0)" \
  || fail "evictions_by_class.infrastructure is '$INFRA_N', expected 0 — the agent's own failure was blamed on the environment"

read -r CLASS_TOTAL REASON_TOTAL UNCLASSIFIED <<< "$(eviction_class_coverage "$PE_ID" "$AGENT")" || true
[[ "$CLASS_TOTAL" == "$REASON_TOTAL" && "$UNCLASSIFIED" == "0" ]] \
  && pass "every evicted job is accounted for by exactly one class ($CLASS_TOTAL of $REASON_TOTAL, none unclassified)" \
  || fail "class breakdown does not account for the evictions: by_class=$CLASS_TOTAL by_reason=$REASON_TOTAL unclassified=$UNCLASSIFIED"

close_platform_experiment "$PE_ID"
finish
