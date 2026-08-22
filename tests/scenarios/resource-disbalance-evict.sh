#!/usr/bin/env bash
# The resource-disbalance evictor: a job whose CPU request is wildly out of proportion to the
# accelerators it holds strands that node's other accelerators — they are free, nothing can reach
# them, and no amount of extra hardware fixes it because the blockage is a ratio, not a shortage.
# Preemption cannot answer it (it ranks victims by tier and progress, and the offender is often
# guaranteed-tier and the only thing running), so this is the one pass that terminates a running
# job the queue never asked to stop.
#
# It is also the only such pass, which is why this scenario checks not just that the eviction
# happens but that it explains itself: an agent whose job was killed by a decision it never asked
# for has to be able to read why, and fix its next submission.
#
# Cluster-exclusive: it deliberately consumes almost all of one node's CPU, which would starve any
# scenario running beside it. Needs a node carrying at least 4 accelerators — see MIN_ACCELERATORS
# below for why fewer makes the shape unreachable rather than merely awkward.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

# The accelerator's extended-resource name shares the vendor domain with its type label
# (nvidia.com/gpu.product=... -> nvidia.com/...), which is how a node's accelerator count is found
# without hardcoding a vendor.
VENDOR="${TEST_ACCELERATOR_TYPE%%/*}"

# Pick the node carrying the flavor the jobs will actually request — not merely the vendor. The
# hog's CPU is sized against this node, so choosing a node the jobs cannot land on measures one
# node's shape while the jobs run on another.
read -r NODE NODE_ACCELERATORS NODE_CPU FLAVOR_NODES <<< "$(kubectl get nodes -o json | py "
import json, sys
key, _, value = '$TEST_ACCELERATOR_TYPE'.partition('=')
best = ('', 0, 0.0)
carriers = 0
for node in json.load(sys.stdin)['items']:
    if node['metadata'].get('labels', {}).get(key) != value:
        continue
    carriers += 1
    alloc = node['status'].get('allocatable', {})
    count = 0
    for k, v in alloc.items():
        if k.startswith('$VENDOR/'):
            try:
                count += int(v)
            except ValueError:
                pass
    if count <= best[1]:
        continue
    cpu = alloc.get('cpu', '0')
    cores = float(cpu[:-1]) / 1000.0 if cpu.endswith('m') else float(cpu)
    best = (node['metadata']['name'], count, cores)
print(best[0] or '-', best[1], best[2], carriers)
")"
echo "  node=${NODE} accelerators=${NODE_ACCELERATORS} allocatable_cpu=${NODE_CPU} nodes_with_this_flavor=${FLAVOR_NODES}"

# The blocked job must have nowhere else to go, or it simply runs on another node and the evictor
# is right not to fire. That is correct platform behaviour, not a failure — but it means this
# cluster cannot express the shape under test.
if [[ "${FLAVOR_NODES:-0}" -ne 1 ]]; then
  echo "  [SKIP] ${FLAVOR_NODES} nodes carry ${TEST_ACCELERATOR_TYPE}; the blocked job would just run on another one"
  echo "  [SKIP] this scenario asserted nothing — it is not evidence the evictor works"
  finish
fi

# The arithmetic that decides whether this cluster can express the shape at all. A job holding one
# of N accelerators is entitled to node_cpu/N cores; the pass fires above 3x that, i.e. above
# 3*node_cpu/N cores. Since no job can request more CPU than the node has, that threshold is only
# reachable when 3/N < 1 — a node needs at least 4 accelerators. On a 2-accelerator node the
# threshold is 1.5x the whole node's CPU, which nothing can request, so no disbalance exists to
# detect and there is nothing here to test.
MIN_ACCELERATORS=4
if [[ "$NODE_ACCELERATORS" -lt "$MIN_ACCELERATORS" ]]; then
  echo "  [SKIP] the largest node carries ${NODE_ACCELERATORS} ${VENDOR} accelerator(s); disbalance needs >=${MIN_ACCELERATORS}"
  echo "  [SKIP] (3x the proportionate share exceeds the node's whole CPU below that, so no request can trigger it)"
  echo "  [SKIP] this scenario asserted nothing — it is not evidence the evictor works"
  finish
fi

AGENT="agent-disbalance-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "disbalance-${RUN_ID}" 20.0 1 5)
signup_and_start "$PE_ID" "$AGENT"

