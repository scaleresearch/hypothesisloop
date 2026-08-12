#!/usr/bin/env bash
# HARDWARE-ONLY: requires a real NVIDIA GPU, nvidia-container-toolkit, and podman/docker on
# this host. Excluded from a default tests/run.sh — included only with RUN_HARDWARE_TESTS=1.
#
# Bare-node leg (always runs): admission, real GPU compute, metrics, a negative-admission case,
# and mid-run metrics + eviction + post-eviction reuse — all via runtime/bare-metal/cmd/bare-agent.
# k3s leg (optional): same hardware via cluster-agent's device-plugin path — see
# localdev/k3s-nvidia/install.sh, which registers cluster_name "k3s-nvidia". Skipped if that
# context isn't set up on this host.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DIR/../.." && pwd)"
export JOB_FILE="${ROOT}/tests/workloads/nvidia/job.yaml"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

command -v nvidia-smi >/dev/null 2>&1 || { echo "  [SKIP] no nvidia-smi on this host"; finish; exit 0; }
if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
  echo "  [SKIP] neither podman nor docker found on this host"; finish; exit 0
fi
ENGINE="$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)"
"$ENGINE" run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L >/dev/null 2>&1 \
  || { echo "  [SKIP] $ENGINE cannot pass a GPU through (nvidia-container-toolkit not set up?)"; finish; exit 0; }

# nvidia.com/gpu.product is exact-model (see job.yaml's comment) — read what this host's own
# GPU actually reports and normalize identically to podexec/nvidia.go's normalizeGFDProductName,
# so accelerator_type here always matches what the bare-agent itself will advertise.
GPU_NAME_RAW="$(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)"
ACC_TYPE="nvidia.com/gpu.product=$(printf '%s' "$GPU_NAME_RAW" | tr '[:lower:]' '[:upper:]' | tr -s '[:space:]' '-')"
echo "  -- detected GPU: $GPU_NAME_RAW -> accelerator_type=$ACC_TYPE --"
grep -qF "$ACC_TYPE" "${ROOT}/controlplane/settings/hypothesisloop.yaml" \
  || { echo "  [SKIP] $ACC_TYPE has no acch_rate in controlplane/settings/hypothesisloop.yaml — add one to admit jobs of this type"; finish; exit 0; }

# "bare-" prefix is deliberate: when the k3s leg's cluster ("k3s-nvidia") is also connected,
# clusterWithBestFit's stable sort-by-name tiebreak picks whichever cluster name sorts first
# among equally-good candidates (same pattern tenstorrent-hardware.sh's "tt-bare-e2e-*" name
# relies on against "tt-quietbox") — "bare-" < "k3s-", so this cluster always wins ties for its
# own leg's jobs instead of racing the automatic admission loop for a specific cluster.
CLUSTER_NAME="bare-nvidia-e2e-${RUN_ID}"
SCRATCH_DIR="$(mktemp -d)"
AGENT_BIN="$TMPDIR_T/bare-agent"
AGENT_LOG="$TMPDIR_T/bare-agent.log"
JOB="" JOB2="" JOB3="" JOB4="" JOB5=""

( cd "$ROOT" && go build -o "$AGENT_BIN" ./runtime/bare-metal/cmd/bare-agent )

CLUSTER_NAME="$CLUSTER_NAME" \
CONTROLPLANE_URL="$SCHED_URL" \
REGISTRY_URL="$REGISTRY_URL" \
HYPOTHESISLOOP_CONFIG="$ROOT/controlplane/settings/hypothesisloop.yaml" \
SCRATCH_DIR="$SCRATCH_DIR" \
NODE_NAME="nvidia-e2e-node-${RUN_ID}" \
"$AGENT_BIN" > "$AGENT_LOG" 2>&1 &
AGENT_PID=$!

