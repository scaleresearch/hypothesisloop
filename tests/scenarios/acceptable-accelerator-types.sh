#!/usr/bin/env bash
# acceptable_accelerator_types: a job that hard-requires exactly accelerator_type must still get selected a
# concrete flavor at admission time from among accelerator_type + acceptable_accelerator_types — not just at
# Kubernetes node-affinity time. If the requested flavor is fully saturated but a listed
# alternative has free capacity, the job must be admitted onto the alternative instead of
# sitting QUEUED forever next to idle accelerators.
#
# Regression test for findings.md's P1 "acceptable_accelerator_types cannot be scheduled correctly":
# scheduler capacity selection and reservation used to use only Experiment.AcceleratorType (the
# requested flavor) — see resolveClusterAndFootprint in loop_tick.go for the fix.
#
# H200 (controlplane/settings/openresearch.yaml) is saturated by the filler job below —
# whether that lands it as RUNNING/ADMITTED (real live capacity exists and got claimed) or an
# outright REJECTED (no live H200 capacity exists at all, e.g. this dev cluster's fake-node set
# doesn't happen to include an H200-labeled node — see localdev/k3s-macos/add-fake-nodes.sh) both prove
# the same thing: zero H200 capacity is available afterward, either way. H100 is left untouched
# so it's the only viable landing spot for the flexible job. API-only, parallel-safe (uses accelerator
# types not touched by other scenarios' saturation logic) as long as it doesn't run
# concurrently with another scenario that also saturates H200 or H100.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

REQUESTED="H200"
ALTERNATIVE="H100"
AGENTS=("agent-agt-a-${RUN_ID}" "agent-agt-b-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
PE_ID=$(create_platform_experiment "acceptable-accelerator-types-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  ==> saturating all ${REQUESTED} capacity with a filler guaranteed job (16 is the per-job accelerator_count cap — quota.max_accelerator_count_per_job in openresearch.yaml — and deliberately exceeds any real ${REQUESTED} node's supply)..."
FILLER=$(submit_job "$PE_ID" "${AGENTS[0]}" "guaranteed" "0.05" "$REQUESTED" 16)
# The filler proves "zero ${REQUESTED} capacity available" by simply never reaching
# RUNNING/ADMITTED — whether it settles as REJECTED (synchronous "impossible" rejection) or
# stays QUEUED (admission keeps retrying every tick and never finds room) is an implementation
# detail; either outcome (or a wait-window timeout while still QUEUED) equally proves the
# capacity claim below.
FILLER_STATUS=$(wait_for_status "$FILLER" "RUNNING,ADMITTED,REJECTED" 15 || true)
if [[ "$FILLER_STATUS" == "RUNNING" || "$FILLER_STATUS" == "ADMITTED" ]]; then
  echo "  [WARN] filler job was admitted (status=$FILLER_STATUS) — ${REQUESTED} apparently has real capacity in this cluster; saturation assumption may not hold"
fi

echo "  ==> submitting a job requesting ${REQUESTED} with acceptable_accelerator_types=[${REQUESTED}, ${ALTERNATIVE}]..."
read -r CODE FLEX_ID <<< "$(submit_job_expect_code "$PE_ID" "${AGENTS[1]}" "guaranteed" "0.02" \
  "{\"accelerator_type\": \"${REQUESTED}\", \"accelerator_count\": 1, \"acceptable_accelerator_types\": [\"${REQUESTED}\", \"${ALTERNATIVE}\"]}")"

if [[ "$CODE" -ge 400 ]]; then
  fail "submission with an acceptable alternative was rejected outright (HTTP $CODE) instead of admitted onto ${ALTERNATIVE}"
else
  S=$(wait_for_status "$FLEX_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 30 || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    ADMITTED_TYPE=$(get_field "$FLEX_ID" accelerator_type)
    [[ "$ADMITTED_TYPE" == "$ALTERNATIVE" ]] \
      && pass "job admitted onto free alternative ${ALTERNATIVE} while requested ${REQUESTED} was saturated (status=$S)" \
      || fail "job reached status=$S but admitted accelerator_type=$ADMITTED_TYPE (expected ${ALTERNATIVE} — requested ${REQUESTED} was saturated)"
  else
    fail "job stayed QUEUED (status=$S) with idle ${ALTERNATIVE} capacity available — admission only considered the requested flavor, not acceptable_accelerator_types"
  fi
  [[ "$(wait_for_status "$FLEX_ID" "COMPLETED,FAILED,EVICTED,QUEUED" 100 || true)" == "COMPLETED" ]] && file_finding "$FLEX_ID"
fi

wait_for_status "$FILLER" "COMPLETED,FAILED,EVICTED,REJECTED,QUEUED" 100 > /dev/null || true
close_platform_experiment "$PE_ID"
finish
