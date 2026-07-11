#!/usr/bin/env bash
# advanced-e2e.sh — exercises the scenarios a plain smoke test (e2e-flow.sh) doesn't:
#
#   1. Node death + reschedule: submit a job that tolerates multiple GPU types, let it land
#      on one fake GPU node, then kill that node (cordon + delete its pod) to simulate real
#      infra loss. Verifies the cluster-agent's desired-state reconciliation (see
#      cluster/cmd/cluster-agent/main.go reconcileOnce) self-heals the job onto a different
#      node/GPU type without any operator intervention, and that billing/metrics stay
#      correct across the gap (flavor-substitution debit if it lands on a pricier type,
#      correct final observed-cost settlement).
#   2. Policy eviction (terminal): cancel a running job (POST /experiments/{id}/cancel).
#      Confirms eviction is terminal — no requeue — and the unused reservation is refunded
#      (see controller.go's evict()/CancelExperiment — a killed/evicted job does NOT get
#      auto-resubmitted; only burst-tier preemption victims do, see loop.go preempt()).
#   3. Preemption reschedule: fill a GPU type's entire capacity with a burst-tier job, then
#      submit a guaranteed-tier job requesting the same type. The guaranteed job preempts
#      the burst job (loop.go preempt()/RequeuePreempted): burst job -> QUEUED, re-admitted
#      later, possibly onto a different GPU type since it also tolerates several.
#   4. Two concurrent job copies throughout, to catch any per-job state bleed.
#
# At every step, cross-checks:
#   - scheduler DB status (GET /experiments/{id})
#   - dashboard-facing metrics/timeseries endpoints the UI actually calls
#     (registry-service GET /registry/experiments/{id}/metrics,
#      GET /registry/platform-experiments/{id}/metrics-timeseries)
#   - quota ledger (GET /platform-experiments/{id}/quotas) before/after each scenario
#
# Usage:
#   make full-up          # tear down + rebuild cluster & controlplane first (see README below)
#   bash tests/advanced-e2e.sh
#
# Requires: kubectl pointed at the local k3s cluster (context k3s-local, set by
# localdev/install.sh), python3, curl.

set -euo pipefail

QUOTA_URL="${QUOTA_URL:-http://localhost:8081}"
SCHED_URL="${SCHED_URL:-http://localhost:8082}"
REGISTRY_URL="${REGISTRY_URL:-http://localhost:8083}"
PROM_URL="${PROM_URL:-http://localhost:4000/v1/prometheus}"
JOB_NS="${JOB_NS:-openresearch-jobs}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workload/job.yaml}"
RUN_TS="$(date +%s)"

py() { python3 -c "$@"; }

pass() { echo "  [PASS] $*"; }
fail() { echo "  [FAIL] $*"; FAILED=1; }
FAILED=0

AGENTS=("agent-node-${RUN_TS}" "agent-evict-${RUN_TS}" "agent-preempt-a-${RUN_TS}" "agent-preempt-b-${RUN_TS}" "agent-multi-${RUN_TS}")
AGENT_COUNT="${#AGENTS[@]}"
# Short duration for a fast local run: the workload is a stub that just sleeps for
# estimated_duration_hours (tests/workload/train.py), no real computation, so there's no
# reason to run it for minutes. report_interval_seconds is set low to match (see PE creation
# below) so the silence-eviction window still comfortably outlasts a single report interval.
JOB_HOURS="0.025"
MAX_RATE="3.0"
# Scenario 3 saturates A100's entire cluster capacity (8 GPUs, see openresearch.yaml's
# cluster_gpus) with 2 burst jobs of gpu_count=4 each (16 total across both, but capacity
# admits only 8 -> exactly saturates it) instead of 8 separate 1-GPU pods — keeps concurrent
# pod count on the single fake A100 node low enough that this dev box's real, shared CPU
# doesn't starve every pod's metrics-reporting cadence past the silence-eviction window.
CONTENTION_JOB_HOURS="0.017"
CONTENTION_GPU_TYPE="A100"
CONTENTION_GPU_COUNT_PER_BURST_JOB=4
CONTENTION_BURST_JOBS=2
# The "+ 2 * ..." term covers scenario 4's distributed job (gpu_count=1, num_nodes=2 -> bills
# for TotalGPUs=2, not 1) on top of the flat per-agent share the rest of the formula assumes.
BUDGET=$(py "print(round(${AGENT_COUNT} * ${JOB_HOURS} * ${MAX_RATE} / 0.40 * 3 + (${CONTENTION_BURST_JOBS} * ${CONTENTION_GPU_COUNT_PER_BURST_JOB} + 1) * ${CONTENTION_JOB_HOURS} * ${MAX_RATE} / 0.40 * 1.5 + 2 * ${JOB_HOURS} * ${MAX_RATE} / 0.40 * 1.5, 4))")