# Same reasoning as bare-node-agent.sh's cleanup: an experiment left QUEUED/RUNNING would
# outlive this run and poison future polls of it for the shared control plane. Cancel every job
# this scenario ever submits, not just the first one, however far the script got before exiting.
cleanup() {
  for j in "$JOB" "$JOB2" "$JOB3" "$JOB4" "$JOB5"; do
    [[ -n "$j" ]] && curl -s -o /dev/null -X POST "$SCHED_URL/experiments/${j}/cancel"
  done
  kill "$AGENT_PID" 2>/dev/null || true
  wait "$AGENT_PID" 2>/dev/null || true
  "$ENGINE" ps -aq --filter "label=hypothesisloop.io/agent-id=${AGENT:-}" 2>/dev/null | xargs -r "$ENGINE" rm -f >/dev/null 2>&1 || true
  rm -rf "$SCRATCH_DIR"
}
trap cleanup EXIT

# force_admit POSTs /admit like bare-node-agent.sh's does, plus a short retry: a cluster that
# just registered can take a couple seconds before its capacity is visible to admission,
# briefly 422ing "insufficient capacity" (see runtime/bare-metal/cmd/bare-agent's 3s status
# report interval).
force_admit() {
  local job="$1" cluster="$2"
  for i in 1 2 3 4 5; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$SCHED_URL/experiments/${job}/admit" \
      -H 'Content-Type: application/json' -d "{\"cluster_name\": \"$cluster\"}")
    [[ "$code" == "200" ]] && return 0
    sleep 2
  done
  echo "  [WARN] force_admit($job, $cluster) never returned 200 (last http_code=$code)" >&2
  return 1
}

echo "  -- bare-agent started (pid=$AGENT_PID, cluster=$CLUSTER_NAME), waiting for registration --"
wait_until "bare-agent registered cluster $CLUSTER_NAME" 15 1 \
  bash -c "curl -sf '$SCHED_URL/internal/clusters' | grep -q '\"$CLUSTER_NAME\"'" \
  || fail "bare-agent never showed up as a reachable cluster (see $AGENT_LOG)"

echo "  -- building real NVIDIA workload image ($ENGINE) --"
"$ENGINE" build -f "${ROOT}/tests/workloads/nvidia/Dockerfile.train" \
  -t localhost/hypothesisloop-nvidia-workload "${ROOT}/tests/workloads/nvidia/" > /dev/null

AGENT="agent-nvidia-e2e-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "nvidia-e2e-${RUN_ID}" 1.0 1 5 0 \
  '[{"key": "tflops_measured", "direction": "maximize"}, {"key": "latency_ms", "direction": "minimize"}]')
signup_and_start "$PE_ID" "$AGENT"

echo "  ==> submitting real GPU job (accelerator_type=$ACC_TYPE) --"
JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "$ACC_TYPE" "1" "" "$JOB_FILE")
force_admit "$JOB" "$CLUSTER_NAME" || true

STATUS=$(wait_for_completion_after_running "$JOB" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$STATUS" == "COMPLETED" ]] \
  && pass "job reached COMPLETED on real GPU ($GPU_NAME_RAW)" \
  || fail "job did not reach COMPLETED (status=$STATUS, eviction_reason=$(get_field "$JOB" eviction_reason)) — see $AGENT_LOG"
[[ "$(get_field "$JOB" cluster_name)" == "$CLUSTER_NAME" ]] \
  && pass "job's admitted cluster_name is this bare-agent's cluster ($CLUSTER_NAME)" \
  || fail "job landed on cluster_name='$(get_field "$JOB" cluster_name)', expected $CLUSTER_NAME"

M=$(dashboard_metrics "$JOB")
COUNT=$(echo "$M" | py "import sys,json; d=json.load(sys.stdin); assert isinstance(d,list); print(len(d))")
[[ "$COUNT" -ge 1 ]] \
  && pass "$COUNT metric point(s) pushed from the real container via registry-service" \
  || fail "0 metric points recorded — container may not have actually run/reported"

[[ "$STATUS" == "COMPLETED" ]] && file_finding "$JOB" "NVIDIA e2e: real GPU matmuls via bare-node agent confirmed ($GPU_NAME_RAW)."

# ==> negative case: a job asking for hardware this cluster does not report must never be
# admitted onto it — reusing the same job.yaml, just with accelerator_type overridden to a
# model priced in settings but not backed by any device this host actually has.
echo "  ==> submitting a job for an accelerator type this cluster does NOT have --"
OTHER_TYPE="nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
[[ "$ACC_TYPE" == "$OTHER_TYPE" ]] && OTHER_TYPE="nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
JOB2=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "0.02" "$JOB_FILE" "" "" "" "" "" "" "" "" \
  '{"accelerator_type": "'"$OTHER_TYPE"'", "accelerator_count": 1}')
