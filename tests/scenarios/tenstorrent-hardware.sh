#!/usr/bin/env bash
# HARDWARE-ONLY: requires real Tenstorrent Blackhole (tt-quietbox's `make tt-up`). Excluded from
# a default tests/run.sh — included only with RUN_HARDWARE_TESTS=1. Proves the platform's own
# submission path reaches real hardware: submit a job, verify a genuine DRA ResourceClaim gets
# allocated a real /dev/tenstorrent device, not just that the platform reports success.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${DIR}/../.." && pwd)"
TT_CONTEXT="${TT_CONTEXT:-k3s-tt}"
export JOB_FILE="${REPO_ROOT}/tests/workloads/tenstorrent/job.yaml"
source "${DIR}/../lib/common.sh"
source "${DIR}/../lib/api.sh"

[[ "$(uname -s)" == "Linux" ]] || { echo "ERROR: this scenario only runs on the Linux tt-quietbox host" >&2; exit 2; }
kubectl --context "${TT_CONTEXT}" get node tt-quietbox >/dev/null 2>&1 \
  || { echo "ERROR: context ${TT_CONTEXT} is not the tt-quietbox cluster" >&2; exit 2; }
kubectl --context "${TT_CONTEXT}" get deviceclass tenstorrent.com >/dev/null 2>&1 \
  || { echo "ERROR: context ${TT_CONTEXT} has no Tenstorrent DeviceClass" >&2; exit 2; }
TT_RESOURCE_SLICES=$(kubectl --context "${TT_CONTEXT}" get resourceslices -o json \
	| py "import sys,json; print(sum(len(x.get('spec',{}).get('devices',[])) for x in json.load(sys.stdin)['items'] if x.get('spec',{}).get('driver') == 'tenstorrent.com'))")
[[ "$TT_RESOURCE_SLICES" -gt 0 ]] \
	|| { echo "ERROR: tenstorrent.com publishes no actual devices in ResourceSlices" >&2; exit 2; }

# Attach tt-quietbox just for this job, detach on exit. Combined into one trap since common.sh
# already installs its own EXIT trap for $TMPDIR_T cleanup (trap only keeps the latest handler).
source "${REPO_ROOT}/localdev/lib/node.sh"
lib_attach_node "${TT_CONTEXT}" tt-quietbox
trap 'rm -rf "$TMPDIR_T"; lib_detach_node "${TT_CONTEXT}" tt-quietbox' EXIT

echo "==> Registering agent and a Tenstorrent-budgeted platform experiment"
AGENT="tt-e2e-agent-${RUN_ID}"
register_agent "$AGENT"
# Declare the metrics tests/workloads/tenstorrent/train.py actually pushes — raw measured
# TFLOPS, not the generic val_accuracy default. tflops_measured is a running max, so it is
# unbounded (no clamp to saturate the ranking) and monotonic (cannot trip metric_decline).
PE_ID=$(create_platform_experiment "tt-e2e-${RUN_ID}" 1.0 1 5 0 \
  '[{"key": "tflops_measured", "direction": "maximize"}, {"key": "latency_ms", "direction": "minimize"}]')
signup_and_start "$PE_ID" "$AGENT"

echo "==> Submitting job (accelerator_type=tenstorrent.com/chipArch=blackhole, from ${JOB_FILE})"
JOB_ID=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "tenstorrent.com/chipArch=blackhole" "1")
echo "  job: $JOB_ID"

echo "==> Waiting for admission onto a real cluster..."
CLUSTER_SEEN=""
for i in $(seq 1 20); do
  CLUSTER_SEEN=$(get_field "$JOB_ID" cluster_name)
  [[ -n "$CLUSTER_SEEN" ]] && break
  sleep 2
done
[[ "$CLUSTER_SEEN" == "tt-quietbox" ]] \
  && pass "job admitted onto cluster_name=tt-quietbox (real hardware, not a fake/local node)" \
  || fail "job landed on cluster_name='${CLUSTER_SEEN:-<none>}', expected tt-quietbox"

