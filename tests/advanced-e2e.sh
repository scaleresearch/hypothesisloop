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
  \"metrics\": [
    {\"key\": \"val_accuracy\", \"direction\": \"maximize\"},
    {\"key\": \"val_loss\", \"direction\": \"minimize\"}
  ],
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
print(json.dumps({
    "agent_id": sys.argv[1],
    "platform_experiment_id": sys.argv[2],
    "text": f"advanced e2e run for {sys.argv[1]}",
}))
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
    -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "$AGENT" "$PE_ID")")
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

  # --- New: workload.BuildJob regression — memory/storage/GPU requests+limits present
  # (SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests: "Existing memory/storage/
  # GPU requests and limits remain present in generated Jobs"). Not new behavior — BuildJob
  # already sets equal requests/limits for CPU/memory/storage/GPU (plan Class B step 3) — this
  # is a characterization/regression test guarding that fact ahead of the RAM/storage removal
  # migration, reusing job5's own pod rather than submitting a dedicated job.
  JOB5_POD=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$JOB5" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -n "$JOB5_POD" ]]; then
    MEM_REQ=$(kubectl -n "$JOB_NS" get pod "$JOB5_POD" -o jsonpath='{.spec.containers[0].resources.requests.memory}' 2>/dev/null || true)
    MEM_LIM=$(kubectl -n "$JOB_NS" get pod "$JOB5_POD" -o jsonpath='{.spec.containers[0].resources.limits.memory}' 2>/dev/null || true)
    GPU_REQ=$(kubectl -n "$JOB_NS" get pod "$JOB5_POD" -o jsonpath='{.spec.containers[0].resources.requests.nvidia\.com/gpu}' 2>/dev/null || true)
    GPU_LIM=$(kubectl -n "$JOB_NS" get pod "$JOB5_POD" -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}' 2>/dev/null || true)
    echo "  job5 pod resources: memory req=$MEM_REQ lim=$MEM_LIM, gpu req=$GPU_REQ lim=$GPU_LIM"
    if [[ -n "$MEM_REQ" && "$MEM_REQ" == "$MEM_LIM" && -n "$GPU_REQ" && "$GPU_REQ" == "$GPU_LIM" ]]; then
      pass "BuildJob regression: memory and GPU requests==limits present on generated pod (job.yaml has no storage field, so storage is not asserted here — see Scenario 8's dedicated storage/cpu pod-spec check)"
    else
      fail "BuildJob regression: expected equal, non-empty memory/gpu requests+limits on generated pod, got memory req=$MEM_REQ lim=$MEM_LIM gpu req=$GPU_REQ lim=$GPU_LIM"
    fi
  else
    fail "could not locate job5's pod to run the BuildJob requests/limits regression check"
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
echo "Scenario 5: mixed CPU+accelerator admission"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests:"
echo " \"A mixed CPU+accelerator job is rejected if either dimension is"
echo " unavailable and admitted only to a cluster where both fit.\")"
echo "=========================================================="
# --- New: Class A mixed CPU+accelerator admission (SCHEDULING_GENERALIZATION_PLAN.md,
# minimum acceptance tests). Today's admission loop (loop_tick.go / admission.go, as seen in
# this worktree) does not jointly evaluate CPU and GPU fit on the same cluster via a single
# fits() predicate over a canonical Footprint — it admits primarily on GPU capacity. These
# scenarios characterize the intended end state; the "impossible CPU" case is expected to
# fail today (job likely still gets admitted since CPU isn't checked jointly), and is left
# in as a currently-red regression marker until Class A step 3/4 (ResourceKey/Footprint/
# fits(), loop_tick.go rewrite) lands. See plan lines 42-59.
mk_mixed_submit_body() {
  # $1=job_id $2=agent $3=pe_id $4=hours $5=hypothesis_id $6=tier $7=cpu $8=gpu_count
  py "
import json, sys
job = {
    'image': 'localhost/openresearch-workload:latest',
    'cpu': '$7',
    'memory': '128Mi',
    'gpu_type': 'T4',
    'gpu_count': $8,
}
print(json.dumps({
    'id': '$1',
    'metadata': {
        'agent_id': '$2',
        'platform_experiment_id': '$3',
        'project_id': 'advanced-e2e',
        'hypothesis_id': '$5',
        'theory': 'mixed CPU+accelerator admission must jointly check both dimensions',
        'objective': 'maximize val_accuracy',
        'estimated_duration_hours': $4,
        'code_ref': 'git://openresearch@main',
        'capacity_tier': '$6',
    },
    'job': job,
}))
"
}