# The hog takes 85% of the node's CPU for a single accelerator. Against a share of node_cpu/N that
# is 0.85*N times its entitlement — comfortably past 3x for N>=4 — and it leaves only ~15% of the
# node's CPU, too little for the blocked job below, while N-1 accelerators sit idle.
HOG_CPU=$(py "print(max(1, int($NODE_CPU * 0.85)))")
# Big enough that the ~15% the hog leaves cannot satisfy it, so the ONLY way it runs is if the hog
# is evicted — which is exactly the claim under test. Small enough to fit comfortably once it is.
BLOCKED_CPU=$(py "print(max(1, int($NODE_CPU * 0.25)))")
FAIR_SHARE=$(py "print(round($NODE_CPU / $NODE_ACCELERATORS, 3))")
RATIO=$(py "print(round($HOG_CPU / max(1e-9, $FAIR_SHARE), 2))")
echo "  hog=${HOG_CPU} cores for 1 accelerator (share=${FAIR_SHARE}, ratio=${RATIO}x), blocked=${BLOCKED_CPU} cores"

OVER_TOLERANCE=$(py "print(float('$RATIO') > 3.0)")
[[ "$OVER_TOLERANCE" == "True" ]] \
  && pass "hog's request is ${RATIO}x its proportionate share, past the 3x tolerance" \
  || { echo "  [SKIP] node shape cannot produce a >3x disproportion (cpu=${NODE_CPU}, accelerators=${NODE_ACCELERATORS}) — asserted nothing"; close_platform_experiment "$PE_ID"; finish; }
BLOCKED_NOW=$(py "print(float('$BLOCKED_CPU') > float('$NODE_CPU') - float('$HOG_CPU'))")
[[ "$BLOCKED_NOW" == "True" ]] \
  && pass "blocked job cannot fit alongside the hog, so only an eviction can admit it" \
  || fail "blocked job would fit anyway (${BLOCKED_CPU} cores vs $(py "print(round($NODE_CPU - $HOG_CPU, 2))") free) — this scenario would prove nothing"

JOB_HOURS="$(scale_budget 0.05)"
HOG=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "" "" "" "" \
  "{\"cpu\": \"${HOG_CPU}\", \"accelerator_count\": 1, \"acceptable_accelerator_types\": []}")
echo "  ==> hog $HOG submitted"
S=$(wait_for_status "$HOG" "RUNNING" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" ]] \
  && pass "hog is RUNNING and holding the node's CPU" \
  || { fail "hog never reached RUNNING (status=$S) — nothing to strand accelerators"; close_platform_experiment "$PE_ID"; finish; }

# A modest job that fits the idle accelerators perfectly and is blocked only by the hog's CPU.
# Alternatives are cleared for the same reason the single-carrier check above exists: given one it
# would land elsewhere, correctly, and prove nothing about disbalance.
BLOCKED=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "" "" "" "" \
  "{\"cpu\": \"${BLOCKED_CPU}\", \"accelerator_count\": 1, \"acceptable_accelerator_types\": []}")
echo "  ==> blocked job $BLOCKED submitted (${BLOCKED_CPU} cores, 1 accelerator — fits an idle one only once the hog is gone)"

echo "  -- the disproportionate job is evicted so the idle accelerators become reachable --"
HOG_FINAL=$(wait_for_status "$HOG" "EVICTED,COMPLETED,FAILED" 90 || true)
if [[ "$HOG_FINAL" == "EVICTED" ]]; then
  pass "hog was evicted while the blocked job waited on accelerators it could not reach"
  REASON=$(get_field "$HOG" eviction_reason)
  [[ "$REASON" == resource_disbalance* ]] \
    && pass "eviction reason is resource_disbalance" \
    || fail "hog evicted for '$REASON', expected resource_disbalance"

  # An eviction nobody asked for has to explain itself. The reason carries its evidence in the
  # same "code: detail" shape the scheduler uses for not_admitted_reason.
  [[ "$REASON" == *":"* ]] \
    && pass "eviction reason carries an explanation, not just a code" \
    || fail "eviction reason is a bare code ('$REASON') — the agent has nothing to act on"
  for phrase in "accelerator" "share" "stranding"; do
    [[ "$REASON" == *"$phrase"* ]] \
      && pass "explanation states the '$phrase' part of the decision" \
      || fail "explanation is missing '$phrase': $REASON"
  done
  echo "  full reason: $REASON"

  echo "  -- and the previously blocked job can now run --"
  BS=$(wait_for_status "$BLOCKED" "RUNNING,COMPLETED" "$ADMISSION_BUDGET_SECONDS" || true)
  [[ "$BS" == "RUNNING" || "$BS" == "COMPLETED" ]] \
    && pass "blocked job admitted once the disproportionate one was gone (status=$BS)" \
    || fail "blocked job is $BS — the eviction freed nothing, so it destroyed work for no gain"
elif [[ "$HOG_FINAL" == "COMPLETED" ]]; then
  fail "hog ran to completion while a fitting job waited on idle accelerators it was stranding"
else
  fail "hog ended as '$HOG_FINAL', expected EVICTED for resource_disbalance"
fi

cancel_job "$BLOCKED" || true
wait_for_status "$BLOCKED" "COMPLETED,FAILED,EVICTED,REJECTED" 30 > /dev/null || true
close_platform_experiment "$PE_ID"
finish
