#!/usr/bin/env bash
# End-to-end proof that the bare-node agent (runtime/bare-metal/cmd/bare-agent — see
# different-backend.md) genuinely collaborates with the control plane: it registers as a real
# cluster, runs a submitted job as a real container on this host (not just that the control
# plane's own bookkeeping says RUNNING), and a cancel actually stops a running one.
#
# Unlike every other scenario here (which submit against an already-running k3s/tenstorrent
# cluster-agent), this scenario starts its own bare-agent process pointed at the local control
# plane (assumed already up via `make controlplane-up`, same assumption every scenario makes —
# see tests/run.sh) and a unique CLUSTER_NAME, so it cannot collide with any other cluster
# registered against this control plane. Requires a real podman or docker binary on this host
# (same requirement as runtime/bare-metal/internal/podexec's own integration test).
#
# CLUSTER_EXCLUSIVE-worthy in spirit (it's the only scenario touching the host's container
# engine directly with a throwaway cluster name), but since its cluster name is unique per RUN_ID
# and it touches no shared k8s/daemonset state, it's safe to run concurrently with the rest —
# left out of tests/lib/cluster.sh's CLUSTER_EXCLUSIVE list on purpose.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$DIR/../.." && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
  echo "  [SKIP] neither podman nor docker found on this host — bare-node agent needs a container engine"
  finish
  exit 0
fi

CLUSTER_NAME="bare-e2e-${RUN_ID}"
SCRATCH_DIR="$(mktemp -d)"
AGENT_BIN="$TMPDIR_T/bare-agent"
AGENT_LOG="$TMPDIR_T/bare-agent.log"
JOB=""

( cd "$ROOT" && go build -o "$AGENT_BIN" ./runtime/bare-metal/cmd/bare-agent )

CLUSTER_NAME="$CLUSTER_NAME" \
API_URL="$API_URL" \
HYPOTHESISLOOP_CONFIG="$ROOT/controlplane/settings/hypothesisloop.yaml" \
SCRATCH_DIR="$SCRATCH_DIR" \
NODE_NAME="bare-e2e-node-${RUN_ID}" \
"$AGENT_BIN" > "$AGENT_LOG" 2>&1 &
AGENT_PID=$!

# A job left RUNNING forever (e.g. this script killed early, or a real assertion failure) would
# outlive its bare-agent process — its cluster never reports again, so every future poll of it
# errors, and JobWatcher.scanAndWatch (controlplane/services/scheduler/job_watcher_scan.go)
# aborts its *entire* pass on the first error, starving every other experiment's status updates
# too. Cancelling unconditionally on exit is this scenario's duty not to poison the shared
# control plane for whatever else is using it (including this scenario's own next run).
cleanup() {
  for j in "$JOB" "${JOB2:-}"; do
    [[ -n "$j" ]] && curl -s -o /dev/null -X POST "$API_URL/experiments/${j}/cancel" || true
  done
  kill "$AGENT_PID" 2>/dev/null || true
  wait "$AGENT_PID" 2>/dev/null || true
  if [[ -n "$ENGINE" ]]; then
    "$ENGINE" ps -aq --filter "label=hypothesisloop.io/managed-by=hypothesisloop" \
      --filter "label=hypothesisloop.io/agent-id=${AGENT}" 2>/dev/null \
      | xargs -r "$ENGINE" rm -f >/dev/null 2>&1 || true
  fi
  rm -rf "$SCRATCH_DIR"
}
trap cleanup EXIT

echo "  -- bare-agent process started (pid=$AGENT_PID, cluster=$CLUSTER_NAME), waiting for its first reconcile --"
wait_until "bare-agent registered cluster $CLUSTER_NAME" 15 1 \
  bash -c "curl -sf '$API_URL/internal/clusters' | grep -q '\"$CLUSTER_NAME\"'" \
  || fail "bare-agent never showed up as a reachable cluster (see $AGENT_LOG)"

# Ask the agent itself which container engine it resolved to (runtime/bare-metal/internal/
# podexec/util.go's resolveEngineClient logs this at startup) rather than re-guessing with the
# same podman-then-docker preference order a bash `command -v` check would use — the two can
# legitimately disagree (e.g. podman installed but its API socket not enabled), and re-guessing
# would just reproduce that mismatch here.
ENGINE="$(grep -oE 'engine=(podman|docker)' "$AGENT_LOG" | head -1 | cut -d= -f2)"
[[ -n "$ENGINE" ]] || fail "could not determine which container engine the bare-agent resolved to (see $AGENT_LOG)"
echo "  -- bare-agent resolved container engine: $ENGINE --"

AGENT="agent-bare-e2e-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "bare-e2e-${RUN_ID}" 0.001 1 10 0.08)
signup_and_start "$PE_ID" "$AGENT"

# job_container_name JOB_ID -> prints the real container name backing JOB_ID, on $ENGINE.
job_container_name() { "$ENGINE" ps -a --filter "label=hypothesisloop.io/experiment-id=${1}" --format '{{.Names}}' | head -1; }
container_exists() { [[ -n "$(job_container_name "$1")" ]]; }
container_absent()  { [[ -z "$(job_container_name "$1")" ]]; }