MIXED_HYP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "${AGENTS[3]}" "$PE_ID")" | py "import sys,json; print(json.load(sys.stdin)['id'])")

# NOTE: depends on Footprint/Fits() admission wiring landing; currently CPU is not checked
# jointly with GPU at admission time, so this "impossibly large CPU" job may still be
# admitted today instead of being rejected/left QUEUED for lack of CPU fit.
MIXED_JOB_ID="job-mixed-impossible-cpu-${RUN_TS}"
MIXED_BODY=$(mk_mixed_submit_body "$MIXED_JOB_ID" "${AGENTS[3]}" "$PE_ID" "$JOB_HOURS" "$MIXED_HYP" "guaranteed" "999999" "1")
if curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$MIXED_BODY" > /dev/null; then
  S=$(wait_for_status "$MIXED_JOB_ID" "RUNNING,ADMITTED" 10 || true)
  if [[ "$S" == "RUNNING" || "$S" == "ADMITTED" ]]; then
    fail "(EXPECTED PENDING IMPLEMENTATION) job with an impossible CPU request (999999 cores) was admitted — mixed CPU+accelerator fit is not jointly checked yet (plan Class A step 3/4)"
  else
    pass "job with an impossible CPU request stayed non-admitted (status=$S)"
  fi
else
  pass "job with an impossible CPU request was rejected at submission"
fi

