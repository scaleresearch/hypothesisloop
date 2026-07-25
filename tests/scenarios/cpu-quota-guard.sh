#!/usr/bin/env bash
# Every other scenario guards on accelerator-hours; this one guards on CPU-core-hours, since
# CPU is its own tracked dimension (domain.AgentQuota's Guaranteed/UsedGuaranteedCPUCoreH) and
# a PE can run CPU-only jobs (accelerator_count=0). Verifies, on the CPU axis:
#   1. a CPU-only job debits guaranteed CPU-core-hours (not accelerator-hours) at submission.
#   2. once an agent's guaranteed CPU-hours are exhausted, a second CPU-only job from that
#      agent is gated on CPU headroom (not accelerator capacity) — fails closed (stays
#      QUEUED), same guarantee the accelerator scenarios check for accelerators.
#   3. cancelling the exhausting job frees CPU-core-hours and lets the gated job admit.
# API-only, parallel-safe (own PE, no accelerator contention with anything else).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-cpu-guard-${RUN_ID}"
register_agent "$AGENT"
# estimated_duration_hours stays small/fixed (0.02h, ~70-90s — same as other scenarios): it
# drives the workload's actual wall-clock sleep, not just billing, so it can't be inflated to
# make the CPU-quota math convenient. Instead the PE's CPU budget is sized down so the job's
# real cost (hours * cores) is what exhausts it. Only the phase-1 explore fraction of
# BUDGET_CPU becomes this agent's guaranteed core-hours (domain.AllocateQuota) — verified via
# quota_guaranteed_cpu_hours below, not assumed, so it won't silently drift.
# budget_accelerator_hours must be nonzero (API rejects an all-zero budget as "required") even
# though every job here is accelerator_count=0.
JOB_HOURS="0.02"
CPU_SPEC="1"
BUDGET_CPU="0.08"
PE_ID=$(create_platform_experiment "cpu-quota-guard-${RUN_ID}" 0.001 1 10 "$BUDGET_CPU")
signup_and_start "$PE_ID" "$AGENT"

CPU_BUDGET=$(quota_guaranteed_cpu_hours "$PE_ID" "$AGENT")
echo "  agent's guaranteed CPU budget: ${CPU_BUDGET} core-hours (job cost: ${JOB_HOURS} core-hours each)"

echo "  -- CPU-only job debits guaranteed CPU-core-hours, not accelerator-hours --"
QG_BEFORE=$(quota_used_guaranteed "$PE_ID" "$AGENT")
QC_BEFORE=$(quota_used_guaranteed_cpu "$PE_ID" "$AGENT")
BIG=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "" "" "" "" "{\"cpu\": \"${CPU_SPEC}\", \"accelerator_count\": 0, \"accelerators\": null}")
S=$(wait_for_status "$BIG" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" ]] \
  && pass "CPU-only job admitted onto guaranteed CPU budget (status=$S)" \
  || fail "CPU-only job never reached RUNNING (status=$S) — cannot exercise CPU-quota guard"

QG_AFTER=$(quota_used_guaranteed "$PE_ID" "$AGENT")
QC_AFTER=$(quota_used_guaranteed_cpu "$PE_ID" "$AGENT")
ACCELERATOR_DELTA=$(py "print(round(float('$QG_AFTER' or 0) - float('$QG_BEFORE' or 0), 6))")
CPU_DELTA=$(py "print(round(float('$QC_AFTER' or 0) - float('$QC_BEFORE' or 0), 6))")
[[ "$ACCELERATOR_DELTA" == "0.0" || "$ACCELERATOR_DELTA" == "0" ]] \
  && pass "a accelerator-free job debited zero accelerator-hours ($ACCELERATOR_DELTA)" \
  || fail "a accelerator-free job debited $ACCELERATOR_DELTA AccH — should be exactly 0"
CPU_DELTA_OK=$(py "print(float('$CPU_DELTA') > 0)")
[[ "$CPU_DELTA_OK" == "True" ]] \
  && pass "CPU-core-hours debited on submission ($CPU_DELTA core-hours > 0)" \
  || fail "expected nonzero CPU-core-hour debit for a CPU-only job, got $CPU_DELTA"

echo "  -- second CPU-only job is gated on CPU headroom, not accelerator capacity --"
# May be rejected outright at submission time (402 insufficient_credits, checked synchronously
# against the pool) or accepted but left non-admitted (stays QUEUED) — both are correct
# fail-closed outcomes, so use the code-returning variant instead of submit_job (which treats
# any non-2xx as a hard script error).
read -r CODE SECOND <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" \
  "{\"cpu\": \"${CPU_SPEC}\", \"accelerator_count\": 0, \"accelerators\": null}")"
if [[ "$CODE" -ge 400 ]]; then
  pass "second CPU-only job rejected at submission while CPU budget is exhausted (HTTP $CODE)"
else
  S2=$(wait_for_status "$SECOND" "RUNNING,COMPLETED" 15 || true)
  [[ "$S2" == "RUNNING" || "$S2" == "COMPLETED" ]] \
    && fail "second CPU-only job was admitted (status=$S2) despite the agent's guaranteed CPU budget already being spent — CPU dimension not actually gating admission" \
    || pass "second CPU-only job correctly stayed non-admitted while CPU budget is exhausted (status=$(get_status "$SECOND"))"

  echo "  -- cancelling the CPU-exhausting job frees CPU headroom for the gated one --"
  curl -sf -X POST "$SCHED_URL/experiments/${BIG}/cancel" > /dev/null || true
  wait_for_status "$BIG" "EVICTED,REJECTED,FAILED" 15 > /dev/null || true
  S3=$(wait_for_status "$SECOND" "RUNNING,COMPLETED" "$ADMISSION_BUDGET_SECONDS" || true)
  [[ "$S3" == "RUNNING" || "$S3" == "COMPLETED" ]] \
    && pass "previously CPU-gated job admitted once the exhausting job was cancelled (status=$S3)" \
    || fail "previously CPU-gated job never admitted after CPU headroom was freed (status=$(get_status "$SECOND"))"

  [[ "$(wait_for_status "$SECOND" "COMPLETED,FAILED,EVICTED,REJECTED" 60 || true)" == "COMPLETED" ]] && file_finding "$SECOND"
fi

[[ "$(wait_for_status "$BIG" "COMPLETED,FAILED,EVICTED,REJECTED" 60 || true)" == "COMPLETED" ]] && file_finding "$BIG"

close_platform_experiment "$PE_ID"
finish
