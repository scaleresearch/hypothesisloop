#!/usr/bin/env bash
# Shared bash test-lib: source this (then api.sh / cluster.sh as needed) from any
# scenario under tests/scenarios/. Every scenario gets its own RUN_ID so concurrent
# scenarios never collide on agent IDs, job IDs or platform-experiment names.
set -euo pipefail

# How long a job's admission (queueing/scheduling delay, not its own runtime) may take before a
# scenario gives up — scales with concurrent suite load (see tests/run.sh), small fixed default
# when a scenario is run standalone since there's no contention to budget for.
ADMISSION_BUDGET_SECONDS="${ADMISSION_BUDGET_SECONDS:-60}"

API_URL="${API_URL:-http://localhost:8081}"
PROM_URL="${PROM_URL:-http://localhost:4000/v1/prometheus}"
JOB_NS="${JOB_NS:-hypothesisloop-jobs}"
CLUSTER_NS="${CLUSTER_NS:-hypothesisloop}"

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_DIR="$(cd "${LIB_DIR}/.." && pwd)"
JOB_FILE="${JOB_FILE:-${SCRIPT_DIR}/workloads/generic/job.yaml}"

# Which accelerator the generic scenarios schedule onto. The generic workload is plain Python
# (tests/workloads/generic/train.py) and never touches the device, so any type the cluster really
# advertises works — the scenarios only need the request to be admissible. Defaults to the NVIDIA
# dev cluster; point it at whatever `GET /resource-catalog/capacity` lists to run the suite on
# other hardware, e.g. TEST_ACCELERATOR_TYPE=tenstorrent.com/chipArch=blackhole.
# TEST_ACCH_RATE must match this type's acch_rate in controlplane/settings/hypothesisloop.yaml —
# scenarios that assert on reserved cost derive it from here.
TEST_ACCELERATOR_TYPE="${TEST_ACCELERATOR_TYPE:-nvidia.com/gpu.product=NVIDIA-L40}"
TEST_ACCH_RATE="${TEST_ACCH_RATE:-0.25}"
# The rate the scenarios' hardcoded budgets were sized against; scale_budget converts them.
TEST_BASELINE_ACCH_RATE=0.25
# Extra pod-level resources the chosen accelerator's runtime needs, as a JSON object (e.g.
# '{"hugepages-1Gi":"4Gi"}'). Empty for types that need none.
TEST_ACCELERATOR_POD_RESOURCES="${TEST_ACCELERATOR_POD_RESOURCES:-}"

# PID makes RUN_ID unique even if two scenarios start in the same second.
RUN_ID="$(date +%s)-$$"

TMPDIR_T="$(mktemp -d)"

# Every platform experiment a scenario creates is recorded here (a file, not an array: the
# create call runs in a $(...) subshell, so an array assignment would not survive it).
CREATED_PE_LOG="${TMPDIR_T}/created-platform-experiments"
: > "$CREATED_PE_LOG"

# A scenario that dies mid-way — a failed assertion, the suite's timeout, an operator ^C — used
# to leave its platform experiment `running`. That is not leftover noise: a running platform
# experiment still admits jobs, so it keeps competing for the same accelerators as a real run.
# Close them however the scenario exits.
cleanup_created_platform_experiments() {
  [[ -s "$CREATED_PE_LOG" ]] || return 0
  local pe
  while read -r pe; do
    [[ -n "$pe" ]] || continue
    curl -sf -m 10 -X POST "$API_URL/platform-experiments/${pe}/close" \
      -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1 || true
  done < "$CREATED_PE_LOG"
}

on_exit() {
  local rc=$?
  cleanup_created_platform_experiments
  rm -rf "$TMPDIR_T"
  return "$rc"
}
trap on_exit EXIT
# Without these a ^C or the suite's `timeout` kills the shell without running the EXIT trap.
trap 'exit 130' INT
trap 'exit 143' TERM

py() { python3 -c "$@"; }

# scale_budget BUDGET — a budget is spent in AccH (wall-clock hours * the type's acch_rate), so a
# figure tuned for the default accelerator buys proportionally less on a pricier one and the
# scenario's own jobs stop fitting. Converts a budget expressed at the baseline rate to the
# configured one, leaving it untouched on the default.
scale_budget() {
  py "print(round($1 * $TEST_ACCH_RATE / $TEST_BASELINE_ACCH_RATE, 6))"
}

# The generic job spec pins one accelerator type. Rather than have every scenario override it,
# rewrite the spec once here when TEST_ACCELERATOR_TYPE asks for something else, so JOB_FILE is
# already correct everywhere it is read. acceptable_accelerator_types is dropped along with it:
# those are alternates for the default type and never apply to a substituted one.
if [[ "$JOB_FILE" == "${SCRIPT_DIR}/workloads/generic/job.yaml" ]]; then
  _rendered="${TMPDIR_T}/job.yaml"
  TEST_ACCELERATOR_TYPE="$TEST_ACCELERATOR_TYPE" \
  TEST_ACCELERATOR_POD_RESOURCES="$TEST_ACCELERATOR_POD_RESOURCES" \
  python3 - "$JOB_FILE" "$_rendered" <<'PY'