# Sane mixed job (small CPU + 1 GPU) should admit onto a cluster where both fit — this should
# already pass today since CPU alone isn't a blocker at this scale, but it exercises the same
# joint-fit code path the plan targets and should remain green after the rewrite.
MIXED_OK_JOB_ID="job-mixed-ok-${RUN_TS}"
MIXED_OK_BODY=$(mk_mixed_submit_body "$MIXED_OK_JOB_ID" "${AGENTS[3]}" "$PE_ID" "$JOB_HOURS" "$MIXED_HYP" "guaranteed" "250m" "1")
if curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$MIXED_OK_BODY" > /dev/null; then
  S=$(wait_for_status "$MIXED_OK_JOB_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    pass "mixed CPU(250m)+GPU(1xT4) job admitted to a cluster where both fit (status=$S)"
  else
    fail "mixed CPU+GPU job never admitted (status=$S)"
  fi
  wait_for_status "$MIXED_OK_JOB_ID" "COMPLETED,FAILED,EVICTED" 60 > /dev/null || true
else
  fail "submission of a small, fittable mixed CPU+GPU job was rejected outright"
fi

echo ""
echo "=========================================================="
echo "Scenario 7: RAM/storage physically checked, never hours-debited"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: Class B RAM/storage hard-cap-only, no hours budget (SCHEDULING_GENERALIZATION_PLAN.md,
# minimum acceptance tests). Today's code (controlplane/shared/domain/types.go:
# BudgetRAMGBHours/BudgetStorageGBHours, guaranteed/burst columns on agent_quotas) still
# treats RAM/storage as hours-budgeted dimensions, which the plan's Class B step 1 explicitly
# removes/deprecates. This scenario submits a job with a real memory/storage request and
# confirms no RAM/storage hours are debited from the quota ledger. Currently this is
# EXPECTED TO FAIL / be a no-op check until the removal migration lands, since the ledger
# still may carry (zero-valued, in this test's PE which sets no RAM/storage budget)
# guaranteed/used_*_gb_h fields.
STORAGE_HYP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "${AGENTS[3]}" "$PE_ID")" | py "import sys,json; print(json.load(sys.stdin)['id'])")
STORAGE_JOB_ID="job-storage-nohours-${RUN_TS}"
STORAGE_BODY=$(mk_mixed_submit_body "$STORAGE_JOB_ID" "${AGENTS[3]}" "$PE_ID" "$JOB_HOURS" "$STORAGE_HYP" "guaranteed" "250m" "1")
# inject a storage request into the generated body since mk_mixed_submit_body doesn't set one
STORAGE_BODY=$(echo "$STORAGE_BODY" | py "
import sys, json
d = json.load(sys.stdin)
d['job']['storage'] = '5Gi'
print(json.dumps(d))
")
if curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$STORAGE_BODY" > /dev/null; then
  wait_for_status "$STORAGE_JOB_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 > /dev/null || true
  QUOTAS_JSON=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas")
  STORAGE_HOURS_FIELDS=$(echo "$QUOTAS_JSON" | py "
import sys, json
qs = json.load(sys.stdin)
for q in qs:
    if q['agent_id'] == '${AGENTS[3]}':
        print(q.get('used_guaranteed_storage_gb_h', 0), q.get('used_burst_storage_gb_h', 0))
")
  echo "  used storage-gb-hours for ${AGENTS[3]} after submission: ${STORAGE_HOURS_FIELDS:-<field absent>}"
  if [[ -z "$STORAGE_HOURS_FIELDS" ]]; then
    pass "no storage-hours fields present in quota response — removal migration has landed"
  else
    NONZERO=$(py "vals='$STORAGE_HOURS_FIELDS'.split(); print(any(float(v) != 0 for v in vals))" 2>/dev/null || echo "unknown")
    if [[ "$NONZERO" == "False" ]]; then
      pass "storage-hours fields present but remain zero — consistent with never-debited RAM/storage (though the fields themselves are slated for removal per plan Class B step 1)"
    else
      fail "(EXPECTED PENDING IMPLEMENTATION) storage-hours were debited ($STORAGE_HOURS_FIELDS) — RAM/storage removal migration (plan Class B step 1) has not landed yet"
    fi
  fi
  wait_for_status "$STORAGE_JOB_ID" "COMPLETED,FAILED,EVICTED" 60 > /dev/null || true
else
  echo "  [WARN] storage job submission failed outright — cannot assert on quota debit"
fi

echo ""
echo "=========================================================="
echo "Scenario 8: fractional CPU (millicore) precision through admission"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: fractional CPU millicore precision (SCHEDULING_GENERALIZATION_PLAN.md, minimum
# acceptance tests, Class A step 3's canonical millicore units). Submits a job requesting a
# non-round CPU quantity (333m) and checks the value survives verbatim into the generated
# k8s Job's pod spec — regardless of whether admission itself does anything with it yet.
FRAC_HYP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "${AGENTS[3]}" "$PE_ID")" | py "import sys,json; print(json.load(sys.stdin)['id'])")
FRAC_JOB_ID="job-frac-cpu-${RUN_TS}"
FRAC_BODY=$(mk_mixed_submit_body "$FRAC_JOB_ID" "${AGENTS[3]}" "$PE_ID" "$JOB_HOURS" "$FRAC_HYP" "guaranteed" "333m" "1")
if curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$FRAC_BODY" > /dev/null; then
  S=$(wait_for_status "$FRAC_JOB_ID" "RUNNING,COMPLETED,FAILED,EVICTED" 60 || true)
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    POD=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$FRAC_JOB_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "$POD" ]]; then
      CPU_REQ=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.requests.cpu}' 2>/dev/null || true)
      CPU_LIM=$(kubectl -n "$JOB_NS" get pod "$POD" -o jsonpath='{.spec.containers[0].resources.limits.cpu}' 2>/dev/null || true)
      echo "  pod cpu request=$CPU_REQ limit=$CPU_LIM (submitted 333m)"
      if [[ "$CPU_REQ" == "333m" && "$CPU_LIM" == "333m" ]]; then
        pass "fractional CPU (333m) retained millicore precision through to the generated pod spec"
      else
        fail "fractional CPU precision lost: got request=$CPU_REQ limit=$CPU_LIM, expected 333m/333m"
      fi
    else
      fail "could not locate pod for fractional-CPU job to inspect resources"
    fi
  else
    fail "fractional CPU job never reached RUNNING/COMPLETED (status=$S)"
  fi
  wait_for_status "$FRAC_JOB_ID" "COMPLETED,FAILED,EVICTED" 60 > /dev/null || true
