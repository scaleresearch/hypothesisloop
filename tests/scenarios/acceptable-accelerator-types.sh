#!/usr/bin/env bash
# acceptable_accelerator_types: a job must get its concrete flavor selected at admission time
# from accelerator_type + acceptable_accelerator_types, not just at k8s node-affinity time. If
# the requested flavor is saturated but a listed alternative is free, the job must admit onto
# the alternative instead of sitting QUEUED next to idle accelerators.
#
# Regression test for findings.md's P1 "acceptable_accelerator_types cannot be scheduled correctly":
# scheduler capacity selection used to use only Experiment.AcceleratorType (the
# requested flavor) — see resolveClusterAndFootprint in loop_tick.go for the fix.
#
# Cluster-exclusive because the assertion deliberately consumes one live accelerator pool.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

REQUESTED=nvidia.com/gpu.product=NVIDIA-L40
ALTERNATIVE=nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe
REQUESTED_CAPACITY=$(kubectl get nodes -l nvidia.com/gpu.product=NVIDIA-L40 -o json | py '
import json, sys
print(sum(int(node["status"]["allocatable"].get("nvidia.com/gpu", 0)) for node in json.load(sys.stdin)["items"]))
')
ALTERNATIVE_CAPACITY=$(kubectl get nodes -l nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe -o json | py '
import json, sys
print(sum(int(node["status"]["allocatable"].get("nvidia.com/gpu", 0)) for node in json.load(sys.stdin)["items"]))
')
[[ "$REQUESTED_CAPACITY" -gt 0 && "$ALTERNATIVE_CAPACITY" -gt 0 ]] \
  || { echo "acceptable accelerator scenario requires live L40 and A100 native node inventory" >&2; exit 2; }

# The two counts above are ground truth, read straight off the vendor's own node labels. The
# catalog an agent reads before submitting must agree with them, under the very strings a job
# spec takes — that is the whole contract of a driver-published accelerator type, and the only
# check here that costs no job and no extra wall time.
#
# Splitting matters as much as the totals: L40 and A100 both advertise nvidia.com/gpu, so a
# catalog that keyed on the extended resource alone would report one merged pool that matches no
# submittable accelerator_type, and an agent picking from it would queue forever.
echo "  -- catalog reports live capacity under the driver-published type strings --"
CATALOG=$(curl -sf "$QUOTA_URL/resource-catalog/capacity")
for pair in "${REQUESTED}:${REQUESTED_CAPACITY}" "${ALTERNATIVE}:${ALTERNATIVE_CAPACITY}"; do
  want_type="${pair%:*}" want_total="${pair##*:}"
  got_total=$(printf '%s' "$CATALOG" | WANT="$want_type" py '
import json, os, sys
want = os.environ["WANT"]
print(sum(a["total"] for c in json.load(sys.stdin)["clusters"]
          for a in c["accelerators"] if a["accelerator_type"] == want))
')
  [[ "$got_total" == "$want_total" ]] \
    && pass "catalog reports ${want_type} total=${got_total}, matching live node inventory" \
    || fail "catalog reports ${want_type} total=${got_total}; live node inventory has ${want_total}"
done

AGENTS=("agent-agt-flex-${RUN_ID}")
for ((i = 1; i <= REQUESTED_CAPACITY; i++)); do AGENTS+=("agent-agt-fill-${i}-${RUN_ID}"); done
for a in "${AGENTS[@]}"; do register_agent "$a"; done
PE_ID=$(create_platform_experiment "acceptable-accelerator-types-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  ==> unknown accelerator types must fail at the API boundary..."
read -r UNKNOWN_REQUESTED_CODE _ <<< "$(submit_job_expect_code "$PE_ID" "${AGENTS[0]}" "guaranteed" "0.01" \
  '{"accelerator_count":1,"accelerator_type":"nvidia.com/gpu.product=not-in-catalog"}')"
[[ "$UNKNOWN_REQUESTED_CODE" -ge 400 && "$UNKNOWN_REQUESTED_CODE" -lt 500 ]] \
  && pass "unknown requested accelerator type failed fast (HTTP $UNKNOWN_REQUESTED_CODE)" \
  || fail "unknown requested accelerator type returned HTTP $UNKNOWN_REQUESTED_CODE; expected a client error"

read -r UNKNOWN_ACCEPTABLE_CODE _ <<< "$(submit_job_expect_code "$PE_ID" "${AGENTS[0]}" "guaranteed" "0.01" \
  "{\"accelerator_count\":1,\"accelerator_type\":\"${ALTERNATIVE}\",\"acceptable_accelerator_types\":[\"nvidia.com/gpu.product=not-in-catalog\"]}")"
[[ "$UNKNOWN_ACCEPTABLE_CODE" -ge 400 && "$UNKNOWN_ACCEPTABLE_CODE" -lt 500 ]] \
  && pass "unknown acceptable accelerator type failed fast (HTTP $UNKNOWN_ACCEPTABLE_CODE)" \
  || fail "unknown acceptable accelerator type returned HTTP $UNKNOWN_ACCEPTABLE_CODE; expected a client error"

echo "  ==> saturating all ${REQUESTED_CAPACITY} observed ${REQUESTED} devices..."
FILLERS=()
for ((i = 1; i <= REQUESTED_CAPACITY; i++)); do
  FILLERS+=("$(submit_job "$PE_ID" "${AGENTS[$i]}" "guaranteed" "0.05" "$REQUESTED" 1)")
done
requested_saturated() {
  local filler status
  for filler in "${FILLERS[@]}"; do
    status=$(get_field "$filler" status)
    [[ "$status" == "RUNNING" || "$status" == "COMPLETED" ]] || return 1
  done
}
wait_until "all ${REQUESTED} capacity is occupied" "$ADMISSION_BUDGET_SECONDS" 1 \
  requested_saturated \
  || { fail "${REQUESTED} saturation precondition failed"; close_platform_experiment "$PE_ID"; finish; }

echo "  ==> submitting a job requesting ${REQUESTED} with acceptable_accelerator_types=[${REQUESTED}, ${ALTERNATIVE}]..."
read -r CODE FLEX_ID <<< "$(submit_job_expect_code "$PE_ID" "${AGENTS[0]}" "guaranteed" "0.02" \
  "{\"accelerator_count\":1,\"accelerator_type\":\"${REQUESTED}\",\"acceptable_accelerator_types\":[\"${ALTERNATIVE}\"],\"accelerator_tolerations\":[\"nvidia.com/gpu\"]}")"

if [[ "$CODE" -ge 400 ]]; then
  fail "submission with an acceptable alternative was rejected outright (HTTP $CODE) instead of admitted onto ${ALTERNATIVE}"
else
  S=$(wait_for_status "$FLEX_ID" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    ADMITTED_TYPE=$(get_field "$FLEX_ID" accelerator_type)
    [[ "$ADMITTED_TYPE" == "$ALTERNATIVE" ]] \
      && pass "job admitted onto free alternative ${ALTERNATIVE} while requested ${REQUESTED} was saturated (status=$S)" \
      || fail "job reached status=$S but admitted accelerator_type=$ADMITTED_TYPE (expected ${ALTERNATIVE} — requested ${REQUESTED} was saturated)"
  else
    fail "job stayed QUEUED (status=$S) with idle ${ALTERNATIVE} capacity available — admission only considered the requested flavor, not acceptable_accelerator_types"
  fi
  [[ "$(wait_for_status "$FLEX_ID" "COMPLETED,FAILED,EVICTED,QUEUED" "$(completion_wait_tries 0.02)" || true)" == "COMPLETED" ]] \
    && file_finding "$FLEX_ID" \
    || fail "flexible accelerator job did not complete cleanly"
fi

for filler in "${FILLERS[@]}"; do cancel_job "$filler"; done
close_platform_experiment "$PE_ID"
finish
