#!/usr/bin/env bash
# Manual demo driver (not part of the automated suite — see tests/scenarios/
# robotics-competing-hypotheses.sh for the assertion-based version of this same flow).
# Spins up a "best robotics VLA policy" platform experiment where several agents train the
# same VLA baseline but each bets on a different hypothesis, then leaves the platform
# experiment open/running so the UI has live, multi-agent competing metrics to show.
#
# Usage:
#   bash tests/robotics-demo.sh
#   DURATION_SECONDS=180 bash tests/robotics-demo.sh
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/lib/common.sh"
source "$DIR/lib/api.sh"

ROBOTICS_JOB_FILE="${JOB_FILE:-${DIR}/workload-robotics/job.yaml}"

# Parallel arrays (agent / lr / chunk_len / cameras / hypothesis) — macOS ships bash 3.2,
# which has no associative arrays.
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

DURATION_SECONDS="${DURATION_SECONDS:-180}"
REPORT_INTERVAL_SECONDS="${REPORT_INTERVAL_SECONDS:-5}"
JOB_HOURS=$(py "print(round(${DURATION_SECONDS}/3600, 6))")

echo "==> Registering ${#AGENTS[@]} agents..."
for idx in "${!AGENTS[@]}"; do
  register_agent "${AGENTS[$idx]}"
  echo "  ${AGENTS[$idx]}: ok (lr=${LRS[$idx]} chunk_len=${CHUNKS[$idx]} cameras=${CAMS[$idx]})"
done

echo ""
echo "==> Creating platform experiment..."
PE_ID=$(create_platform_experiment "Best Robotics VLA Policy — ${RUN_ID}" 20.0 "${#AGENTS[@]}" 0.40)
echo "  id: $PE_ID"
signup_and_start "$PE_ID" "${AGENTS[@]}"

echo ""
echo "==> Submitting jobs (one per agent, same VLA baseline, different hypothesis)..."
declare -a JOB_IDS
for idx in "${!AGENTS[@]}"; do
  ENV_JSON=$(py "import json; print(json.dumps({
    'OPENRESEARCH_LEARNING_RATE': '${LRS[$idx]}',
    'OPENRESEARCH_CHUNK_LEN': '${CHUNKS[$idx]}',
    'OPENRESEARCH_CAMERA_VIEWS': '${CAMS[$idx]}',
    'OPENRESEARCH_REPORT_INTERVAL_SECONDS': '${REPORT_INTERVAL_SECONDS}',
    'OPENRESEARCH_BASELINE': '0.30',
  }))")
  JOB_ID=$(submit_job_ext "$PE_ID" "${AGENTS[$idx]}" "" "$JOB_HOURS" "$ROBOTICS_JOB_FILE" "$ENV_JSON" \
    "" "" "" "robotics-vla-demo" \
    "lr=${LRS[$idx]} chunk_len=${CHUNKS[$idx]} cameras=${CAMS[$idx]}, same VLA baseline architecture as every other agent in this experiment" \
    "maximize task_success_rate above 0.30 baseline" "${HYPOTHESES[$idx]}")
  echo "  ${AGENTS[$idx]} (lr=${LRS[$idx]} chunk=${CHUNKS[$idx]} cam=${CAMS[$idx]}) -> $JOB_ID"
  JOB_IDS+=("$JOB_ID")
done

echo ""
echo "==> Jobs submitted. Platform experiment: ${PE_ID}"
echo "    UI: http://localhost:3000/platform-experiments/${PE_ID}"
echo ""
echo "==> Polling for up to 60s so you can see them move to RUNNING..."
for i in $(seq 1 12); do
  statuses=()
  for idx in "${!JOB_IDS[@]}"; do
    statuses+=("${AGENTS[$idx]}=$(get_status "${JOB_IDS[$idx]}")")
  done
  echo "  [${i}] $(IFS=', '; echo "${statuses[*]}")"
  sleep 5
done

echo ""
echo "==> Done. Jobs keep running/reporting metrics for ~${DURATION_SECONDS}s total from submission."
echo "    Open the UI to watch task_success_rate / action_mse race across the same VLA baseline"
echo "    under 4 competing hyperparameter hypotheses (high-lr / low-lr / long-chunk / multi-cam)."
