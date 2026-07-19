#!/usr/bin/env bash
# Assertion-based competing-agents flow: several agents train the same VLA baseline
# (tests/workloads/robotics) but each bets on a different hyperparameter hypothesis,
# competing under one platform experiment. Confirms each job runs its own hypothesis to a
# terminal state and reports its own metrics — the code path the UI's "competing agents"
# chart depends on. API-only, parallel-safe.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

ROBOTICS_JOB_FILE="$SCRIPT_DIR/workloads/robotics/job.yaml"
JOB_HOURS="0.02"

AGENTS=("agent-hi-lr-${RUN_ID}" "agent-lo-lr-${RUN_ID}")
LRS=("1e-3" "3e-5")
HYPOTHESES=(
  "A higher learning rate (1e-3) converges faster to a better task_success_rate"
  "A conservative learning rate (3e-5) trades speed for stability"
)
for a in "${AGENTS[@]}"; do register_agent "$a"; done
PE_ID=$(create_platform_experiment "robotics-competing-${RUN_ID}" 1.0 "${#AGENTS[@]}")
signup_and_start "$PE_ID" "${AGENTS[@]}"

declare -a JOBS
for i in "${!AGENTS[@]}"; do
  ENV_JSON=$(py "import json; print(json.dumps({'OPENRESEARCH_LEARNING_RATE': '${LRS[$i]}', 'OPENRESEARCH_BASELINE': '0.30'}))")
  J=$(submit_job_ext "$PE_ID" "${AGENTS[$i]}" "guaranteed" "$JOB_HOURS" "$ROBOTICS_JOB_FILE" "$ENV_JSON" \
    "" "" "" "robotics-vla-demo" "lr=${LRS[$i]}, same VLA baseline as every other agent" \
    "maximize task_success_rate above 0.30 baseline" "${HYPOTHESES[$i]}")
  JOBS+=("$J")
  echo "  ${AGENTS[$i]} (lr=${LRS[$i]}) -> $J"
done

OK=0
for J in "${JOBS[@]}"; do
  S=$(wait_for_status "$J" "COMPLETED,FAILED,EVICTED" 150 || true)
  if [[ "$S" == "COMPLETED" ]]; then
    M=$(dashboard_metrics "$J")
    COUNT=$(echo "$M" | py "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo 0)
    if [[ "$COUNT" -ge 1 ]]; then
      OK=$((OK + 1))
    else
      fail "$J completed but reported 0 metric points"
    fi
  else
    fail "$J ended as $S instead of COMPLETED"
  fi
done
[[ "$OK" == "${#JOBS[@]}" ]] \
  && pass "all ${#JOBS[@]} competing-hypothesis jobs completed and reported metrics" \
  || fail "only $OK/${#JOBS[@]} competing-hypothesis jobs completed with metrics"

close_platform_experiment "$PE_ID"
finish
