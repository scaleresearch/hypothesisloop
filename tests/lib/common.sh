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
trap 'rm -rf "$TMPDIR_T"' EXIT

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

# wait_until DESC TRIES SLEEP CHECK_CMD...  — polls CHECK_CMD (a command, not a string) until
# it exits 0 or TRIES is exhausted. Every "wait for X" in this suite reduces to this.
wait_until() {
  local desc="$1" tries="$2" sleep_s="$3"; shift 3
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
