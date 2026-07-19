#!/usr/bin/env bash
# Admission correctness on the CPU dimension: mixed CPU+accelerator jobs are jointly fit-
# checked, fractional (millicore) CPU survives verbatim to the pod spec, a CPU-only job
# (no GPU dimension at all) is admitted and billed on CPU alone, and a submission missing a
# required resource field is rejected rather than silently defaulted. API-only, parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-mixed-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "mixed-admission-${RUN_ID}" 1.0 1)
signup_and_start "$PE_ID" "$AGENT"

echo "  -- impossible CPU request should not be admitted --"
read -r CODE IMPOSSIBLE_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" \
  '{"cpu": "999999", "gpu_type": "T4", "gpu_count": 1}')"
if [[ "$CODE" -lt 400 ]]; then
  S=$(wait_for_status "$IMPOSSIBLE_ID" "RUNNING,ADMITTED" 10 || true)
  [[ "$S" == "RUNNING" || "$S" == "ADMITTED" ]] \
    && fail "job with an impossible CPU request (999999 cores) was admitted — mixed CPU+accelerator fit not jointly checked" \
    || pass "job with an impossible CPU request stayed non-admitted (status=$S)"
else
  pass "job with an impossible CPU request was rejected at submission (HTTP $CODE)"
fi

echo "  -- small, fittable mixed CPU+GPU job should admit and run --"
read -r CODE OK_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" \
  '{"cpu": "250m", "gpu_type": "T4", "gpu_count": 1}')"
if [[ "$CODE" -lt 400 ]]; then
  S=$(wait_for_status "$OK_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
  [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]] \
    && pass "mixed CPU(250m)+GPU(1xT4) job admitted where both fit (status=$S)" \
    || fail "mixed CPU+GPU job never admitted (status=$S)"
  [[ "$(wait_for_status "$OK_ID" "COMPLETED,FAILED,EVICTED" 100 || true)" == "COMPLETED" ]] && file_finding "$OK_ID"
else
  fail "submission of a small, fittable mixed CPU+GPU job was rejected outright (HTTP $CODE)"
fi

echo "  -- fractional CPU (333m) must retain millicore precision through to the pod spec --"
read -r CODE FRAC_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" \
  '{"cpu": "333m", "gpu_type": "T4", "gpu_count": 1}')"
if [[ "$CODE" -lt 400 ]]; then
  S=$(wait_for_status "$FRAC_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    POD=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$FRAC_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    CPU_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.cpu}' 2>/dev/null || true)
    CPU_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.cpu}' 2>/dev/null || true)
    [[ "$CPU_REQ" == "333m" && "$CPU_LIM" == "333m" ]] \
      && pass "fractional CPU (333m) retained millicore precision through to the pod spec" \
      || fail "fractional CPU precision lost: got request=$CPU_REQ limit=$CPU_LIM, expected 333m/333m"
  else
    fail "fractional CPU job never reached RUNNING/COMPLETED (status=$S)"
  fi
  [[ "$(wait_for_status "$FRAC_ID" "COMPLETED,FAILED,EVICTED" 100 || true)" == "COMPLETED" ]] && file_finding "$FRAC_ID"
else
  fail "submission of a valid fractional-CPU (333m) job was rejected outright (HTTP $CODE)"
fi

echo "  -- CPU-only job (no GPU dimension at all) should admit and bill on CPU alone --"
read -r CODE CPU_ONLY_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" \
  '{"cpu": "500m", "gpu_count": 0, "gpu_type": null, "acceptable_gpu_types": null}')"
if [[ "$CODE" -lt 400 ]]; then
  S=$(wait_for_status "$CPU_ONLY_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    ADMITTED_GPU_COUNT=$(get_field "$CPU_ONLY_ID" gpu_count)
    EST_COST=$(get_field "$CPU_ONLY_ID" estimated_cost_t4h)
    [[ "$ADMITTED_GPU_COUNT" == "0" && ( "$EST_COST" == "0" || "$EST_COST" == "0.0" ) ]] \
      && pass "CPU-only job admitted with gpu_count=0 and zero GPU-hour billing (status=$S)" \
      || fail "CPU-only job admitted but gpu_count=$ADMITTED_GPU_COUNT estimated_cost_t4h=$EST_COST (expected 0/0 — a GPU-free job must not bill GPU-hours)"
  else
    fail "CPU-only job never reached RUNNING/COMPLETED (status=$S) — CPU-only admission may be broken"
  fi
  [[ "$(wait_for_status "$CPU_ONLY_ID" "COMPLETED,FAILED,EVICTED" 100 || true)" == "COMPLETED" ]] && file_finding "$CPU_ONLY_ID"
else
  fail "submission of a CPU-only (gpu_count=0) job was rejected outright (HTTP $CODE)"
fi

echo "  -- submission with an unset required resource must be rejected, not defaulted --"
read -r CODE UNSET_ID <<< "$(submit_job_expect_code "$PE_ID" "$AGENT" "guaranteed" "0.02" \
  '{"cpu": null, "gpu_type": "T4", "gpu_count": 1}')"
if [[ "$CODE" -ge 400 ]]; then
  pass "submission with unset cpu request rejected at submission time (HTTP $CODE)"
else
  S=$(wait_for_status "$UNSET_ID" "RUNNING,ADMITTED,REJECTED,FAILED" 15 || true)
  [[ "$S" == "REJECTED" ]] \
    && pass "submission with unset cpu request rejected (status=REJECTED)" \
    || fail "submission with unset cpu request was accepted (HTTP $CODE, status=$S) instead of rejected"
fi

close_platform_experiment "$PE_ID"
finish