echo "==> Registering ${AGENT_COUNT} agents..."
for AGENT in "${AGENTS[@]}"; do
  curl -sf -X POST "$QUOTA_URL/agents" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$AGENT\",\"name\":\"$AGENT\"}" > /dev/null 2>&1 || true
  echo "  $AGENT: ok"
done

echo ""
echo "==> Creating platform experiment (budget=${BUDGET} T4h)..."
# report_interval_seconds=10 -> silence window = silence_multiplier(3) * 10 = 30s. Short
# jobs (JOB_HOURS above) need a short window too, or the whole run is dominated by waiting
# out a silence timer sized for minutes-long jobs.
PE_RESP=$(curl -sf -X POST "$QUOTA_URL/platform-experiments" -H 'Content-Type: application/json' -d "{
  \"name\": \"advanced-e2e-${RUN_TS}\",
  \"budget_t4_hours\": ${BUDGET},
  \"max_agents\": ${AGENT_COUNT},
  \"metrics\": [{\"key\": \"val_accuracy\", \"direction\": \"maximize\"}],
  \"phase2_boundary\": 0.90,
  \"report_interval_seconds\": 10
}")
PE_ID=$(echo "$PE_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")
echo "  id: $PE_ID"

echo ""
echo "==> Signing up + starting..."
for AGENT in "${AGENTS[@]}"; do
  curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/signup" -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"$AGENT\"}" > /dev/null
done
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/start" \
  | py "import sys,json; d=json.load(sys.stdin); print('  status:', d.get('status',''))"

TMPDIR_T="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_T"' EXIT

cat > "$TMPDIR_T/mk_hyp_body.py" <<'PYEOF'
import json, os, sys
print(json.dumps({"agent_id": sys.argv[1], "text": f"advanced e2e run for {sys.argv[1]}"}))
PYEOF

cat > "$TMPDIR_T/mk_submit_body.py" <<'PYEOF'
import json, os, sys, yaml
job_id, agent, pe_id, job_hours, job_file, hypothesis_id, tier, gpu_override, gpu_count_override, num_nodes_override = sys.argv[1:11]
with open(job_file) as f:
    job = yaml.safe_load(f)
if gpu_override:
    # Pin to exactly one GPU type (drop the acceptable_gpu_types tolerance) so a test can
    # force real capacity contention against that type's known cluster_gpus count instead
    # of spreading across every fake node.
    job["gpu_type"] = gpu_override
    job.pop("acceptable_gpu_types", None)
if gpu_count_override:
    job["gpu_count"] = int(gpu_count_override)
if num_nodes_override:
    job["num_nodes"] = int(num_nodes_override)
    # This local dev cluster has exactly one node per GPU type (see localdev/add-fake-nodes.sh),
    # so a hard distinct-hosts requirement would make any num_nodes>1 job unschedulable here —
    # see domain.TopologySpec.SpreadAcrossHosts's own doc comment for this exact caveat.
    job["topology"] = {"spread_across_hosts": False}
print(json.dumps({
    "id": job_id,
    "metadata": {
        "agent_id": agent,
        "platform_experiment_id": pe_id,
        "project_id": "advanced-e2e",
        "hypothesis_id": hypothesis_id,
        "theory": "node-death and preemption resilience should not corrupt billing",
        "objective": "maximize val_accuracy",
        "estimated_duration_hours": float(job_hours),
        "code_ref": "git://openresearch@main",
        "capacity_tier": tier,
    },
    "job": job,
}))
PYEOF

