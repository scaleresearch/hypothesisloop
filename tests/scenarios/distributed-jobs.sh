#!/usr/bin/env bash
# Distributed (num_nodes>1) job admission, in two parts sharing one platform experiment:
#   1. depth: a single num_nodes=2 job's gang scheduling creates all replicas, generated pod
#      specs carry equal/non-empty requests==limits, and billing reflects
#      TotalAccelerators = accelerator_count * num_nodes, not just accelerator_count.
#   2. pressure: several num_nodes=2 jobs submitted concurrently must each still get their
#      full per-node x NumNodes footprint under real scheduling pressure, not a partial
#      replica count.
# API + pod-spec checks; parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"
source "$DIR/../lib/cluster.sh"

JOB_HOURS="0.025"
# The estimate above is what the cost assertions below are about; the job's real runtime is set
# separately and kept short. Running the full 90s the estimate implies put this scenario at the
# suite's per-scenario ceiling, so any contention killed it mid-assertion — and a longer run
# proves nothing extra about distributed placement, replica-count billing or Service repair.
RUN_SECONDS=30
RUN_ENV="{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${RUN_SECONDS}\"}"
# H100, not T4: T4 is every other parallel-set scenario's default accelerator type (job-lifecycle,
# mixed-admission, stages-and-settlement, cpu-quota-guard's own dimension aside), so
# num_nodes=2 T4 jobs here would have to out-rank a constant stream of fresh guaranteed-tier
# submissions from those sibling scenarios for the scheduler's fairness-window priority —
# real capacity was free but priority never favored these jobs within any reasonable wait
# window. H100 is unused by every other scenario (capacity-safety uses L40, preemption-requeue
# uses A100 — same reasoning, see those files), so this scenario gets a clean, uncontended pool.
ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
H100_ACCH_RATE=1.0
AGENTS=("agent-dist-depth-${RUN_ID}" "agent-dist-pressure-a-${RUN_ID}" "agent-dist-pressure-b-${RUN_ID}"
        "agent-gang-form-${RUN_ID}" "agent-gang-pernode-${RUN_ID}" "agent-gang-toobig-${RUN_ID}"
        "agent-gang-fail-${RUN_ID}" "agent-gang-retry-${RUN_ID}"
        "agent-groups-form-${RUN_ID}" "agent-groups-fail-${RUN_ID}")
for a in "${AGENTS[@]}"; do register_agent "$a"; done
# Budget sized well above what Part1+Part2 will actually debit (H100's acch_rate=1.0 is the
# AccH baseline itself, so num_nodes=2 jobs cost exactly 2x their wall-clock hours) — also
# comfortably clears the first stage boundary at 40% of budget (see preemption-requeue.sh's
# identical reasoning for why that matters).
# Part 3 declares the metrics its workload reports. A metric nobody declares is still recorded
# and readable -- which is all Part 3 asserts on -- but val_accuracy stays the ranking metric so
# the stage ladder behaves exactly as it does for every other scenario.
DIST_METRICS='[{"key": "val_accuracy", "direction": "maximize"},
               {"key": "world_reduced_sum", "direction": "maximize"},
               {"key": "accelerators_per_node", "direction": "maximize"},
               {"key": "gang_attempt", "direction": "maximize"}]'
# Raised from 40.0 for Part 4, which adds two 7-accelerator grouped jobs (1 trainer x 4 + 3
# workers x 1). Each reserves 7 x 0.02h x 1.0 = 0.14 AccH and settles well under that, so the two
# together add ~0.28 AccH reserved — 2.0 of headroom rather than 0.3 so the ratio every earlier
# part is sized against (and the stage boundary at 40% of budget they all rely on) is unchanged
# rather than quietly tightened.
PE_ID=$(create_platform_experiment "distributed-jobs-${RUN_ID}" 42.0 "${#AGENTS[@]}" 10 0 "$DIST_METRICS")
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo "=========================================================="
echo "Part 1: single distributed job — gang scheduling + billing depth"
echo "=========================================================="
QUOTA_BEFORE=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
JOB=$(submit_job_ext "$PE_ID" "${AGENTS[0]}" "guaranteed" "$JOB_HOURS" "$JOB_FILE" "$RUN_ENV" "$ACCELERATOR_TYPE" "1" "2")
echo "  submitted distributed job (accelerator_count=1, num_nodes=2): $JOB"