# job_override SLEEP_SECONDS -> JSON body for a CPU-only busybox job that runs SLEEP_SECONDS,
# with every k8s-only field the bare executor rejects (podexec.BuildContainerSpec —
# accelerator_tolerations/extra_resources/accelerator_pod_resources) explicitly nulled out per
# different-backend.md's coupling-2 table, rather than relying on job.yaml's defaults.
job_override() {
  local sleep_s="$1"
  echo '{
    "image": "busybox", "command": ["sh", "-c"],
    "args": ["echo hello-from-bare-node-agent && sleep '"$sleep_s"'"],
    "cpu": "0.2", "memory": "64Mi", "storage": "128Mi", "max_retries": 0,
    "accelerator_count": 0, "accelerator_type": null, "acceptable_accelerator_types": null,
    "accelerator_tolerations": null, "extra_resources": null, "accelerator_pod_resources": null
  }'
}

# force_admit JOB_ID -> forces admission onto our own cluster (POST /experiments/{id}/admit —
# still goes through the same capacity-claiming transition normal admission uses, see
# scheduler/handler.go). This control plane may already have other clusters connected that
# could just as validly win ordinary capacity-based admission for a footprint this trivial —
# that would prove nothing about *this* bare-agent, so force it rather than leaving cluster
# choice to chance/racing the automatic admission loop's own next tick.
force_admit() {
  curl -s -o /dev/null -X POST "$API_URL/experiments/${1}/admit" \
    -H 'Content-Type: application/json' -d "{\"cluster_name\": \"$CLUSTER_NAME\"}"
}

echo "  ==> run a job to completion, verify a real container backed it --"
JOB=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "0.01" "$JOB_FILE" "" "" "" "" "" "" "" "" "$(job_override 2)")
force_admit "$JOB"
S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED,REJECTED" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" || "$S" == "COMPLETED" ]] \
  && pass "job admitted and started on the bare-node cluster (status=$S)" \
  || fail "job never reached RUNNING/COMPLETED on the bare-node cluster, got $S"
[[ "$(get_field "$JOB" cluster_name)" == "$CLUSTER_NAME" ]] \
  && pass "job's admitted cluster_name is this bare-agent's cluster ($CLUSTER_NAME)" \
  || fail "job landed on cluster_name='$(get_field "$JOB" cluster_name)', expected $CLUSTER_NAME"
wait_until "a real $ENGINE container for $JOB exists on this host" 20 1 container_exists "$JOB" \
  && pass "a real container backing $JOB exists on this host, launched directly by the bare-agent" \
  || fail "no local $ENGINE container found for $JOB — bare-agent never actually launched it"

# The bare-agent's own FetchLogTail (runtime/bare-metal/internal/podexec) reads this same
# container's stdout live and reports it alongside job phase on its next status push (see
# runtime/shared/agentloop.reportChangedStatuses + controlplane/services/clusteragentapi) --
# the control plane never reaches into the container engine itself. Confirms the whole chain:
# real container -> bare-agent -> clusteragentapi -> GreptimeDB -> registry GET, no job-side
# involvement at all (the job here is a bare `sh -c echo ...`, nothing pushes its own logs).
job_log_tail_has_hello() {
  curl -sf "$API_URL/experiments/${1}/logs" | grep -q "hello-from-bare-node-agent"
}
wait_until "registry's log-tail endpoint reports this container's own stdout" 15 1 job_log_tail_has_hello "$JOB" \
  && pass "GET /experiments/{id}/logs surfaces the real container's stdout, reported by the bare-agent itself" \
  || fail "registry never reported the container's stdout via the log-tail endpoint"

S=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" 30 || true)
[[ "$S" == "COMPLETED" ]] \
  && pass "job completed end-to-end via the bare-node agent (real container ran and exited 0)" \
  || fail "expected job to COMPLETE, got $S"
CONTAINER_LOGS="$("$ENGINE" logs "$(job_container_name "$JOB")" 2>&1 || true)"
[[ "$CONTAINER_LOGS" == *"hello-from-bare-node-agent"* ]] \
  && pass "container's own stdout confirms the job body actually ran" \
  || fail "container logs missing expected output: $CONTAINER_LOGS"
file_finding "$JOB"

echo "  ==> cancel a running job, verify it actually stops --"
JOB2=$(submit_job_ext "$PE_ID" "$AGENT" "guaranteed" "0.01" "$JOB_FILE" "" "" "" "" "" "" "" "" "$(job_override 60)")
force_admit "$JOB2"
S=$(wait_for_status "$JOB2" "RUNNING,COMPLETED,FAILED,EVICTED,REJECTED" "$ADMISSION_BUDGET_SECONDS" || true)
[[ "$S" == "RUNNING" ]] || fail "second job must be RUNNING before cancel, got $S"
wait_until "a real $ENGINE container for $JOB2 exists on this host" 20 1 container_exists "$JOB2" \
  || fail "no local $ENGINE container found for $JOB2 before cancel"
cancel_job "$JOB2"
S=$(wait_for_status "$JOB2" "EVICTED,COMPLETED,FAILED" 30 || true)
[[ "$S" == "EVICTED" ]] \
  && pass "cancelling a RUNNING job on the bare-node agent terminates it as EVICTED" \
  || fail "expected EVICTED after cancelling a RUNNING job, got $S"
wait_until "the $ENGINE container for $JOB2 is removed after cancel" 15 1 container_absent "$JOB2" \
  && pass "the bare-agent actually stopped/removed the real container on cancel" \
  || fail "container for $JOB2 still present on this host after cancel — bare-agent did not really stop it"

close_platform_experiment "$PE_ID"
finish