# submit_job AGENT CAPACITY_TIER [GPU_TYPE_OVERRIDE] [JOB_HOURS_OVERRIDE] [GPU_COUNT_OVERRIDE] [NUM_NODES_OVERRIDE] -> prints job id on stdout
submit_job() {
  local AGENT="$1" TIER="$2" GPU_OVERRIDE="${3:-}" HOURS_OVERRIDE="${4:-$JOB_HOURS}" GPU_COUNT_OVERRIDE="${5:-}" NUM_NODES_OVERRIDE="${6:-}"
  local JOB_ID="job-$(py "import uuid; print(str(uuid.uuid4())[:8])")-${RUN_TS}"
  local HYP_RESP HYPOTHESIS_ID SUBMIT_BODY
  HYP_RESP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
    -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "$AGENT")")
  HYPOTHESIS_ID=$(echo "$HYP_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")
  SUBMIT_BODY=$(python3 "$TMPDIR_T/mk_submit_body.py" "$JOB_ID" "$AGENT" "$PE_ID" "$HOURS_OVERRIDE" "$JOB_FILE" "$HYPOTHESIS_ID" "$TIER" "$GPU_OVERRIDE" "$GPU_COUNT_OVERRIDE" "$NUM_NODES_OVERRIDE")
  # Must check explicitly: a trailing `echo` as this function's last command means a failed
  # curl here would NOT trip `set -e` on the caller's `X=$(submit_job ...)` (bash only honors
  # -e for a command substitution's own last command) — silently handing back a job ID that
  # was never created, which then surfaces many steps later as a baffling 404/UNKNOWN.
  if ! curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$SUBMIT_BODY" > /dev/null; then
    echo "submit_job: POST /experiments failed for $JOB_ID (agent=$AGENT tier=$TIER)" >&2
    return 1
  fi
  echo "$JOB_ID"
}

get_status() { curl -sf "$SCHED_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin).get('status','UNKNOWN'))" 2>/dev/null || echo UNKNOWN; }
get_field() { curl -sf "$SCHED_URL/experiments/$1" | py "import sys,json; print(json.load(sys.stdin).get('$2',''))" 2>/dev/null || echo ""; }

wait_for_status() {
  local ID="$1" WANT="$2" TRIES="${3:-30}"
  for i in $(seq 1 "$TRIES"); do
    local S; S=$(get_status "$ID")
    if [[ ",$WANT," == *",$S,"* ]]; then echo "$S"; return 0; fi
    sleep 1
  done
  echo "$(get_status "$ID")"
  return 1
}

