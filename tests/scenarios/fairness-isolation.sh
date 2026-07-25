#!/usr/bin/env bash
# Two platform experiments sharing one agent must never leak usage into each other's quota
# ledger, and a CPU-only (accelerator_count=0) job must be accepted into the fairness pool
# without requiring an accelerator dimension. API-only, parallel-safe (two fresh PEs of its own).
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

AGENT="agent-fair-${RUN_ID}"
register_agent "$AGENT"
PE1=$(create_platform_experiment "fairness-1-${RUN_ID}" 1.0 1)
PE2=$(create_platform_experiment "fairness-2-${RUN_ID}" 1.0 1)
signup_and_start "$PE1" "$AGENT"
signup_and_start "$PE2" "$AGENT"

# Establish nonzero usage in PE1 first, so a PE1/PE2 quota-key collision would be visible as
# contamination in PE2's ledger below.
J1=$(submit_job "$PE1" "$AGENT" "guaranteed" "0.02")
wait_for_status "$J1" "COMPLETED,FAILED,EVICTED,QUEUED,RUNNING" 60 > /dev/null || true

read -r CODE CPU_ONLY_ID <<< "$(submit_job_expect_code "$PE2" "$AGENT" "guaranteed" "0.02" \
  '{"accelerator_count": 0, "accelerators": null}')"
if [[ "$CODE" -lt 400 ]]; then
  wait_for_status "$CPU_ONLY_ID" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED,REJECTED" 30 > /dev/null || true
  pass "CPU-only (accelerator_count=0) job accepted into PE2's fairness pool without requiring a accelerator dimension"
else
  fail "CPU-only (accelerator_count=0) job submission was rejected outright (HTTP $CODE)"
fi

wait_for_status "$J1" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true
PE1_USED=$(quota_used_guaranteed "$PE1" "$AGENT")
PE2_USED=$(quota_used_guaranteed "$PE2" "$AGENT")
echo "  agent $AGENT: PE1 used_guaranteed_acch=$PE1_USED, PE2 used_guaranteed_acch=$PE2_USED"
# PE2's only job requested no accelerator, so any nonzero AccH usage recorded against it can
# only have leaked in from PE1 via a quota-map key collision (AgentID-only vs (AgentID, PEID)).
PE2_CLEAN=$(py "print(float('$PE2_USED' or 0) == 0.0)")
[[ "$PE2_CLEAN" == "True" ]] \
  && pass "PE2's guaranteed usage is unaffected by PE1's — no cross-PE quota leakage" \
  || fail "PE2 shows nonzero guaranteed usage ($PE2_USED) from a accelerator-free job — quota map may key on AgentID alone, leaking PE1's usage"

close_platform_experiment "$PE1"
close_platform_experiment "$PE2"
finish