import json, os, sys
import yaml

src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    job = yaml.safe_load(f)

want = os.environ["TEST_ACCELERATOR_TYPE"]
if job.get("accelerator_type") != want:
    job["accelerator_type"] = want
    job.pop("acceptable_accelerator_types", None)
    job.pop("accelerator_tolerations", None)

extra = os.environ.get("TEST_ACCELERATOR_POD_RESOURCES", "").strip()
if extra:
    job["accelerator_pod_resources"] = json.loads(extra)

with open(dst, "w") as f:
    yaml.safe_dump(job, f, sort_keys=False)
PY
  JOB_FILE="$_rendered"
fi

# Cluster preconditions (see preflight.sh); sourced last so scenarios run standalone get them too.
source "${LIB_DIR}/preflight.sh"

FAILED=0
pass() { echo "  [PASS] $*"; }
fail() { echo "  [FAIL] $*"; FAILED=1; }

# One deadline for the whole scenario, shared by every wait below.
#
# Each wait carries its own generous budget, which is right in isolation: none of them knows what
# else the scenario still has to do. But the suite kills a scenario at SCENARIO_TIMEOUT_SECONDS
# regardless, so on a contended cluster a run could spend its entire allowance inside the first two
# waits and be SIGTERMed mid-assertion — surfacing as a bare [FAIL] with no failed assertion in it,
# which reads as a product bug rather than a slow cluster. Clamping every wait to what is actually
# left turns that into an explicit, attributable [BUDGET] line instead.
#
# The margin leaves room for the scenario's own cleanup (closing its platform experiments) to run
# before the suite's timeout fires.
SCENARIO_BUDGET_MARGIN_SECONDS="${SCENARIO_BUDGET_MARGIN_SECONDS:-15}"
SCENARIO_BUDGET_SECONDS=$(( ${SCENARIO_TIMEOUT_SECONDS:-240} - SCENARIO_BUDGET_MARGIN_SECONDS ))
# A ceiling at or below the margin would leave nothing to spend and fail every wait instantly,
# turning a misconfigured timeout into a wall of unexplained failures. Fall back to the ceiling
# itself: the scenario then loses only the cleanup margin, and is still bounded by the number the
# operator actually set — never by a floor larger than it, which would put this budget back on the
# wrong side of the suite's own timeout.
[[ "$SCENARIO_BUDGET_SECONDS" -lt 1 ]] && SCENARIO_BUDGET_SECONDS="${SCENARIO_TIMEOUT_SECONDS:-240}"
SCENARIO_STARTED_AT=$(date +%s)

# scenario_seconds_left prints how much of the scenario's ceiling remains, never below zero.
scenario_seconds_left() {
  local left=$(( SCENARIO_BUDGET_SECONDS - ($(date +%s) - SCENARIO_STARTED_AT) ))
  [[ "$left" -lt 0 ]] && left=0
  echo "$left"
}

# budgeted_tries prints the smaller of a wait's own tries and what the scenario can still afford,
# given each try costs SLEEP seconds. Zero means the scenario is out of time.
budgeted_tries() {
  local tries="$1" sleep_s="${2:-1}" left affordable
  left=$(scenario_seconds_left)
  affordable=$(( left / sleep_s ))
  # Polling checks first and only sleeps between failed attempts, so any remaining time at all buys
  # one more check. Rounding that down to zero would give up a whole interval early — and the check
  # it skips is the one most likely to have just become true.
  [[ "$affordable" -lt 1 && "$left" -gt 0 ]] && affordable=1
  [[ "$affordable" -lt "$tries" ]] && tries="$affordable"
  [[ "$tries" -lt 0 ]] && tries=0
  echo "$tries"
}

# budget_exhausted reports the scenario running out of time as its own distinct outcome — never a
# silent timeout that reads like the platform failing to do something.
budget_exhausted() {
  echo "  [BUDGET] scenario ran out of time waiting for: $1 (ceiling ${SCENARIO_BUDGET_SECONDS}s)" >&2
}

# wait_until DESC TRIES SLEEP CHECK_CMD...  — polls CHECK_CMD (a command, not a string) until
# it exits 0 or TRIES is exhausted. Every "wait for X" in this suite reduces to this.
wait_until() {
  local desc="$1" tries="$2" sleep_s="$3"; shift 3
  tries=$(budgeted_tries "$tries" "$sleep_s")
  if [[ "$tries" -le 0 ]]; then
    budget_exhausted "$desc"
    return 1
  fi
  for ((i = 1; i <= tries; i++)); do
    if "$@"; then return 0; fi
    [[ "$i" -lt "$tries" ]] && sleep "$sleep_s"
  done
  echo "  [TIMEOUT] $desc (${tries}x${sleep_s}s)" >&2
  return 1
}

# Call at the end of every scenario script; exits 1 if any pass()/fail() call recorded a failure.
finish() {
  if [[ "$FAILED" == "1" ]]; then
    echo "==> RESULT: FAILED"
    exit 1
  fi
  echo "==> RESULT: PASSED"
}
