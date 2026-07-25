#!/usr/bin/env bash
# True concurrency (not "back-to-back sequential") race: N guaranteed jobs are fired at the
# scheduler at the same instant, all racing for one accelerator type's capacity, where the sum of
# their requests exceeds what the node actually has. Unlike capacity-safety.sh (which submits
# sequentially and checks quota debit isn't doubled), this scenario exercises the actual
# concurrent-write path — multiple submitJob calls landing inside the same or adjacent
# scheduler ticks — which is exactly where a reservation-write race would show up as
# over-admission. Cluster-exclusive because it deliberately saturates a shared accelerator pool.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-L40"
NODE_ACCELERATOR_CAPACITY=$(kubectl get nodes -l 'nvidia.com/gpu.product=NVIDIA-L40' -o json \
  | py "import sys,json; print(sum(int(n.get('status',{}).get('allocatable',{}).get('nvidia.com/gpu',0)) for n in json.load(sys.stdin)['items']))")
[[ "$NODE_ACCELERATOR_CAPACITY" -ge 2 && $((NODE_ACCELERATOR_CAPACITY % 2)) -eq 0 ]] \
  || { echo "ERROR: concurrent admission fixture needs a positive even L40 capacity, observed $NODE_ACCELERATOR_CAPACITY" >&2; exit 2; }
ACCELERATOR_COUNT_PER_JOB=$((NODE_ACCELERATOR_CAPACITY / 2))
N_JOBS=3   # Three half-capacity requests: exactly two fit and one must lose the race.
EXPECT_ADMITTED=2

AGENTS=()
for i in $(seq 1 "$N_JOBS"); do
  a="agent-race-${i}-${RUN_ID}"
  AGENTS+=("$a")
  register_agent "$a"