echo "==> Independently verifying real DRA device allocation on ${TT_CONTEXT} while the pod runs..."
# The ResourceClaim is owned by the pod (ownerReference, delete-protection finalizer) and is
# garbage-collected the moment the pod terminates — so this has to happen *before* waiting out
# a terminal job status, not after. BuildJob names the k8s Job/pod after the experiment ID (see
# workload.jobName) and labels the pod with the experiment ID, same selector CreateWorkload's
# own headless-service/label scheme uses elsewhere in this codebase.
POD_NAME=""
CLAIM_STATUS=""
for i in $(seq 1 20); do
  POD_JSON=$(kubectl --context "${TT_CONTEXT}" -n hypothesisloop-jobs get pods \
    -l "hypothesisloop.io/experiment-id=${JOB_ID}" -o json)
  POD_NAME=$(echo "$POD_JSON" | py "import sys,json; items=json.load(sys.stdin)['items']; print(items[0]['metadata']['name'] if items else '')")
  if [[ -n "$POD_NAME" ]]; then
    # status.allocation.devices.results — resource.k8s.io/v1's DRA scheduler plugin populates
    # this once it has bound the claim to real device(s); status.devices (no "allocation")
    # doesn't exist in this API version, only in an earlier draft this test used to check.
    CLAIM_STATUS=$(kubectl --context "${TT_CONTEXT}" -n hypothesisloop-jobs get resourceclaim \
      -l "hypothesisloop.io/experiment-id=${JOB_ID}" -o json \
      | py "import sys,json; items=json.load(sys.stdin)['items']; d=(items[0].get('status',{}).get('allocation') or {}).get('devices',{}).get('results',[]) if items else []; print(d)")
    [[ -n "$CLAIM_STATUS" && "$CLAIM_STATUS" != "[]" ]] && break
  fi
  sleep 2
done
if [[ -n "$POD_NAME" ]]; then
  pass "found real pod ${POD_NAME} on ${TT_CONTEXT}"
else
  fail "no pod found on ${TT_CONTEXT} for experiment ${JOB_ID} — DRA/scheduling path likely did not run"
fi
echo "  resourceclaim status.allocation.devices.results: ${CLAIM_STATUS}"
[[ "$CLAIM_STATUS" == *"tenstorrent.com"* ]] \
  && pass "ResourceClaim shows a real tenstorrent.com device allocated (not a fake/simulated one)" \
  || fail "ResourceClaim did not show a tenstorrent.com device allocation"