S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" && "$S" != "COMPLETED" ]]; then
  fail "distributed job never reached RUNNING/COMPLETED (status=$S) — gang scheduling may be broken"
else
  pass "distributed job reached $S"

  REPLICAS=$(job_pod_count "$JOB")
  [[ "$REPLICAS" -ge 2 ]] && pass "$REPLICAS pods (expected 2, one per num_nodes replica)" \
    || fail "only $REPLICAS pod(s) — expected 2, gang scheduling did not create both replicas"
  DISTINCT_NODES=$(job_distinct_node_count "$JOB")
  [[ "$DISTINCT_NODES" -eq 2 ]] \
    && pass "default hard host spreading placed both ranks on distinct nodes" \
    || fail "distributed ranks occupy $DISTINCT_NODES distinct node(s), expected 2"

  POD=$(job_pod "$JOB")
  if [[ -n "$POD" ]]; then
    MEM_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || true)
    MEM_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.memory}' 2>/dev/null || true)
    ACCELERATOR_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu}' 2>/dev/null || true)
    ACCELERATOR_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}' 2>/dev/null || true)
    if [[ -n "$MEM_REQ" && "$MEM_REQ" == "$MEM_LIM" && -n "$ACCELERATOR_REQ" && "$ACCELERATOR_REQ" == "$ACCELERATOR_LIM" ]]; then
      pass "generated pod has equal, non-empty memory/accelerator requests==limits"
    else
      fail "expected equal, non-empty memory/accelerator requests+limits, got memory req=$MEM_REQ lim=$MEM_LIM accelerator req=$ACCELERATOR_REQ lim=$ACCELERATOR_LIM"
    fi
  else
    fail "could not locate pod to inspect resources"
  fi

  # The Service is part of the same logical desired workload, not an untracked side object.
  # Corrupt only its desired identity and require the stateless reconciler to replace the
  # complete workload from PostgreSQL without any repair queue or stored tick history.
  if [[ "$(get_status "$JOB")" == "RUNNING" ]]; then
    OLD_UID=$(job_uid "$JOB")
    corrupt_service_desired_hash "$JOB"
    # Repairing this means deleting the Job, waiting out its pods' termination, and recreating —
    # a delete/recreate cycle, not a single reconcile pass. On a contended cluster pod termination
    # alone can outlast the 30s this used to allow, which read as "the reconciler does not repair
    # drift" when it was still in the middle of doing exactly that.
    wait_until "cluster-agent repairs drifted distributed-job Service" 120 1 job_recreated_with_new_uid "$JOB" "$OLD_UID" \
      && pass "drifted companion Service caused deterministic full-workload reconciliation" \
      || fail "cluster-agent did not repair a drifted distributed-job Service"
  fi

  EST_COST=$(get_field "$JOB" estimated_cost_acch)
  EXPECTED_COST=$(py "print(round(2 * ${JOB_HOURS} * ${H100_ACCH_RATE}, 6))")
  COST_OK=$(py "print(abs(float('$EST_COST' or 0) - float('$EXPECTED_COST')) < 0.01)")
  [[ "$COST_OK" == "True" ]] && pass "estimated_cost_acch=$EST_COST bills for both replicas (TotalAccelerators)" \
    || fail "estimated_cost_acch=$EST_COST does not match TotalAccelerators-based expected~$EXPECTED_COST"

  # Budget derived from the job's real runtime (RUN_SECONDS), not from the estimate, and timed
  # fresh from here (already confirmed RUNNING/COMPLETED above) rather than from submission.
  S=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" "$(( RUN_SECONDS + 40 ))" || true)
  [[ "$S" == "COMPLETED" ]] && pass "completed cleanly" || fail "did not complete cleanly (status=$S)"

  QUOTA_AFTER=$(quota_used_guaranteed "$PE_ID" "${AGENTS[0]}")
  DEBIT=$(py "print(round(float('$QUOTA_AFTER' or 0) - float('$QUOTA_BEFORE' or 0), 4))")
  echo "  guaranteed quota debited: ${DEBIT} AccH (single-replica equivalent would be ~$(py "print(round(${JOB_HOURS} * ${H100_ACCH_RATE}, 4))") AccH)"
fi