done
PE_ID=$(create_platform_experiment "concurrent-race-${RUN_ID}" 50.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "  ==> firing ${N_JOBS} guaranteed jobs (${ACCELERATOR_COUNT_PER_JOB}x${ACCELERATOR_TYPE} each = $((N_JOBS * ACCELERATOR_COUNT_PER_JOB)) requested, ${NODE_ACCELERATOR_CAPACITY} available) at the same instant..."
OUT_DIR="$TMPDIR_T/race"
mkdir -p "$OUT_DIR"
PIDS=()
for i in $(seq 1 "$N_JOBS"); do
  (
    submit_job "$PE_ID" "${AGENTS[$((i - 1))]}" "guaranteed" "0.05" "$ACCELERATOR_TYPE" "$ACCELERATOR_COUNT_PER_JOB" \
      > "$OUT_DIR/job_$i.id" 2> "$OUT_DIR/job_$i.err"
  ) &
  PIDS+=("$!")
done
FAILED_SUBMITS=0
for pid in "${PIDS[@]}"; do
  wait "$pid" || FAILED_SUBMITS=$((FAILED_SUBMITS + 1))
done
[[ "$FAILED_SUBMITS" -eq 0 ]] \
  && pass "all $N_JOBS concurrent submissions were accepted at the HTTP layer" \
  || fail "$FAILED_SUBMITS of $N_JOBS concurrent submissions failed outright — see $OUT_DIR/*.err"

JOBS=()
for i in $(seq 1 "$N_JOBS"); do
  jid="$(cat "$OUT_DIR/job_$i.id" 2>/dev/null || true)"
  [[ -n "$jid" ]] && JOBS+=("$jid")
done

# Wait only for the capacity-sized winning set. The over-capacity loser is expected to remain
# QUEUED, so requiring every job to leave QUEUED is contradictory and burns the full timeout.
capacity_admitted() {
	local n=0 j s
  for j in "${JOBS[@]}"; do
    s=$(get_status "$j")
    [[ "$s" == "SUBMITTED" || "$s" == "ADMITTED" || "$s" == "RUNNING" ]] && n=$((n + 1))
  done
	[[ "$n" -ge "$EXPECT_ADMITTED" ]]
}
wait_until "capacity-sized winning set is admitted" "$ADMISSION_BUDGET_SECONDS" 1 capacity_admitted \
  || fail "capacity-sized winning set was not admitted"

ADMITTED_COUNT=0
QUEUED_COUNT=0
declare -a ADMITTED_JOBS=()
for j in "${JOBS[@]}"; do
  s=$(get_status "$j")
  echo "  $j -> $s"
  if [[ "$s" == "SUBMITTED" || "$s" == "ADMITTED" || "$s" == "RUNNING" ]]; then
    ADMITTED_COUNT=$((ADMITTED_COUNT + 1))
    ADMITTED_JOBS+=("$j")
  elif [[ "$s" == "QUEUED" ]]; then
    QUEUED_COUNT=$((QUEUED_COUNT + 1))
  fi
done

# The core race-safety invariant: however the scheduler interleaves concurrent submitJob
# calls, it must never admit more total accelerator count than the node physically has. This is the
# assertion that would catch the reservation-write-race class of bug (fixed in loop_preempt.go
# per findings.md, but only ever exercised there by sequential back-to-back submission).
TOTAL_ADMITTED_ACCELERATORS=$((ADMITTED_COUNT * ACCELERATOR_COUNT_PER_JOB))
[[ "$TOTAL_ADMITTED_ACCELERATORS" -le "$NODE_ACCELERATOR_CAPACITY" ]] \
  && pass "admitted total (${ADMITTED_COUNT} jobs = ${TOTAL_ADMITTED_ACCELERATORS} accelerators) does not exceed physical capacity (${NODE_ACCELERATOR_CAPACITY} accelerators) under true concurrency" \
  || fail "OVER-ADMISSION under concurrent submission: ${ADMITTED_COUNT} jobs (${TOTAL_ADMITTED_ACCELERATORS} accelerators) admitted against only ${NODE_ACCELERATOR_CAPACITY} accelerators available — reservation race"

[[ "$ADMITTED_COUNT" -eq "$EXPECT_ADMITTED" ]] \
  && pass "exactly $EXPECT_ADMITTED of $N_JOBS raced jobs admitted, as capacity allows" \
  || fail "expected exactly $EXPECT_ADMITTED admitted, got $ADMITTED_COUNT"

[[ "$QUEUED_COUNT" -ge 1 ]] \
  && pass "at least one over-subscribed job correctly lost the race and stayed QUEUED/non-admitted" \
  || fail "no job was left QUEUED even though requests (${N_JOBS}x${ACCELERATOR_COUNT_PER_JOB}=$((N_JOBS * ACCELERATOR_COUNT_PER_JOB))) exceed capacity (${NODE_ACCELERATOR_CAPACITY}) — every job appears admitted, which is impossible if capacity accounting is correct"

for j in "${ADMITTED_JOBS[@]:-}"; do
  [[ -z "$j" ]] && continue
  cancel_job "$j"
done
for j in "${ADMITTED_JOBS[@]:-}"; do
  [[ -z "$j" ]] && continue
  s=$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,REJECTED" 30 || true)
  [[ "$s" == "COMPLETED" || "$s" == "FAILED" || "$s" == "EVICTED" || "$s" == "REJECTED" ]] \
    && pass "$j stopped after race assertions (status=$s)" \
    || fail "$j did not stop after cancellation (status=$s)"
  wait_until "$j Kubernetes Job is removed after cancellation" 30 1 job_resource_absent "$j" \
    && pass "$j no longer has a Kubernetes Job" \
    || fail "$j remained present in Kubernetes after cancellation (status=$s)"
done

close_platform_experiment "$PE_ID"

echo "  ==> racing two same-agent submissions against one PostgreSQL quota boundary..."
QUOTA_AGENT="agent-quota-race-${RUN_ID}"
register_agent "$QUOTA_AGENT"
QUOTA_PE=$(create_platform_experiment "concurrent-quota-race-${RUN_ID}" 1.0 1)
signup_and_start "$QUOTA_PE" "$QUOTA_AGENT"
# Phase-1 guaranteed allocation is 0.4 AccH. Each job reserves 0.24 AccH, so either job fits
# alone but both cannot. The correct outcome is exactly one committed desired row and one 4xx;
# zero winners exposes the old provisional-row visibility race, while two exposes over-admission.
QUOTA_OUT="$TMPDIR_T/quota-race"
mkdir -p "$QUOTA_OUT"
for i in 1 2; do
  (
    submit_job_expect_code "$QUOTA_PE" "$QUOTA_AGENT" "guaranteed" "0.06" \
      '{"accelerator_count":4,"accelerator_type":"nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}' > "$QUOTA_OUT/result_$i"
  ) &
  PIDS[$i]=$!
done
for i in 1 2; do wait "${PIDS[$i]}"; done
QUOTA_ACCEPTED=0
QUOTA_REJECTED=0
QUOTA_JOB=""
for i in 1 2; do
  read -r code id < "$QUOTA_OUT/result_$i"
  if [[ "$code" -ge 200 && "$code" -lt 300 ]]; then
    QUOTA_ACCEPTED=$((QUOTA_ACCEPTED + 1))
    QUOTA_JOB="$id"
  elif [[ "$code" -ge 400 && "$code" -lt 500 ]]; then
    QUOTA_REJECTED=$((QUOTA_REJECTED + 1))
  else
    fail "same-agent quota race request $i returned unexpected HTTP $code"
  fi
done
[[ "$QUOTA_ACCEPTED" -eq 1 && "$QUOTA_REJECTED" -eq 1 ]] \
  && pass "same-agent quota race committed exactly one desired row and rejected exactly one request" \
  || fail "same-agent quota race produced accepted=$QUOTA_ACCEPTED rejected=$QUOTA_REJECTED; expected 1/1"
QUOTA_USED=$(quota_used_guaranteed "$QUOTA_PE" "$QUOTA_AGENT")
[[ "$(py "print(round(float('$QUOTA_USED'), 6))")" == "0.24" ]] \
  && pass "the sole committed PostgreSQL row contributes exactly 0.24 AccH desired usage" \
  || fail "same-agent quota race reports $QUOTA_USED AccH desired usage; expected 0.24"
[[ -n "$QUOTA_JOB" ]] && cancel_job "$QUOTA_JOB"
close_platform_experiment "$QUOTA_PE"
finish