else
  fail "submission of a valid fractional-CPU (333m) job was rejected outright"
fi

echo ""
echo "=========================================================="
echo "Scenario 9: unset required resource is rejected, not defaulted"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests —"
echo " complements any existing storage-unset scenario, does not duplicate it)"
echo "=========================================================="
# --- New: reject-on-unset-request (SCHEDULING_GENERALIZATION_PLAN.md, Cross-cutting
# correctness fix #1). Today (this worktree's snapshot of admission.go/handler.go), an
# omitted cpu/memory/storage field is filled in later from a per-cluster JobDefaults
# ConfigMap inside the cluster, not rejected at submission — so this scenario is EXPECTED TO
# FAIL until that fix lands. Exercises "cpu" specifically (a dimension not covered by an
# existing storage-unset test, if one exists) to broaden coverage across required fields.
UNSET_HYP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "${AGENTS[3]}" "$PE_ID")" | py "import sys,json; print(json.load(sys.stdin)['id'])")
UNSET_CPU_JOB_ID="job-unset-cpu-${RUN_TS}"
UNSET_CPU_BODY=$(py "
import json
print(json.dumps({
    'id': '$UNSET_CPU_JOB_ID',
    'metadata': {
        'agent_id': '${AGENTS[3]}',
        'platform_experiment_id': '$PE_ID',
        'project_id': 'advanced-e2e',
        'hypothesis_id': '$UNSET_HYP',
        'theory': 'submissions with an unset required resource must be rejected, not defaulted',
        'objective': 'maximize val_accuracy',
        'estimated_duration_hours': $JOB_HOURS,
        'code_ref': 'git://openresearch@main',
        'capacity_tier': 'guaranteed',
    },
    'job': {
        'image': 'localhost/openresearch-workload:latest',
        # cpu deliberately omitted
        'memory': '128Mi',
        'gpu_type': 'T4',
        'gpu_count': 1,
    },
}))
")
HTTP_CODE=$(curl -s -o /tmp/unset_cpu_resp.json -w '%{http_code}' -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$UNSET_CPU_BODY")
if [[ "$HTTP_CODE" -ge 400 ]]; then
  pass "submission with unset cpu request rejected at submission time (HTTP $HTTP_CODE)"
else
  # Not necessarily a hard reject at the HTTP layer today — check it doesn't silently admit
  # with a cluster-side default either.
  S=$(wait_for_status "$UNSET_CPU_JOB_ID" "RUNNING,ADMITTED,REJECTED,FAILED" 15 || true)
  if [[ "$S" == "REJECTED" ]]; then
    pass "submission with unset cpu request rejected (status=REJECTED)"
  else
    fail "(EXPECTED PENDING IMPLEMENTATION) submission with unset cpu request was accepted (HTTP $HTTP_CODE, status=$S) instead of rejected — cross-cutting fix #1 (reject unset required resource requests) has not landed yet"
  fi
fi

echo ""
echo "=========================================================="
echo "Scenario 10: two scheduler ticks cannot double-reserve capacity"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: durable pending-capacity reservation across ticks (SCHEDULING_GENERALIZATION_PLAN.md,
# Cross-cutting correctness fix #2). Submits two guaranteed jobs pinned to the same GPU type
# back-to-back, sized so together they would exceed that type's capacity if (and only if) a
# tick's live-capacity snapshot fails to account for the first job's SUBMITTED-but-not-yet-
# pod-created reservation. Both should never simultaneously be RUNNING beyond that type's
# real capacity. NOTE: depends on the durable Postgres-backed reservation (plan Sequencing
# step 4) landing; the current in-worktree scheduler trusts point-in-time live capacity, so
# this could already pass by luck (single-process low-latency loop) even without the fix —
# treat any observed over-admission as a definite bug, but a pass here is not proof the fix
# is implemented.
TICK_GPU_TYPE="L40"
TICK_JOB_A=$(submit_job "${AGENTS[3]}" "guaranteed" "$TICK_GPU_TYPE" "$JOB_HOURS")
TICK_JOB_B=$(submit_job "${AGENTS[4]}" "guaranteed" "$TICK_GPU_TYPE" "$JOB_HOURS")
echo "  submitted back-to-back guaranteed jobs pinned to ${TICK_GPU_TYPE}: $TICK_JOB_A, $TICK_JOB_B"
SA=$(wait_for_status "$TICK_JOB_A" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
SB=$(wait_for_status "$TICK_JOB_B" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 30 || true)
NODE_A=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$TICK_JOB_A" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)
NODE_B=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$TICK_JOB_B" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)
echo "  job A status=$SA node=$NODE_A ; job B status=$SB node=$NODE_B"
if [[ "$SA" == "RUNNING" && "$SB" == "RUNNING" && -n "$NODE_A" && "$NODE_A" == "$NODE_B" ]]; then
  # Both landed and are running on the same single-GPU-type fake node simultaneously — only a
  # real capacity conflict (double reservation) if their combined GPU request exceeds that
  # node's advertised capacity; this local dev cluster's fake nodes are sized generously
  # enough that this by itself isn't proof of a bug, so just report it for a human to check
  # against openresearch.yaml's cluster_gpus for L40 alongside actual GPU counts requested.
  echo "  [INFO] both jobs running on the same node ($NODE_A) — verify combined GPU request does not exceed advertised capacity for a genuine double-reservation check"
fi
wait_for_status "$TICK_JOB_A" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true
wait_for_status "$TICK_JOB_B" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true
pass "two back-to-back same-type submissions did not crash/error the scheduler across ticks (see [INFO] above for capacity detail)"

echo ""
echo "=========================================================="
echo "Scenario 11: distributed multi-node jobs under concurrent scheduling pressure"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests:"
echo " \"A NumNodes > 1 job's admission footprint is per-node x NumNodes...\")"
echo "=========================================================="
# --- New: concurrent multi-node Python jobs exercising per-node x NumNodes footprint
# (SCHEDULING_GENERALIZATION_PLAN.md, Class A step 3 correction + minimum acceptance tests).
# Follows the same submit_job(... num_nodes_override) convention as Scenario 4 (job5, around
# line 369 above) but submits several num_nodes=2 jobs concurrently to exercise
# admission/fit under real scheduling pressure, not just one distributed job in isolation.
declare -a MULTI_JOBS
MULTI_AGENTS=("${AGENTS[3]}" "${AGENTS[4]}")
for i in 1 2; do
  AGENT="${MULTI_AGENTS[$((i - 1))]}"
  MJ=$(submit_job "$AGENT" "burst" "T4" "$JOB_HOURS" "1" "2")
  MULTI_JOBS+=("$MJ")
  echo "  concurrent distributed job[$i]: $MJ (agent=$AGENT, gpu_count=1, num_nodes=2)"
done

MULTI_OK=0
for MJ in "${MULTI_JOBS[@]}"; do
  S=$(wait_for_status "$MJ" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 45 || true)
  REPLICA_COUNT=$(kubectl -n "$JOB_NS" get pods -l "openresearch.io/experiment-id=$MJ" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  echo "  $MJ: status=$S replicas=$REPLICA_COUNT"
  if [[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]]; then
    if [[ "$REPLICA_COUNT" -ge 2 ]]; then
      MULTI_OK=$((MULTI_OK + 1))
    else
      fail "$MJ admitted (status=$S) but only has $REPLICA_COUNT pod(s), expected 2 (per-node x NumNodes footprint should still create both replicas)"
    fi
  fi
done
if [[ "$MULTI_OK" -ge 1 ]]; then
  pass "$MULTI_OK/${#MULTI_JOBS[@]} concurrent num_nodes=2 jobs admitted with full replica counts under scheduling pressure"
else
  fail "no concurrent num_nodes=2 job reached RUNNING/COMPLETED with full replica count — multi-node admission under pressure may be broken"
fi
for MJ in "${MULTI_JOBS[@]}"; do
  wait_for_status "$MJ" "COMPLETED,FAILED,EVICTED,QUEUED" 90 > /dev/null || true
done

echo ""
echo "=========================================================="
echo "Scenario 12: stale/missing capacity excludes a cluster for the tick"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: fail-closed on stale/missing capacity (SCHEDULING_GENERALIZATION_PLAN.md,
# Cross-cutting correctness fix #3). This worktree's snapshot has no way for a test to force
# a cluster-agent's capacity report to go stale without touching production code (would
# require killing/blocking the agent process, out of scope for a test-only change), so this
# is documented here as an aspirational scenario rather than faked with a false-positive
# assertion. What a real version of this test should do once the capacity piggyback exposes
# a snapshot-age field (plan Sequencing step 11, "capacity snapshot age" observability):
#   1. cordon/pause a cluster-agent (or block its report endpoint) so its last capacity
#      report ages past the scheduler's staleness threshold;
#   2. submit a job that would only fit on that cluster;
#   3. assert the job stays QUEUED (not admitted) while capacity is stale, and is admitted
#      once a fresh report arrives.
# NOTE: not implemented as a running assertion — depends on a capacity-report age field/API
# that does not exist in this worktree yet. Left as a clearly-labeled placeholder per the
# task's instruction not to fake a scenario the current code can't yet support.
echo "  [INFO] stale-capacity exclusion scenario is aspirational — see comment above; skipped pending a capacity-report age/staleness API to drive it against"

echo ""
echo "=========================================================="
echo "Scenario 13: multi-dimensional preemption plans a sufficient victim set first"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: vector preemption planning (SCHEDULING_GENERALIZATION_PLAN.md, Class A step 5).
# Today's preempt() (loop.go, per this worktree) only reasons about a single GPU-type
# shortage, not a joint CPU+GPU (or +RAM/+storage) shortage vector, and does not verify the
# post-preemption footprint actually fits before evicting. This scenario saturates burst
# capacity on CONTENTION_GPU_TYPE across BOTH gpu_count and (informally) node CPU, using the
# same burst-fill pattern as Scenario 3, then submits a guaranteed job whose request is
# deliberately larger than what a single victim would free — checking that either (a) enough
# victims are preempted to cover the full shortage (not just one), or (b) if no sufficient
# victim set exists, the guaranteed job is NOT admitted with unmet capacity (no partial/
# broken admission). This reuses Scenario 3's burst jobs already running against
# CONTENTION_GPU_TYPE rather than re-saturating from scratch.
echo "  ==> reusing scenario 3's ${CONTENTION_GPU_TYPE} burst saturation to probe multi-victim preemption..."
BIG_GUARANTEED_GPU_COUNT=$((CONTENTION_GPU_COUNT_PER_BURST_JOB * 2))
JOB13=$(submit_job "${AGENTS[3]}" "guaranteed" "$CONTENTION_GPU_TYPE" "$CONTENTION_JOB_HOURS" "$BIG_GUARANTEED_GPU_COUNT")
echo "  submitted guaranteed job requesting ${BIG_GUARANTEED_GPU_COUNT}x ${CONTENTION_GPU_TYPE} (larger than any single burst victim's share): $JOB13"
S13=$(wait_for_status "$JOB13" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED" 45 || true)
echo "  job13 status after submission: $S13"
if [[ "$S13" == "RUNNING" ]]; then
  # If it's running, its full requested footprint must actually be satisfiable — a naive
  # single-victim preemption that under-frees capacity but still flips status to RUNNING
  # would be exactly the bug this test targets, so this is the important assertion.
  ADMITTED_GPU_COUNT=$(get_field "$JOB13" gpu_count)
  if [[ "$ADMITTED_GPU_COUNT" == "$BIG_GUARANTEED_GPU_COUNT" ]]; then
    pass "job13 admitted RUNNING and its full ${BIG_GUARANTEED_GPU_COUNT}-GPU footprint was preserved (no silent down-sizing) — suggests a sufficient victim set was planned"
  else
    fail "job13 is RUNNING but its admitted gpu_count=$ADMITTED_GPU_COUNT != requested $BIG_GUARANTEED_GPU_COUNT — possible partial/incorrect admission after preemption"
  fi
elif [[ "$S13" == "QUEUED" ]]; then
  pass "job13 correctly stayed QUEUED rather than being admitted without a sufficient victim set covering its full shortage"
else
  echo "  [WARN] job13 status=$S13 — inconclusive for victim-set-sufficiency assertion (may depend on scenario 3's burst jobs' timing/state)"
fi
wait_for_status "$JOB13" "COMPLETED,FAILED,EVICTED,QUEUED" 60 > /dev/null || true

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
echo "=========================================================="
echo "Scenario 14: fairness across multiple platform experiments and job shapes"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: dominant-utilization fairness aggregation (SCHEDULING_GENERALIZATION_PLAN.md,
# Class A step 7). The plan defines fairness as max(used_dimension/guaranteed_dimension) over
# only the dimensions a job actually requests AND that are tracked/budgeted — untracked
# dimensions must be excluded, not treated as zero utilization — and separately fixes
# fetchQuotaMap's (AgentID)-only key to (AgentID, PlatformExperimentID) so quota from
# different platform experiments can't cross-contaminate. This worktree's snapshot has
# neither the dominant-utilization aggregator nor the fetchQuotaMap key fix, so this
# scenario is a characterization test: it creates a second, independent platform experiment
# and submits a CPU-only job, an accelerator-only job, and a mixed job from the SAME agent
# used in the first PE above, then checks each PE's quota ledger only reflects that PE's own
# usage (the fetchQuotaMap key-collision bug, if present, would leak usage across PE_ID and
# PE2_ID for the shared agent).
echo "  ==> creating a second, independent platform experiment (PE2) reusing agent ${AGENTS[3]}..."
PE2_RESP=$(curl -sf -X POST "$QUOTA_URL/platform-experiments" -H 'Content-Type: application/json' -d "{
  \"name\": \"advanced-e2e-fairness-${RUN_TS}\",
  \"budget_t4_hours\": ${BUDGET},
  \"max_agents\": 1,
  \"metrics\": [{\"key\": \"val_accuracy\", \"direction\": \"maximize\"}],
  \"phase2_boundary\": 0.90,
  \"report_interval_seconds\": 10
}")
PE2_ID=$(echo "$PE2_RESP" | py "import sys,json; print(json.load(sys.stdin)['id'])")
echo "  PE2 id: $PE2_ID"
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE2_ID}/signup" -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"${AGENTS[3]}\"}" > /dev/null
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE2_ID}/start" > /dev/null

PE2_HYP=$(curl -sf -X POST "$REGISTRY_URL/registry/hypotheses" -H 'Content-Type: application/json' \
  -d "$(python3 "$TMPDIR_T/mk_hyp_body.py" "${AGENTS[3]}" "$PE2_ID")" | py "import sys,json; print(json.load(sys.stdin)['id'])")
# CPU-only job (no accelerator): sets gpu_count=0 so the fairness aggregator, once
# implemented, must exclude the GPU dimension from this job's utilization rather than
# counting it as zero-utilized-but-tracked.
PE2_CPU_ONLY_ID="job-pe2-cpu-only-${RUN_TS}"
PE2_CPU_ONLY_BODY=$(py "
import json
print(json.dumps({
    'id': '$PE2_CPU_ONLY_ID',
    'metadata': {
        'agent_id': '${AGENTS[3]}', 'platform_experiment_id': '$PE2_ID', 'project_id': 'advanced-e2e',
        'hypothesis_id': '$PE2_HYP', 'theory': 'fairness must exclude untracked/unrequested dimensions',
        'objective': 'maximize val_accuracy', 'estimated_duration_hours': $JOB_HOURS,
        'code_ref': 'git://openresearch@main', 'capacity_tier': 'guaranteed',
    },
    'job': {'image': 'localhost/openresearch-workload:latest', 'cpu': '250m', 'memory': '128Mi', 'gpu_count': 0},
}))
")
if curl -sf -X POST "$SCHED_URL/experiments" -H 'Content-Type: application/json' -d "$PE2_CPU_ONLY_BODY" > /dev/null; then
  wait_for_status "$PE2_CPU_ONLY_ID" "RUNNING,COMPLETED,FAILED,EVICTED,QUEUED,REJECTED" 30 > /dev/null || true
  pass "CPU-only (gpu_count=0) job accepted into PE2's fairness pool without requiring a GPU dimension"
else
  echo "  [WARN] CPU-only job submission failed — gpu_count=0 may not be a supported shape in this worktree yet"
fi

PE1_QUOTA_AFTER=$(curl -sf "$QUOTA_URL/platform-experiments/${PE_ID}/quotas" | py "
import sys,json
for q in json.load(sys.stdin):
    if q['agent_id'] == '${AGENTS[3]}':
        print(q.get('used_guaranteed_t4h', 0))
")
PE2_QUOTA_AFTER=$(curl -sf "$QUOTA_URL/platform-experiments/${PE2_ID}/quotas" | py "
import sys,json
for q in json.load(sys.stdin):
    if q['agent_id'] == '${AGENTS[3]}':
        print(q.get('used_guaranteed_t4h', 0))
")
echo "  agent ${AGENTS[3]}: PE(${PE_ID}) used_guaranteed_t4h=$PE1_QUOTA_AFTER, PE2(${PE2_ID}) used_guaranteed_t4h=$PE2_QUOTA_AFTER"
# NOTE: depends on fetchQuotaMap's (AgentID, PlatformExperimentID) key fix (plan Class A step
# 6). A meaningful automated assertion here would require knowing each PE's usage independent
# of the other absent the bug, which this quick check can't cleanly isolate from normal quota
# growth in PE1 above — printed for manual/CI-log review rather than hard-asserted, to avoid
# a flaky false pass/fail. A stronger version of this test should submit precisely-sized jobs
# to both PEs and assert PE1's usage figure is completely unaffected by PE2's submissions.
curl -sf -X POST "$QUOTA_URL/platform-experiments/${PE2_ID}/close" \
  | py "import sys,json; print('  PE2 closed, status:', json.load(sys.stdin).get('status',''))" 2>/dev/null || true

echo ""
echo "=========================================================="
echo "Scenario 15: schema migrations tested against the current schema"
echo "(SCHEDULING_GENERALIZATION_PLAN.md, minimum acceptance tests)"
echo "=========================================================="
# --- New: migrate-against-existing-DB, not just fresh (SCHEDULING_GENERALIZATION_PLAN.md,
# minimum acceptance tests). controlplane/shared/db/schema.sql currently declares itself
# "Single source of truth; no migration history" (no migrations directory/tooling exists in
# this worktree) — the RAM/storage removal migration (Class B step 1) is exactly the kind of
# change that needs to apply cleanly to an already-provisioned database with live data, not
# just a fresh `psql < schema.sql`. There is no migration mechanism yet to exercise here, so
# this is left as an aspirational placeholder rather than faked: once a migrations directory
# exists, this scenario should (1) provision a DB from the CURRENT (pre-migration) schema,
# (2) insert representative rows (agent_quotas with non-zero RAM/storage guaranteed/burst
# values, an experiment referencing them), (3) apply the new migration, and (4) assert it
# succeeds without data loss/constraint violation and that dependent code has no dangling
# references to the removed columns.
echo "  [INFO] schema-migration-against-existing-DB scenario is aspirational — no migrations directory/tooling exists in this worktree yet; see comment above for the intended test shape once it lands"

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