echo "==> Waiting for terminal status..."
# cluster_name assignment above isn't RUNNING, so use the two-phase wait; admission_budget kept
# modest since the DRA polling above already spent real wall-clock time past submission.
STATUS=$(wait_for_completion_after_running "$JOB_ID" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
echo "  status: $STATUS"
EVICTION_REASON=$(get_field "$JOB_ID" eviction_reason)
if [[ "$STATUS" == "COMPLETED" ]]; then
  pass "job reached COMPLETED"
else
  fail "job did not reach COMPLETED (status=$STATUS, eviction_reason=${EVICTION_REASON:-n/a}) — see: kubectl --context ${TT_CONTEXT} -n hypothesisloop-jobs get pods,resourceclaims"
fi

M=$(dashboard_metrics "$JOB_ID")
COUNT=$(echo "$M" | py "import sys,json; d=json.load(sys.stdin); assert isinstance(d,list); print(len(d))")
[[ "$COUNT" -ge 1 ]] \
  && pass "$COUNT metric point(s) pushed from the real pod via registry-service" \
  || fail "0 metric points recorded — pod may not have actually run/reported"

# file_finding here (not just at the bottom) because it gates further submissions from the same
# agent+PE_ID: the bare-node-agent job below reuses both, and would get 403 summary_required
# otherwise.
file_finding "$JOB_ID" "Tenstorrent e2e: real DRA allocation on tt-quietbox confirmed."

echo "==> Submitting the same job straight to this host's bare-node agent (no k3s/DRA involved) --"
# Proves runtime/bare-metal/internal/podexec's own Tenstorrent probe (tenstorrent.go) finds and
# uses the same real hardware this scenario just proved k3s's DRA path can reach — same
# accelerator_type string, same physical cards, the other supported runtime.
BARE_CLUSTER="tt-bare-e2e-${RUN_ID}"
BARE_SCRATCH="$(mktemp -d)"
BARE_AGENT_BIN="$TMPDIR_T/bare-agent"
BARE_AGENT_LOG="$TMPDIR_T/bare-agent.log"
( cd "$REPO_ROOT" && go build -o "$BARE_AGENT_BIN" ./runtime/bare-metal/cmd/bare-agent )

CLUSTER_NAME="$BARE_CLUSTER" \
CONTROLPLANE_URL="$SCHED_URL" \
REGISTRY_URL="$REGISTRY_URL" \
HYPOTHESISLOOP_CONFIG="${REPO_ROOT}/controlplane/settings/hypothesisloop.yaml" \
SCRATCH_DIR="$BARE_SCRATCH" \
NODE_NAME="tt-bare-e2e-node-${RUN_ID}" \
"$BARE_AGENT_BIN" > "$BARE_AGENT_LOG" 2>&1 &
BARE_AGENT_PID=$!
trap 'kill "$BARE_AGENT_PID" 2>/dev/null || true; wait "$BARE_AGENT_PID" 2>/dev/null || true; rm -rf "$BARE_SCRATCH" "$TMPDIR_T"; lib_detach_node "${TT_CONTEXT}" tt-quietbox' EXIT

wait_until "bare-agent registered cluster $BARE_CLUSTER" 15 1 \
  bash -c "curl -sf '$SCHED_URL/internal/clusters' | grep -q '\"$BARE_CLUSTER\"'" \
  || fail "bare-agent never showed up as a reachable cluster (see $BARE_AGENT_LOG)"

# The bare-agent talks to whichever container engine it resolved to (see runtime/bare-metal/
# internal/podexec/util.go's resolveEngineClient, logged at startup) — not necessarily the same
# one `make tt-hardware-image` built the workload image into (that target always uses podman).
# Build it directly into the resolved engine's own local store so this step is self-contained
# and idempotent regardless of which engine is present on this host.
BARE_ENGINE="$(grep -oE 'engine=(podman|docker)' "$BARE_AGENT_LOG" | head -1 | cut -d= -f2)"
"$BARE_ENGINE" build -f "${REPO_ROOT}/tests/workloads/tenstorrent/Dockerfile.train" \
  -t localhost/hypothesisloop-tenstorrent-workload "${REPO_ROOT}/tests/workloads/tenstorrent/" > /dev/null

# Same job.yaml, byte-for-byte, as the k3s submission above: accelerator_pod_resources
# (hugepages) has no bare-metal equivalent, so podexec.BuildContainerSpec logs a warning and
# ignores it rather than rejecting the job — the control plane picks the runtime, not the spec.
BARE_JOB_ID=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "0.02" "tenstorrent.com/chipArch=blackhole" "1" "" "$JOB_FILE")
curl -s -o /dev/null -X POST "$SCHED_URL/experiments/${BARE_JOB_ID}/admit" \
  -H 'Content-Type: application/json' -d "{\"cluster_name\": \"$BARE_CLUSTER\"}"
BARE_STATUS=$(wait_for_completion_after_running "$BARE_JOB_ID" "0.02" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$(get_field "$BARE_JOB_ID" cluster_name)" == "$BARE_CLUSTER" ]] \
  && pass "bare-node job admitted onto this host's bare-agent ($BARE_CLUSTER), not k3s" \
  || fail "bare-node job landed on cluster_name='$(get_field "$BARE_JOB_ID" cluster_name)', expected $BARE_CLUSTER"
[[ "$BARE_STATUS" == "COMPLETED" ]] \
  && pass "bare-node job reached COMPLETED using the real Tenstorrent device directly (no k3s/DRA)" \
  || fail "bare-node job did not reach COMPLETED (status=$BARE_STATUS, eviction_reason=$(get_field "$BARE_JOB_ID" eviction_reason)) — see $BARE_AGENT_LOG"
BARE_M=$(dashboard_metrics "$BARE_JOB_ID")
BARE_COUNT=$(echo "$BARE_M" | py "import sys,json; d=json.load(sys.stdin); assert isinstance(d,list); print(len(d))")
[[ "$BARE_COUNT" -ge 1 ]] \
  && pass "$BARE_COUNT metric point(s) pushed from the real bare-node container via registry-service" \
  || fail "0 metric points recorded from the bare-node job — container may not have actually run/reported"
[[ "$BARE_STATUS" == "COMPLETED" ]] && file_finding "$BARE_JOB_ID" "Tenstorrent e2e: real device access via bare-node agent confirmed."

close_platform_experiment "$PE_ID"
finish