echo ""
echo "=========================================================="
echo "Part 2: concurrent distributed jobs under scheduling pressure"
echo "=========================================================="
declare -a JOBS
for a in "${AGENTS[@]:1}"; do
  J=$(submit_job "$PE_ID" "$a" "burst" "0.02" "$ACCELERATOR_TYPE" "1" "2")
  JOBS+=("$J")
  echo "  concurrent distributed job: $J (agent=$a, accelerator_count=1, num_nodes=2)"
done

OK=0
for J in "${JOBS[@]}"; do
  # QUEUED deliberately excluded from the wait target: every fresh submission starts QUEUED,
  # so including it here would make wait_for_status return almost instantly (on its very
  # first poll) instead of actually waiting up to the timeout for a real admission decision —
  # the timeout value below would then be dead code. A timeout prints the current status for
  # the assertion below.
  # burst-tier jobs are also processed after guaranteed-tier jobs every tick, and the
  # scheduler is one shared service across the whole concurrent test-suite run — several
  # other scenarios in this scenario's batch submit guaranteed-tier jobs, which can keep
  # pushing burst-tier admission back for longer than a short window comfortably covers.
  S=$(wait_for_status "$J" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
  REPLICAS=$(job_pod_count "$J")
  echo "  $J: status=$S replicas=$REPLICAS"
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    if [[ "$REPLICAS" -ge 2 ]]; then
      DISTINCT_NODES=$(job_distinct_node_count "$J")
      if [[ "$DISTINCT_NODES" -ge 2 ]]; then
        OK=$((OK + 1))
      else
        fail "$J has full replica count but only $DISTINCT_NODES distinct node(s)"
      fi
    else
      fail "$J admitted (status=$S) but only has $REPLICAS pod(s), expected 2"
    fi
  fi
done
[[ "$OK" -ge 1 ]] \
  && pass "$OK/${#JOBS[@]} concurrent num_nodes=2 jobs admitted with full replica counts under pressure" \
  || fail "no concurrent num_nodes=2 job reached RUNNING/COMPLETED with full replica count"

for J in "${JOBS[@]}"; do wait_for_status "$J" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true; done

echo ""
echo "=========================================================="
echo "Part 3: the gang guarantees, asserted on a number the platform cannot fake"
echo "=========================================================="
# Every assertion below rests on train_distributed.py's all_reduce: for num_nodes N the only
# correct reduced value is N(N-1)/2. A rank that never joined makes it smaller and a rank that
# joined twice makes it larger, so "the gang formed" is a statement about arithmetic rather than
# about a pod spec that merely looks right.
GANG_RUN_SECONDS=20
# A predicate, not an inline $(...) : wait_until re-invokes its command every iteration, so a
# command substitution written at the call site would be evaluated once and then polled forever
# against its first result.
gang_pods_gone() { [[ "$(job_pod_count "$1")" -eq 0 ]]; }
gang_cmd() { echo '{"command": ["python", "train_distributed.py"], "max_retries": '"$1"'}'; }
# Only two hosts advertise H100 (see the node inventory), and spread_across_hosts defaults to true
# for num_nodes>1 -- so a 3-rank gang has to be allowed to share a host. That is not a weakening of
# what is under test: the hard-spread default is already asserted in Part 1, and what Part 3 needs
# is three ranks that must find each other, not three ranks on three hosts.
gang_cmd_colocated() { echo '{"command": ["python", "train_distributed.py"], "max_retries": '"$1"', "topology": {"spread_across_hosts": false}}'; }

# --- 1. G2 + G8: every rank resolves the same MASTER_ADDR, and COMPLETED means all of them exited 0
JOB_FORM=$(submit_job_ext "$PE_ID" "${AGENTS[3]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GANG_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "1" "3" \
  "" "" "" "" "$(gang_cmd_colocated 0)")
echo "  submitted 3-rank gang: $JOB_FORM"
S=$(wait_for_completion_after_running "$JOB_FORM" "0.02" "$ADMISSION_BUDGET_SECONDS" "$(( GANG_RUN_SECONDS + 90 ))" || true)
if [[ "$S" == "COMPLETED" ]]; then
  pass "3-rank gang reached COMPLETED — every rank exited 0 (G8)"
  REDUCED=$(metric_max "$JOB_FORM" world_reduced_sum)
  if [[ -z "$REDUCED" ]]; then
    fail "the gang never reported world_reduced_sum — it completed without proving the process group formed"
  elif [[ "$(py "print(abs(float('$REDUCED') - 3.0) < 0.001)")" == "True" ]]; then
    pass "all_reduce over 3 ranks = $REDUCED (0+1+2) — every rank resolved the same MASTER_ADDR (G2)"
  else
    fail "all_reduce = $REDUCED, want 3 (0+1+2) — a rank did not join the process group"
  fi
  file_finding "$JOB_FORM" "distributed e2e: 3-rank gloo-style rendezvous formed, reduced value correct."
else
  fail "3-rank gang did not complete (status=$S, eviction_reason=$(get_field "$JOB_FORM" eviction_reason))"
fi

# --- 2. G3: per-node environment facts are true of the pod they are in
# accelerator_count 2 x num_nodes 2: the pod must be told 2, while the experiment carries 4. The
# old code injected the experiment total, so a workload sizing --nproc_per_node from it would
# launch 4 processes for the 2 devices it actually holds.
JOB_PERNODE=$(submit_job_ext "$PE_ID" "${AGENTS[4]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GANG_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "2" "2" \
  "" "" "" "" "$(gang_cmd 0)")
echo "  submitted accelerator_count=2 num_nodes=2 gang: $JOB_PERNODE"
S=$(wait_for_completion_after_running "$JOB_PERNODE" "0.02" "$ADMISSION_BUDGET_SECONDS" "$(( GANG_RUN_SECONDS + 90 ))" || true)
if [[ "$S" == "COMPLETED" ]]; then
  PER_NODE=$(metric_max "$JOB_PERNODE" accelerators_per_node)
  TOTAL=$(get_field "$JOB_PERNODE" accelerator_count)
  if [[ "$(py "print(abs(float('${PER_NODE:-0}') - 2.0) < 0.001)")" == "True" ]]; then
    pass "each pod was told it holds 2 accelerators, not the 4-device job total (G3)"
  else
    fail "HYPOTHESISLOOP_ACCELERATOR_COUNT reported as ${PER_NODE:-<never reported>}, want 2 — the pod is being told the job total"
  fi
  [[ "$TOTAL" == "4" ]] \
    && pass "the experiment still carries the 4-accelerator total that admission and billing read" \
    || fail "experiment accelerator_count=$TOTAL, want 4 (2 per node x 2 nodes)"
  file_finding "$JOB_PERNODE" "distributed e2e: per-node accelerator count is true of the pod."
else
  fail "accelerator_count=2 num_nodes=2 gang did not complete (status=$S)"
fi

# --- 3. G1: all N nodes are admitted together or none is
# More ranks than there are hosts advertising this type, with the default hard host spread. There
# is no partial admission to fall back to, so it must sit in the queue saying why.
JOB_TOOBIG=$(submit_job_ext "$PE_ID" "${AGENTS[5]}" "guaranteed" "0.02" "$JOB_FILE" "$RUN_ENV" "$ACCELERATOR_TYPE" "1" "9" \
  "" "" "" "" "$(gang_cmd 0)")
echo "  submitted 9-rank gang (more nodes than the cluster advertises): $JOB_TOOBIG"
assert_stable_status "$JOB_TOOBIG" "QUEUED,SUBMITTED" 20 "impossible-width gang is never partially admitted"
REASON=$(get_field "$JOB_TOOBIG" not_admitted_reason)
[[ "$REASON" == "capacity_unavailable" ]] \
  && pass "stayed QUEUED with not_admitted_reason=$REASON (G1)" \
  || fail "not_admitted_reason=$REASON, want capacity_unavailable"
PARTIAL=$(job_pod_count "$JOB_TOOBIG")
[[ "$PARTIAL" -eq 0 ]] \
  && pass "no partial pods exist for the unadmitted gang" \
  || fail "$PARTIAL pod(s) exist for a gang that was never admitted — admission is not atomic"
cancel_job "$JOB_TOOBIG"

# --- 4. G4: a rank failing stops the gang, promptly
# FAIL_RANK exits non-zero AFTER the barrier, so every rank is provably running when one dies --
# which is what makes "the survivors were torn down" a statement about the gang policy rather than
# about a rank that never started. The old per-index behaviour left ranks 0 and 1 blocked in the
# collective holding their accelerators; the wall-clock bound below is what distinguishes the two.
JOB_FAIL=$(submit_job_ext "$PE_ID" "${AGENTS[6]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"600\", \"FAIL_RANK\": \"2\"}" "$ACCELERATOR_TYPE" "1" "3" \
  "" "" "" "" "$(gang_cmd_colocated 0)")
echo "  submitted 3-rank gang with rank 2 failing after the barrier: $JOB_FAIL"
S=$(wait_for_status "$JOB_FAIL" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" == "RUNNING" || "$S" == "FAILED" ]]; then
  FAIL_START=$(date +%s)
  # The workload would otherwise hold every surviving rank for a full 600s. Anything close to that
  # means the survivors ran on after rank 2 died, which is precisely the failure being ruled out.
  S=$(wait_for_status "$JOB_FAIL" "FAILED,EVICTED,COMPLETED" 180 || true)
  ELAPSED=$(( $(date +%s) - FAIL_START ))
  [[ "$S" == "FAILED" ]] \
    && pass "one rank's failure failed the whole job (G4)" \
    || fail "gang reached $S after a rank failed, want FAILED"
  [[ "$ELAPSED" -lt 150 ]] \
    && pass "gang terminated ${ELAPSED}s after the failing rank died, far short of the 600s the survivors were told to run" \
    || fail "gang took ${ELAPSED}s to terminate — the survivors ran on rather than being torn down with it"
  wait_until "surviving ranks' pods terminate" 60 2 gang_pods_gone "$JOB_FAIL" \
    && pass "no surviving rank's pod is left holding an accelerator" \
    || fail "$(job_pod_count "$JOB_FAIL") pod(s) still present after the gang failed"
else
  fail "failing gang never reached RUNNING (status=$S) — nothing about G4 was exercised"
fi

# --- 5. G4 + G7: max_retries restarts the whole gang, and every attempt is billed
# Kubernetes cannot restart a gang, so this budget is spent by the control plane requeueing the
# experiment. Each attempt reports its own index under the same experiment id, so the number of
# distinct gang_attempt values IS the number of attempts that actually ran.
QUOTA_BEFORE_RETRY=$(quota_used_guaranteed "$PE_ID" "${AGENTS[7]}")
JOB_RETRY=$(submit_job_ext "$PE_ID" "${AGENTS[7]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GANG_RUN_SECONDS}\", \"FAIL_RANK\": \"1\"}" "$ACCELERATOR_TYPE" "1" "3" \
  "" "" "" "" "$(gang_cmd_colocated 1)")
echo "  submitted 3-rank gang with max_retries=1 and rank 1 failing: $JOB_RETRY"
# Two full attempts, each of which must be admitted, run to the barrier and die -- plus the
# requeue and re-admission between them, which is a scheduler tick rather than a pod restart.
S=$(wait_for_status "$JOB_RETRY" "FAILED,EVICTED,COMPLETED" "$(( ADMISSION_BUDGET_SECONDS * 2 + 240 ))" || true)
[[ "$S" == "FAILED" ]] \
  && pass "gang with an exhausted retry budget ended FAILED" \
  || fail "gang with max_retries=1 ended $S, want FAILED"
ATTEMPTS=$(metric_distinct_count "$JOB_RETRY" gang_attempt)
[[ "$ATTEMPTS" -eq 2 ]] \
  && pass "exactly 2 gang attempts ran (max_retries=1 means N+1 attempts)" \
  || fail "$ATTEMPTS gang attempt(s) reported, want exactly 2 — a gang retry either never happened or ran away"
# Every attempt reported the full reduced value, so both attempts were complete 3-rank gangs
# rather than a retry of the one rank that died.
REDUCED_RETRY=$(metric_max "$JOB_RETRY" world_reduced_sum)
[[ "$(py "print(abs(float('${REDUCED_RETRY:-0}') - 3.0) < 0.001)")" == "True" ]] \
  && pass "each attempt formed a complete 3-rank gang (reduced value $REDUCED_RETRY)" \
  || fail "reduced value across retries was ${REDUCED_RETRY:-<never reported>}, want 3 — a retry ran a partial gang"
QUOTA_AFTER_RETRY=$(quota_used_guaranteed "$PE_ID" "${AGENTS[7]}")
RETRY_DEBIT=$(py "print(round(float('${QUOTA_AFTER_RETRY:-0}') - float('${QUOTA_BEFORE_RETRY:-0}'), 4))")
# Both attempts held 3 accelerators; billing counts observed hours across the whole experiment, so
# a debit no larger than a single attempt's would mean the second one ran for free.
SINGLE_ATTEMPT=$(py "print(round(3 * ${GANG_RUN_SECONDS} / 3600.0 * ${H100_ACCH_RATE}, 6))")
[[ "$(py "print(float('$RETRY_DEBIT') > float('$SINGLE_ATTEMPT') * 1.2)")" == "True" ]] \
  && pass "settled cost ${RETRY_DEBIT} AccH covers 3 nodes x both attempts (one attempt alone is ~${SINGLE_ATTEMPT}) (G7)" \
  || fail "settled cost ${RETRY_DEBIT} AccH is within one attempt's worth (~${SINGLE_ATTEMPT}) — the second gang attempt was not billed"

echo ""
echo "=========================================================="
echo "Part 4: one gang, several shapes — a heterogeneous grouped job"
echo "=========================================================="
# Groups are a second way to express a gang, so they are asserted here rather than in a file of
# their own: everything Part 3 proves about a gang has to hold when the gang's nodes are not
# identical, and the way to show that is the same reduced value out of the same workload.
#
# One `trainer` replica holding 4 accelerators alongside three smaller single-accelerator
# `worker` replicas: 4 nodes, 7 accelerators. That shape is the whole argument for the feature —
# submitted as an ungrouped num_nodes=4 job it would have to take the trainer's shape on every
# node and pay for 16 accelerators to use 7, and split into two jobs it would be two experiments
# that never meet at a rendezvous.
#
# 30s rather than Part 3's 20: the settled-cost band below is measured against wall clock, and
# the two figures it has to separate (7 accelerators vs. 16) only stay apart once the run is
# long enough that a few seconds of measurement slack cannot close the gap. See the arithmetic there.
GROUP_RUN_SECONDS=30
TRAINER_ACCELERATORS=4
WORKER_ACCELERATORS=1
WORKER_REPLICAS=3
GROUP_TOTAL_ACCELERATORS=$(( TRAINER_ACCELERATORS + WORKER_ACCELERATORS * WORKER_REPLICAS ))  # 7
GROUP_NODES=$(( 1 + WORKER_REPLICAS ))                                                        # 4
# 4 nodes and only two hosts advertise H100, so the multi-node default of hard host spreading
# cannot be satisfied — the same reason gang_cmd_colocated exists in Part 3, and the same
# non-weakening: hard spreading is already asserted in Part 1, and what this part needs is four
# ranks of two different shapes that must find each other.
#
# The per-node fields job.yaml carries are deleted (null pops the key in mk_body.py) because
# domain.JobSpec.ValidateGroups rejects a spec that states a node's shape both at the top level
# and per group — one way to say a thing.
groups_cmd() {
  cat <<JSON
{"command": ["python", "train_distributed.py"],
 "max_retries": $1,
 "cpu": null, "memory": null, "storage": null, "accelerator_count": null,
 "topology": {"spread_across_hosts": false},
 "groups": [
   {"name": "trainer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi", "accelerator_count": ${TRAINER_ACCELERATORS}},
   {"name": "worker", "replicas": ${WORKER_REPLICAS}, "cpu": "100m", "memory": "64Mi", "storage": "256Mi", "accelerator_count": ${WORKER_ACCELERATORS}}
 ]}
JSON
}

# --- 1. one process group across both groups, plus cost summed over the shapes
QUOTA_BEFORE_GROUPS=$(quota_used_guaranteed "$PE_ID" "${AGENTS[8]}")
JOB_GROUPS=$(submit_job_ext "$PE_ID" "${AGENTS[8]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"${GROUP_RUN_SECONDS}\"}" "$ACCELERATOR_TYPE" "" "" \
  "" "" "" "" "$(groups_cmd 0)")
echo "  submitted 2-group job (trainer x1 @${TRAINER_ACCELERATORS}, worker x${WORKER_REPLICAS} @${WORKER_ACCELERATORS}): $JOB_GROUPS"

# Reserved cost is exact arithmetic on desired state, with no timing in it: 7 accelerators, not
# 4 nodes x the largest shape. Those two are 0.14 and 0.32 AccH apart, so this assertion alone
# distinguishes "sums the groups" from "replicates the biggest one".
GROUP_TOTAL=$(get_field "$JOB_GROUPS" accelerator_count)
[[ "$GROUP_TOTAL" == "$GROUP_TOTAL_ACCELERATORS" ]] \
  && pass "the experiment carries ${GROUP_TOTAL} accelerators — the SUM of the group shapes" \
  || fail "experiment accelerator_count=$GROUP_TOTAL, want ${GROUP_TOTAL_ACCELERATORS} (${TRAINER_ACCELERATORS} + ${WORKER_REPLICAS}x${WORKER_ACCELERATORS})"
EST_GROUPS=$(get_field "$JOB_GROUPS" estimated_cost_acch)
EXPECTED_GROUPS=$(py "print(round(${GROUP_TOTAL_ACCELERATORS} * 0.02 * ${H100_ACCH_RATE}, 6))")
WORST_GROUPS=$(py "print(round(${GROUP_NODES} * ${TRAINER_ACCELERATORS} * 0.02 * ${H100_ACCH_RATE}, 6))")
[[ "$(py "print(abs(float('${EST_GROUPS:-0}') - ${EXPECTED_GROUPS}) < 0.01)")" == "True" ]] \
  && pass "reserved ${EST_GROUPS} AccH = the summed shapes (~${EXPECTED_GROUPS}), not ${GROUP_NODES} nodes of the largest (~${WORST_GROUPS})" \
  || fail "estimated_cost_acch=${EST_GROUPS}, want ~${EXPECTED_GROUPS} — a grouped job billed as ${GROUP_NODES}x its largest shape would read ~${WORST_GROUPS}"

S=$(wait_for_status "$JOB_GROUPS" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" && "$S" != "COMPLETED" ]]; then
  fail "grouped job never reached RUNNING/COMPLETED (status=$S, not_admitted_reason=$(get_field "$JOB_GROUPS" not_admitted_reason))"
else
  # Timed from here, with the job already confirmed running, so what is measured is the interval
  # the platform bills for rather than the queueing before it.
  GROUP_START=$(date +%s)
  S=$(wait_for_status "$JOB_GROUPS" "COMPLETED,FAILED,EVICTED" "$(( GROUP_RUN_SECONDS + 90 ))" || true)
  GROUP_ELAPSED=$(( $(date +%s) - GROUP_START ))
  if [[ "$S" == "COMPLETED" ]]; then
    pass "the grouped gang reached COMPLETED — every replica of both groups exited 0"
    # 4 nodes across 2 groups: the only correct reduced value is 0+1+2+3 = 6. A worker computing
    # its rank group-locally would contribute 0,1,2 and reduce to 3; the trainer and the first
    # worker both claiming rank 0 is what the platform refuses to let happen by not setting RANK
    # on a grouped pod at all.
    REDUCED_GROUPS=$(metric_max "$JOB_GROUPS" world_reduced_sum)
    EXPECTED_REDUCED=$(py "print(${GROUP_NODES} * (${GROUP_NODES} - 1) // 2)")
    if [[ -z "$REDUCED_GROUPS" ]]; then
      fail "the grouped gang never reported world_reduced_sum — it completed without proving one process group formed"
    elif [[ "$(py "print(abs(float('$REDUCED_GROUPS') - ${EXPECTED_REDUCED}.0) < 0.001)")" == "True" ]]; then
      pass "all_reduce over trainer+workers = $REDUCED_GROUPS (0+1+2+3) — all four joined ONE process group with distinct global ranks"
    else
      fail "all_reduce = $REDUCED_GROUPS, want ${EXPECTED_REDUCED} — the groups did not form a single gang, or two ranks collided"
    fi
    file_finding "$JOB_GROUPS" "distributed e2e: a heterogeneous 2-group gang formed one process group."

    # Settled cost, on observed consumption. The band is deliberately wide, because the only
    # runtime figure this scenario can see is its own wall clock: at ${GROUP_TOTAL_ACCELERATORS}
    # accelerators the debit is ${GROUP_TOTAL_ACCELERATORS}xT/3600, and the band below spans 5 to
    # 11 accelerators over T +/- 10s. What it still separates is the failure it exists to catch:
    # ${GROUP_NODES} nodes of the trainer's shape would be ${GROUP_NODES}x${TRAINER_ACCELERATORS}
    # = 16xT/3600, above the upper bound for any T over ~22s — which is why this job runs
    # ${GROUP_RUN_SECONDS}s rather than Part 3's 20.
    QUOTA_AFTER_GROUPS=$(quota_used_guaranteed "$PE_ID" "${AGENTS[8]}")
    GROUP_DEBIT=$(py "print(round(float('${QUOTA_AFTER_GROUPS:-0}') - float('${QUOTA_BEFORE_GROUPS:-0}'), 6))")
    GROUP_LO=$(py "print(round(5 * max(1, ${GROUP_ELAPSED} - 10) / 3600.0 * ${H100_ACCH_RATE}, 6))")
    GROUP_HI=$(py "print(round(11 * (${GROUP_ELAPSED} + 10) / 3600.0 * ${H100_ACCH_RATE}, 6))")
    GROUP_WORST=$(py "print(round(${GROUP_NODES} * ${TRAINER_ACCELERATORS} * ${GROUP_ELAPSED} / 3600.0 * ${H100_ACCH_RATE}, 6))")
    [[ "$(py "print(${GROUP_LO} <= float('${GROUP_DEBIT}') <= ${GROUP_HI})")" == "True" ]] \
      && pass "settled ${GROUP_DEBIT} AccH over ${GROUP_ELAPSED}s is the summed shapes (band ${GROUP_LO}..${GROUP_HI}; ${GROUP_NODES}x the largest would be ~${GROUP_WORST})" \
      || fail "settled ${GROUP_DEBIT} AccH over ${GROUP_ELAPSED}s falls outside ${GROUP_LO}..${GROUP_HI} — ${GROUP_NODES} nodes of the trainer's shape would be ~${GROUP_WORST}"
  else
    fail "grouped job ended $S after ${GROUP_ELAPSED}s, want COMPLETED (eviction_reason=$(get_field "$JOB_GROUPS" eviction_reason))"
  fi
fi

# --- 2. a worker's failure takes the trainer with it
# A gang is one unit whatever its shapes: the trainer is told to run for 600s, so if it survives
# its worker the wall clock says so unmistakably. FAIL_RANK is the job-global rank 3, i.e. the
# last worker — which only resolves to that pod if the group offset arithmetic is right, so this
# also fails loudly if the trainer or the first worker were the one to die.
JOB_GROUPS_FAIL=$(submit_job_ext "$PE_ID" "${AGENTS[9]}" "guaranteed" "0.02" "$JOB_FILE" \
  "{\"HYPOTHESISLOOP_DURATION_SECONDS\": \"600\", \"FAIL_RANK\": \"3\"}" "$ACCELERATOR_TYPE" "" "" \
  "" "" "" "" "$(groups_cmd 0)")
echo "  submitted 2-group job whose last worker fails after the barrier: $JOB_GROUPS_FAIL"
S=$(wait_for_status "$JOB_GROUPS_FAIL" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" == "RUNNING" || "$S" == "FAILED" ]]; then
  GROUP_FAIL_START=$(date +%s)
  S=$(wait_for_status "$JOB_GROUPS_FAIL" "FAILED,EVICTED,COMPLETED" 180 || true)
  GROUP_FAIL_ELAPSED=$(( $(date +%s) - GROUP_FAIL_START ))
  [[ "$S" == "FAILED" ]] \
    && pass "a worker's failure failed the whole grouped job, trainer included" \
    || fail "grouped gang reached $S after a worker died, want FAILED"
  [[ "$GROUP_FAIL_ELAPSED" -lt 150 ]] \
    && pass "the grouped gang terminated ${GROUP_FAIL_ELAPSED}s after the worker died, far short of the 600s the trainer was told to run" \
    || fail "the grouped gang took ${GROUP_FAIL_ELAPSED}s to terminate — the trainer ran on after its worker died"
  wait_until "the trainer's pod terminates with its workers" 60 2 gang_pods_gone "$JOB_GROUPS_FAIL" \
    && pass "no replica of either group is left holding an accelerator" \
    || fail "$(job_pod_count "$JOB_GROUPS_FAIL") pod(s) still present after the grouped gang failed"
else
  fail "the failing grouped gang never reached RUNNING (status=$S) — nothing about cross-group teardown was exercised"
fi

close_platform_experiment "$PE_ID"
finish