sleep 5
S2=$(get_status "$JOB2")
C2=$(get_field "$JOB2" cluster_name)
[[ ( "$S2" == "SUBMITTED" || "$S2" == "QUEUED" ) && -z "$C2" ]] \
  && pass "job requesting $OTHER_TYPE correctly stayed unadmitted (status=$S2) — scheduler did not misroute it onto $CLUSTER_NAME" \
  || fail "job requesting hardware this cluster lacks should stay SUBMITTED/QUEUED with no cluster, got status=$S2 cluster_name='$C2'"
curl -s -o /dev/null -X POST "$SCHED_URL/experiments/${JOB2}/cancel"

# ==> mid-run metrics + eviction + post-eviction reuse: a long-running job (train.py's repeat
# mode) that streams metrics while RUNNING, gets evicted mid-run, and is confirmed to actually
# stop its container — then a fresh job proves the node still admits work afterward.
echo "  ==> submitting a long-running job to test mid-run metrics + eviction --"
JOB3=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "0.05" "$JOB_FILE" \
  '{"HYPOTHESISLOOP_REPEAT_FOREVER": "1", "HYPOTHESISLOOP_REPEAT_SLEEP_SECONDS": "1"}' \
  "$ACC_TYPE" "1" "" "" "" "" "" "")
force_admit "$JOB3" "$CLUSTER_NAME" || true

# Nested `bash -c` strings run in a fresh subshell that never sourced common.sh/api.sh, so
# shell functions (py, get_status, ...) aren't available there — every check below inlines
# curl+python3 -c directly instead of calling them.
wait_until "JOB3 reaches RUNNING" 20 1 bash -c \
  "[[ \$(curl -sf '$SCHED_URL/experiments/${JOB3}' | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"status\"])') == RUNNING ]]" \
  || fail "JOB3 never reached RUNNING (status=$(get_status "$JOB3"))"

wait_until "JOB3 reports >=2 metric samples while RUNNING" 30 2 bash -c \
  "[[ \$(curl -sf '$REGISTRY_URL/registry/experiments/${JOB3}/metrics' | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))') -ge 2 ]]" \
  && pass "metrics streamed mid-run from a real GPU container" \
  || fail "fewer than 2 metric samples landed while JOB3 was RUNNING"

CONTAINER_NAME_3="$("$ENGINE" ps --filter "label=hypothesisloop.io/experiment-id=${JOB3}" --format '{{.Names}}' | head -1)"
[[ -n "$CONTAINER_NAME_3" ]] \
  && pass "a real $ENGINE container backs JOB3 on this host ($CONTAINER_NAME_3)" \
  || fail "no local $ENGINE container found for JOB3"

echo "  ==> evicting JOB3 mid-run --"
curl -s -o /dev/null -X POST "$SCHED_URL/experiments/${JOB3}/cancel"
wait_until "JOB3 reaches EVICTED" 15 1 bash -c \
  "[[ \$(curl -sf '$SCHED_URL/experiments/${JOB3}' | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"status\"])') == EVICTED ]]" \
  || fail "JOB3 never reached EVICTED (status=$(get_status "$JOB3"))"
wait_until "JOB3's container actually stops" 20 1 \
  bash -c "[[ -z \"\$($ENGINE ps --filter label=hypothesisloop.io/experiment-id=${JOB3} --filter status=running --format '{{.Names}}')\" ]]" \
  && pass "JOB3's container was actually torn down on eviction, not just marked EVICTED in bookkeeping" \
  || fail "JOB3's container is still running after eviction"