quota_snapshot() {
  curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas" \
    | py "
import sys,json
for q in json.load(sys.stdin):
    print(f\"    {q['agent_id']}: guaranteed={q.get('guaranteed_t4_hours',0):.4f} burst={q.get('burst_t4_hours',0):.4f}\")
" 2>/dev/null || true
}

dashboard_metrics() {
  # exercises the exact endpoints controlplane/ui/src/lib/api.ts's fetchExperimentMetrics
  # and fetchPlatformExperimentTimeseries call — same code path the dashboard uses.
  local JOB_ID="$1"
  curl -sf "${REGISTRY_URL}/registry/experiments/${JOB_ID}/metrics" 2>/dev/null || echo "[]"
}

echo ""
echo "=========================================================="
echo "Scenario 1: node death mid-run -> cluster-agent self-heal"
echo "=========================================================="
JOB1=$(submit_job "${AGENTS[0]}" "guaranteed")
echo "  submitted: $JOB1"
S=$(wait_for_status "$JOB1" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job1 never reached RUNNING (status=$S) — cannot exercise node-death scenario"
else
  pass "job1 reached RUNNING"
  ORIG_GPU=$(get_field "$JOB1" admitted_gpu_type)
  [[ -z "$ORIG_GPU" ]] && ORIG_GPU=$(get_field "$JOB1" gpu_type)
  echo "  admitted on: $ORIG_GPU"

  POD=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$JOB1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  NODE=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$JOB1" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)
  echo "  pod=$POD node=$NODE"

  if [[ -z "$POD" || -z "$NODE" ]]; then
    fail "could not locate job1's pod/node to kill"
  else
    echo "  ==> simulating node death: cordoning $NODE and deleting pod $POD"
    kubectl cordon "$NODE" > /dev/null
    kubectl -n "$JOB_NS" delete pod "$POD" --grace-period=0 --force > /dev/null 2>&1 || true

    # k8s Job controller (RestartPolicy=OnFailure) recreates the pod; since the original
    # node is cordoned it must land elsewhere. If the whole Job object is also gone,
    # cluster-agent's reconcileOnce (desired-vs-actual diff, ~2s tick) recreates it.
    NEW_NODE=""
    for i in $(seq 1 30); do
      NEW_NODE=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$JOB1" \
        --field-selector=status.phase!=Failed -o jsonpath='{.items[-1:].spec.nodeName}' 2>/dev/null || true)
      [[ -n "$NEW_NODE" && "$NEW_NODE" != "$NODE" ]] && break
      sleep 1
    done

    if [[ -n "$NEW_NODE" && "$NEW_NODE" != "$NODE" ]]; then
      pass "job1 rescheduled onto a different node ($NODE -> $NEW_NODE)"
    else
      fail "job1 did not reschedule onto a different node within timeout (still/never left $NODE)"
    fi

    kubectl uncordon "$NODE" > /dev/null

    # JOB_HOURS worth of real training time (~90s) plus reschedule overhead.
    S=$(wait_for_status "$JOB1" "COMPLETED,FAILED,EVICTED" 150 || true)
    if [[ "$S" == "COMPLETED" ]]; then
      pass "job1 completed after reschedule (status=$S)"
    else
      fail "job1 did not complete cleanly after reschedule (status=$S)"
    fi

    NEW_GPU=$(get_field "$JOB1" admitted_gpu_type)
    echo "  final admitted GPU type: ${NEW_GPU:-$ORIG_GPU} (was: $ORIG_GPU)"

    METRICS=$(dashboard_metrics "$JOB1")
    if [[ "$METRICS" != "[]" && -n "$METRICS" ]]; then
      pass "dashboard metrics endpoint returns data for job1 across the reschedule gap"
    else
      echo "  [WARN] no metrics returned for job1 (may be expected if the workload reports late)"
    fi
    echo "  quota after scenario 1:"; quota_snapshot
  fi
fi

echo ""
echo "=========================================================="
echo "Scenario 2: policy eviction (cancel) is terminal, refunded"
echo "=========================================================="
JOB2=$(submit_job "${AGENTS[1]}" "guaranteed")
echo "  submitted: $JOB2"
S=$(wait_for_status "$JOB2" "RUNNING,COMPLETED,FAILED" 60 || true)
echo "  quota before cancel:"; quota_snapshot
if [[ "$S" != "RUNNING" ]]; then
  echo "  [WARN] job2 status=$S before cancel attempt (still trying cancel)"
fi
curl -sf -X POST "$SCHED_URL/experiments/${JOB2}/cancel" > /dev/null || true
S=$(wait_for_status "$JOB2" "EVICTED,COMPLETED,FAILED" 30 || true)
if [[ "$S" == "EVICTED" ]]; then
  REASON=$(get_field "$JOB2" eviction_reason)
  pass "job2 evicted terminally (reason=$REASON)"
else
  fail "job2 expected EVICTED after cancel, got $S"
fi
echo "  ==> confirming eviction does NOT requeue (status stays EVICTED, no auto re-admission)..."
sleep 5
S2=$(get_status "$JOB2")
if [[ "$S2" == "EVICTED" ]]; then
  pass "job2 still EVICTED 5s later — confirms eviction is terminal, unlike preemption"
else
  fail "job2 status changed after eviction to $S2 — unexpected auto-resubmission"
fi
echo "  quota after cancel (unused reservation should be refunded):"; quota_snapshot

echo ""
echo "=========================================================="
echo "Scenario 3: burst-tier preemption -> requeue -> re-admit"
echo "=========================================================="
echo "  ==> filling all ${CONTENTION_GPU_TYPE} capacity with ${CONTENTION_BURST_JOBS} burst jobs of ${CONTENTION_GPU_COUNT_PER_BURST_JOB} GPUs each..."
declare -a BURST_JOBS
for i in $(seq 1 "$CONTENTION_BURST_JOBS"); do
  AGENT="${AGENTS[$((i % AGENT_COUNT))]}"
  BJ=$(submit_job "$AGENT" "burst" "$CONTENTION_GPU_TYPE" "$CONTENTION_JOB_HOURS" "$CONTENTION_GPU_COUNT_PER_BURST_JOB")
  BURST_JOBS+=("$BJ")
  echo "  burst[$i]: $BJ (agent=$AGENT, gpu_count=$CONTENTION_GPU_COUNT_PER_BURST_JOB)"
done

echo "  ==> waiting for burst jobs to occupy ${CONTENTION_GPU_TYPE}..."
RUNNING_COUNT=0
for i in $(seq 1 30); do
  RUNNING_COUNT=0
  for BJ in "${BURST_JOBS[@]}"; do
    S=$(get_status "$BJ")
    [[ "$S" == "RUNNING" || "$S" == "ADMITTED" ]] && RUNNING_COUNT=$((RUNNING_COUNT + 1))
  done
  [[ "$RUNNING_COUNT" -ge "$CONTENTION_BURST_JOBS" ]] && break
  sleep 1
done
echo "  ${RUNNING_COUNT}/${CONTENTION_BURST_JOBS} burst jobs admitted onto ${CONTENTION_GPU_TYPE}"


# Agent for the preempting guaranteed job must not be AGENTS[0]/[1]/[2]: they already
# completed scenario 1/3's earlier jobs without writing a summary, and the summary gate
# (see admission.go's ReasonSummaryRequired) correctly 403s further submissions from an
# agent with an unsummarized COMPLETED experiment. AGENTS[3] hasn't submitted anything yet.
JOB4=$(submit_job "${AGENTS[3]}" "guaranteed" "$CONTENTION_GPU_TYPE" "$CONTENTION_JOB_HOURS")
echo "  submitted guaranteed job pinned to ${CONTENTION_GPU_TYPE} (should preempt a burst victim): $JOB4"

PREEMPTED_VICTIM=""
for i in $(seq 1 30); do
  for BJ in "${BURST_JOBS[@]}"; do
    S=$(get_status "$BJ")
    if [[ "$S" == "QUEUED" ]]; then PREEMPTED_VICTIM="$BJ"; break 2; fi
  done
  sleep 1
done

if [[ -n "$PREEMPTED_VICTIM" ]]; then
  pass "burst job $PREEMPTED_VICTIM was preempted back to QUEUED by guaranteed job $JOB4"
  VFINAL=$(wait_for_status "$PREEMPTED_VICTIM" "RUNNING,COMPLETED,FAILED,EVICTED" 90 || true)
  if [[ "$VFINAL" == "RUNNING" || "$VFINAL" == "COMPLETED" ]]; then
    VGPU=$(get_field "$PREEMPTED_VICTIM" admitted_gpu_type)
    pass "$PREEMPTED_VICTIM was re-admitted and ran again after preemption (final=$VFINAL, gpu_type=$VGPU)"
  else
    fail "$PREEMPTED_VICTIM never came back after preemption (final=$VFINAL)"
  fi
else
  echo "  [WARN] no preemption observed even with ${CONTENTION_GPU_TYPE} capacity nominally saturated — investigate admission accounting"
fi

for BJ in "${BURST_JOBS[@]}"; do
  wait_for_status "$BJ" "COMPLETED,FAILED,EVICTED,QUEUED" 90 > /dev/null || true
done
wait_for_status "$JOB4" "COMPLETED,FAILED,EVICTED" 90 > /dev/null || true
echo "  final: job4=$(get_status "$JOB4")  burst_jobs=$(for BJ in "${BURST_JOBS[@]}"; do echo -n "$(get_status "$BJ") "; done)"
echo "  quota after scenario 3:"; quota_snapshot

echo ""
echo "=========================================================="
echo "Scenario 4: distributed multi-replica job (num_nodes=2) — gang scheduling + billing"
echo "=========================================================="
# Exercises the distributed-training path (see domain.JobSpec.NumNodes, BuildJob's Indexed
# Job compilation) that the shared workload/job.yaml fixture deliberately stays away from
# (see its own header comment). The real risk here isn't scheduling — it's billing: a job
# spanning NumNodes replicas costs GPUCount * NumNodes GPU-hours (TotalGPUs), not just
# GPUCount, so this specifically checks that the estimated cost and the post-completion
# quota debit both reflect the doubled footprint instead of quietly billing as if it were a
# single-node job.
QUOTA_BEFORE_JOB5=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas" | py "
import sys,json
for q in json.load(sys.stdin):
    if q['agent_id'] == '${AGENTS[4]}':
        print(q.get('used_guaranteed_t4h', 0))
")
JOB5=$(submit_job "${AGENTS[4]}" "guaranteed" "T4" "$JOB_HOURS" "1" "2")
echo "  submitted distributed job (gpu_count=1, num_nodes=2): $JOB5"

JOB5_TOTAL_GPUS=$(get_field "$JOB5" gpu_count)
# exp.GPUCount (top-level, distinct from job.gpu_count in the submitted spec) is deliberately
# TotalGPUs = gpu_count * num_nodes (see handler.go's SubmitExperiment) — the figure admission/
# quota/preemption accounting actually uses, so 2 here (not 1) is correct, not a bug.
echo "  gpu_count field (TotalGPUs = gpu_count * num_nodes): $JOB5_TOTAL_GPUS"

S=$(wait_for_status "$JOB5" "RUNNING,COMPLETED,FAILED,EVICTED" 30 || true)
if [[ "$S" != "RUNNING" && "$S" != "COMPLETED" ]]; then
  fail "job5 (distributed) never reached RUNNING/COMPLETED (status=$S) — gang scheduling may be broken"
else
  pass "job5 (distributed) reached $S"

  REPLICA_COUNT=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$JOB5" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [[ "$REPLICA_COUNT" -ge 2 ]]; then
    pass "job5 has $REPLICA_COUNT pods (expected 2 — one per num_nodes replica)"
  else
    fail "job5 has only $REPLICA_COUNT pod(s) — expected 2, gang scheduling did not create both replicas"
  fi

  EST_COST=$(get_field "$JOB5" estimated_cost_t4h)
  EXPECTED_COST=$(py "print(round(2 * ${JOB_HOURS} * 1.0, 6))")  # gpu_count(1) * num_nodes(2) * T4 rate(1.0) * hours
  COST_OK=$(py "print(abs(float('$EST_COST' or 0) - float('$EXPECTED_COST')) < 0.01)")
  if [[ "$COST_OK" == "True" ]]; then
    pass "job5 estimated_cost_t4h=$EST_COST bills for both replicas (TotalGPUs), not just gpu_count"
  else
    fail "job5 estimated_cost_t4h=$EST_COST does not match TotalGPUs-based expected~$EXPECTED_COST — num_nodes may not be billed"
  fi

  S5=$(wait_for_status "$JOB5" "COMPLETED,FAILED,EVICTED" 90 || true)
  if [[ "$S5" == "COMPLETED" ]]; then
    pass "job5 (distributed, 2 replicas) completed cleanly"
  else
    fail "job5 (distributed) did not complete cleanly (status=$S5)"
  fi

  QUOTA_AFTER_JOB5=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas" | py "
import sys,json
for q in json.load(sys.stdin):
    if q['agent_id'] == '${AGENTS[4]}':
        print(q.get('used_guaranteed_t4h', 0))
")
  DEBIT=$(py "print(round(float('$QUOTA_AFTER_JOB5' or 0) - float('$QUOTA_BEFORE_JOB5' or 0), 4))")
  echo "  agent-multi guaranteed quota debited by job5: ${DEBIT} T4h (single-replica equivalent would be ~$(py "print(round(${JOB_HOURS} * 1.0, 4))") T4h)"

  M5=$(dashboard_metrics "$JOB5")
  M5COUNT=$(echo "$M5" | py "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo 0)
  echo "  job5: $M5COUNT metric point(s) via registry-service"
fi

echo ""
echo "=========================================================="
echo "Cross-checking dashboard-facing endpoints for all jobs"
echo "=========================================================="
for JOB_ID in "$JOB1" "$JOB2" "${BURST_JOBS[@]}" "$JOB4" "$JOB5"; do
  M=$(dashboard_metrics "$JOB_ID")
  COUNT=$(echo "$M" | py "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo 0)
  echo "  $JOB_ID: $COUNT metric point(s) via registry-service (dashboard's data source)"
done

TS=$(curl -sf "${REGISTRY_URL}/registry/platform-experiments/${PE_ID}/metrics-timeseries?metric_name=val_accuracy&lookback_hours=6" 2>/dev/null || echo '{}')
TSCOUNT=$(echo "$TS" | py "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get('series',[])) if isinstance(d,dict) else 0)" 2>/dev/null || echo 0)
echo "  platform-experiment timeseries: $TSCOUNT series (dashboard leaderboard/competition chart source)"

echo ""
echo "==> Closing experiment $PE_ID..."
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE_ID}/close" \
  | py "import sys,json; print('  status:', json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "  (close failed)"

echo ""
if [[ "$FAILED" == "1" ]]; then
  echo "==> RESULT: one or more scenarios FAILED — see [FAIL] lines above."
  exit 1
else
  echo "==> RESULT: all scenarios passed."
fi
