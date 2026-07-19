#!/usr/bin/env bash
# Manual demo driver (not part of the automated suite). Keeps the robotics VLA platform
# experiment "alive" by having each agent repeatedly resubmit new rounds (new job, same
# hypothesis) as soon as its previous one finishes, for MAX_ROUNDS per agent — so the UI's
# "competing agents over time" chart shows a real race instead of one static point per agent.
#
# Meant to be started once and left running in the background:
#   nohup bash tests/robotics-compete-loop.sh > /tmp/robotics-compete-loop.log 2>&1 &
#
# Env overrides: MAX_ROUNDS (default 10), ROUND_DURATION_SECONDS (default 90),
#                REPORT_INTERVAL_SECONDS (default 5)
set -uo pipefail  # no -e: one agent's submission hiccup shouldn't kill the whole loop
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/lib/common.sh"
source "$DIR/lib/api.sh"
set +e  # common.sh sets -e; this script deliberately doesn't want it (see above)

ROBOTICS_JOB_FILE="${JOB_FILE:-${DIR}/workload-robotics/job.yaml}"

AGENTS=(agent-high-lr       agent-low-lr        agent-long-chunk    agent-multi-cam)
LRS=(1e-3                   3e-5                 3e-4                 3e-4)
CHUNKS=(16                  16                   50                   16)
CAMS=(1                     1                    1                    3)
HYPOTHESES=(
  "A higher learning rate (1e-3) converges faster to a better task_success_rate within the same wall-clock budget"
  "A conservative learning rate (3e-5) trades speed for stability and avoids the success-rate regressions higher rates cause"
  "Longer action-chunk prediction (50 steps) captures multi-step manipulation better than short chunks, raising success rate"
  "Adding camera viewpoints (3 views) gives the policy better spatial coverage of the scene, raising success rate"
)

MAX_ROUNDS="${MAX_ROUNDS:-10}"
ROUND_DURATION_SECONDS="${ROUND_DURATION_SECONDS:-90}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-5}"
JOB_HOURS=$(py "print(round(${ROUND_DURATION_SECONDS}/3600, 6))")

echo "==> Registering ${#AGENTS[@]} agents..."
for a in "${AGENTS[@]}"; do register_agent "$a"; done

echo "==> Creating marathon platform experiment (${MAX_ROUNDS} rounds x ${#AGENTS[@]} agents)..."
PE_ID=$(create_platform_experiment "Best Robotics VLA Policy — Marathon ${RUN_ID}" \
  "$(py "print(round(${#AGENTS[@]} * ${MAX_ROUNDS} * ${JOB_HOURS} * 3.0 / 0.40 * 1.2, 4))")" \
  "${#AGENTS[@]}" 0.40)
echo "  id: $PE_ID"
echo "  UI: http://localhost:3000/platform-experiments/${PE_ID}"
signup_and_start "$PE_ID" "${AGENTS[@]}"

agent_loop() {
  local agent="$1" lr="$2" chunk="$3" cam="$4" hypothesis="$5"
  local round job_id status

  for round in $(seq 1 "${MAX_ROUNDS}"); do
    local env_json theory
    env_json=$(py "import json; print(json.dumps({
      'OPENRESEARCH_LEARNING_RATE': '${lr}', 'OPENRESEARCH_CHUNK_LEN': '${chunk}',
      'OPENRESEARCH_CAMERA_VIEWS': '${cam}', 'OPENRESEARCH_REPORT_INTERVAL_SECONDS': '${REPORT_INTERVAL_SECONDS}',
      'OPENRESEARCH_BASELINE': '0.30',
    }))")
    theory="lr=${lr} chunk_len=${chunk} cameras=${cam}, same VLA baseline architecture as every other agent"
    job_id=$(submit_job_ext "$PE_ID" "$agent" "" "$JOB_HOURS" "$ROBOTICS_JOB_FILE" "$env_json" "" "" "" \
      "robotics-vla-marathon" "$theory" "maximize task_success_rate above 0.30 baseline" \
      "Round ${round}: ${hypothesis}")

    echo "[$(date +%T)] ${agent} round ${round}/${MAX_ROUNDS} -> ${job_id} submitted"

    while true; do
      status=$(get_status "$job_id")
      case "$status" in
        COMPLETED|FAILED|EVICTED|REJECTED) break ;;
      esac
      sleep 5
    done
    echo "[$(date +%T)] ${agent} round ${round}/${MAX_ROUNDS} -> ${job_id} ${status}"

    if [[ "$status" == "COMPLETED" ]]; then
      curl -sf -X POST "$SCHED_URL/experiments/${job_id}/summary" -H 'Content-Type: application/json' \
        -d "{\"summary\": \"Round ${round} (lr=${lr} chunk=${chunk} cam=${cam}) completed.\"}" > /dev/null 2>&1
    fi
    sleep 3
  done
  echo "[$(date +%T)] ${agent} finished all ${MAX_ROUNDS} rounds"
}

echo "==> Launching ${#AGENTS[@]} per-agent round loops (${MAX_ROUNDS} rounds each, ~${ROUND_DURATION_SECONDS}s/round)..."
pids=()
for idx in "${!AGENTS[@]}"; do
  agent_loop "${AGENTS[$idx]}" "${LRS[$idx]}" "${CHUNKS[$idx]}" "${CAMS[$idx]}" "${HYPOTHESES[$idx]}" &
  pids+=("$!")
done

wait "${pids[@]}"
echo "==> Marathon complete. Platform experiment: ${PE_ID}"