echo "  ==> submitting a fresh job to confirm the node is still usable after eviction --"
JOB4=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "$ACC_TYPE" "1" "" "$JOB_FILE")
force_admit "$JOB4" "$CLUSTER_NAME" || true
STATUS4=$(wait_for_completion_after_running "$JOB4" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$STATUS4" == "COMPLETED" ]] \
  && pass "node accepted and completed a new job right after evicting the previous one" \
  || fail "post-eviction job did not COMPLETE (status=$STATUS4)"
[[ "$STATUS4" == "COMPLETED" ]] && file_finding "$JOB4" "NVIDIA e2e: post-eviction reuse confirmed."

# ==> optional k3s/device-plugin leg: same real hardware, reached via cluster-agent's k8s
# executor instead of the bare-node one — see localdev/k3s-nvidia/install.sh, which sets up
# NVIDIA_K3S_CONTEXT="k3s-nvidia" pointed at cluster_name "k3s-nvidia". Skipped (not failed) if
# that context isn't configured on this host, same as tenstorrent-hardware.sh requires a
# specific context rather than assuming one.
NVIDIA_K3S_CONTEXT="${NVIDIA_K3S_CONTEXT:-k3s-nvidia}"
if command -v kubectl >/dev/null 2>&1 && kubectl --context "$NVIDIA_K3S_CONTEXT" get nodes >/dev/null 2>&1; then
  # Bare-node leg is done with it — stop it and wait for its heartbeat to actually go stale
  # (scheduler.cluster_unreachable_after_seconds), so its cluster (which sorts before
  # "k3s-nvidia", see CLUSTER_NAME above) can't also win this leg's admission tiebreak.
  kill "$AGENT_PID" 2>/dev/null || true
  wait "$AGENT_PID" 2>/dev/null || true
  wait_until "bare-node cluster $CLUSTER_NAME drops out of /internal/clusters" 40 1 \
    bash -c "! curl -sf '$SCHED_URL/internal/clusters' | grep -q '\"$CLUSTER_NAME\"'"
  echo "  ==> k3s leg: submitting real GPU job via cluster-agent (context=$NVIDIA_K3S_CONTEXT) --"
  K3S_CLUSTER_NAME="k3s-nvidia"
  K3S_GPU_PRODUCT="$(kubectl --context "$NVIDIA_K3S_CONTEXT" get nodes -o json \
    | py "import sys,json
for n in json.load(sys.stdin)['items']:
    v = n['metadata']['labels'].get('nvidia.com/gpu.product')
    if v: print(v); break")"
  if [[ -z "$K3S_GPU_PRODUCT" ]]; then
    echo "  [SKIP] k3s context $NVIDIA_K3S_CONTEXT has no nvidia.com/gpu.product node label"
  else
    K3S_ACC_TYPE="nvidia.com/gpu.product=$K3S_GPU_PRODUCT"
    JOB5=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "$K3S_ACC_TYPE" "1" "" "$JOB_FILE")
    force_admit "$JOB5" "$K3S_CLUSTER_NAME" || true
    STATUS5=$(wait_for_completion_after_running "$JOB5" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
    [[ "$STATUS5" == "COMPLETED" ]] \
      && pass "k3s leg: job reached COMPLETED via device-plugin GPU passthrough ($K3S_GPU_PRODUCT)" \
      || fail "k3s leg: job did not reach COMPLETED (status=$STATUS5, eviction_reason=$(get_field "$JOB5" eviction_reason))"
    [[ "$(get_field "$JOB5" cluster_name)" == "$K3S_CLUSTER_NAME" ]] \
      && pass "k3s leg: job's admitted cluster_name is $K3S_CLUSTER_NAME" \
      || fail "k3s leg: job landed on cluster_name='$(get_field "$JOB5" cluster_name)', expected $K3S_CLUSTER_NAME"
    M5=$(dashboard_metrics "$JOB5")
    COUNT5=$(echo "$M5" | py "import sys,json; d=json.load(sys.stdin); print(len(d))")
    [[ "$COUNT5" -ge 1 ]] \
      && pass "k3s leg: $COUNT5 metric point(s) pushed from the real pod" \
      || fail "k3s leg: 0 metric points recorded"
    [[ "$STATUS5" == "COMPLETED" ]] && file_finding "$JOB5" "NVIDIA e2e: real GPU matmuls via k3s/device-plugin confirmed ($K3S_GPU_PRODUCT)."
  fi
else
  echo "  [SKIP] k3s leg: no reachable kubectl context '$NVIDIA_K3S_CONTEXT' — run localdev/k3s-nvidia/install.sh to enable it"
fi

close_platform_experiment "$PE_ID"
finish
