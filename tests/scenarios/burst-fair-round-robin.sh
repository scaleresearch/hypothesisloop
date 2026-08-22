#!/usr/bin/env bash
# Burst-tier admission fairness across agents: an agent with a deep burst queue must not be able
# to claim every unit of capacity that frees up in one tick ahead of another agent with only one
# job waiting (see interleaveByAgent in controlplane/services/scheduler/loop_sort.go).
#
# One full-node filler job (accelerator_count=8), owned by a THIRD agent so its completion and
# summary gate never touch either contender, frees the whole node in a single terminal
# transition — both of the node's two half-node (accelerator_count=4) slots become free together,
# in the same tick, with no risk of the two slots freeing at different times. The three then-queued
# jobs are A2, A3 (agent A, submitted first) and B1 (agent B, submitted last). Plain FIFO/
# priority-tiebreak order would admit A2 and A3 together (both older than B1) in that tick,
# starving B1 entirely. Round-robin interleaving must instead cap agent A at one of the two slots
# and admit B1 alongside it — whichever of A2/A3 actually wins that one slot (a transient per-job
# admission failure can legitimately let A3 go instead of A2; that's not what this scenario checks
# for).
# Needs the whole node's capacity accounted for with nothing else touching this accelerator type
# concurrently to keep admission order externally observable — run CLUSTER_EXCLUSIVE (see
# tests/run.sh), not in the concurrent SLOW_TESTS group.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
HOURS="$(scale_budget 0.01)"
AGENT_FILLER="agent-rr-filler-${RUN_ID}"
AGENT_A="agent-rr-a-${RUN_ID}"
AGENT_B="agent-rr-b-${RUN_ID}"
register_agent "$AGENT_FILLER"
register_agent "$AGENT_A"
register_agent "$AGENT_B"
PE_ID=$(create_platform_experiment "burst-round-robin-${RUN_ID}" 50.0 3)
signup_and_start "$PE_ID" "$AGENT_FILLER" "$AGENT_A" "$AGENT_B"

is_admitted() { local s; s=$(get_status "$1"); [[ "$s" == "RUNNING" || "$s" == "ADMITTED" ]]; }

# Every job here is pinned to the one accelerator type whose capacity this scenario accounts for.
# The shared generic spec also lists pricier acceptable alternates, and the scheduler may
# legitimately place a job on one when the requested type is momentarily full -- at which point
# "the node has exactly two slots" stops being true and the admission order this scenario reads
# is no longer about fairness at all. See pin_job_flavor.
PINNED_JOB_FILE=$(pin_job_flavor)

# One filler (a third agent) claims the whole node — its single completion frees both half-node
# slots together, in one tick, with no dependence on two separate jobs completing in sync.
FILLER=$(submit_job "$PE_ID" "$AGENT_FILLER" "burst" "$HOURS" "$ACCELERATOR_TYPE" "8" "" "$PINNED_JOB_FILE")
echo "  ==> filler=$FILLER submitted, waiting for it to claim the node..."
wait_for_status "$FILLER" "RUNNING,ADMITTED" "$ADMISSION_BUDGET_SECONDS" > /dev/null \
  || { fail "$FILLER never admitted; round-robin setup failed"; close_platform_experiment "$PE_ID"; finish; }

# A2 and A3 (agent A) queue strictly before B1 (agent B) — plain FIFO/tiebreak order is A2, A3, B1.
A2=$(submit_job "$PE_ID" "$AGENT_A" "burst" "$HOURS" "$ACCELERATOR_TYPE" "4" "" "$PINNED_JOB_FILE")
A3=$(submit_job "$PE_ID" "$AGENT_A" "burst" "$HOURS" "$ACCELERATOR_TYPE" "4" "" "$PINNED_JOB_FILE")
B1=$(submit_job "$PE_ID" "$AGENT_B" "burst" "$HOURS" "$ACCELERATOR_TYPE" "4" "" "$PINNED_JOB_FILE")
echo "  ==> queued behind the filler: A2=$A2 A3=$A3 B1=$B1 (B1 submitted last)"

# The filler frees the whole node in one transition. It belongs to neither contending agent, so
# its own summary-gate requirement (a COMPLETED job without a filed summary blocks further
# admission for that same (agent, platform experiment) pair — loop_tick.go 3a) never touches
# agent A or agent B; only file it so the filler's own agent doesn't matter for cleanup.
FILLER_WAIT=$(( ADMISSION_BUDGET_SECONDS + $(completion_wait_tries "$HOURS") ))
FILLER_FINAL=$(wait_for_status "$FILLER" "COMPLETED,FAILED,EVICTED" "$FILLER_WAIT" || true)
if [[ "$FILLER_FINAL" == "COMPLETED" ]]; then
  file_finding "$FILLER"
else
  # The node still frees either way (any terminal status stops holding capacity), so the race
  # below remains meaningful — but a filler that didn't complete cleanly means setup didn't go as
  # planned, worth surfacing rather than silently treating as equivalent to the intended path.
  fail "filler $FILLER did not complete cleanly (status=$FILLER_FINAL) — setup did not go as planned"
fi

# The property under test: of the two slots that free together, agent A may take at most one
# (whichever of A2/A3 actually claims it — a transient per-job admission failure legitimately
# lets A3 go instead of A2, and that's not a fairness violation) and agent B's only queued job
# must be among the two admitted. The actual violation this guards against is agent A taking
# BOTH slots (A2 and A3 both admitted) while B1 is left waiting — that's queue-depth
# monopolization, the thing interleaveByAgent exists to prevent.
RACE_WAIT=$(( ADMISSION_BUDGET_SECONDS + 10 ))
A2_ADMITTED=0 B1_ADMITTED=0 A3_ADMITTED=0
for _ in $(seq 1 "$RACE_WAIT"); do
  is_admitted "$A2" && A2_ADMITTED=1
  is_admitted "$B1" && B1_ADMITTED=1
  is_admitted "$A3" && A3_ADMITTED=1
  AGENT_A_COUNT=$((A2_ADMITTED + A3_ADMITTED))
  # Conclusive as soon as either: agent A already has both slots (violation, no point waiting
  # longer), or B1 is admitted (agent A capped at one slot — success, whichever of A2/A3 it was).
  [[ "$AGENT_A_COUNT" -ge 2 || "$B1_ADMITTED" -eq 1 ]] && break
  sleep 1
done
echo "  ==> after the node freed: A2_ADMITTED=$A2_ADMITTED B1_ADMITTED=$B1_ADMITTED A3_ADMITTED=$A3_ADMITTED"

if [[ "$B1_ADMITTED" -eq 1 && $((A2_ADMITTED + A3_ADMITTED)) -le 1 ]]; then
  pass "agent B's only queued job (B1) was admitted alongside at most one of agent A's two jobs (A2=$A2_ADMITTED A3=$A3_ADMITTED) — round-robin bounded agent A's queue-depth advantage"
elif [[ "$A2_ADMITTED" -eq 1 && "$A3_ADMITTED" -eq 1 ]]; then
  # Two different states produce this, and only one of them is a fairness bug. The scheduler
  # already granted A2-vs-A3 the same caveat: a job that loses its reservation to a concurrent
  # one is skipped and the next in interleaved order takes the slot. When that job is B1, agent A
  # ends up with both slots without ever having been ordered ahead of B1.
  #
  # not_admitted_reason separates them exactly (see notAdmittedReasonFor): "outranked" means B1
  # was behind both of A's jobs in admission order — the monopolization this scenario exists to
  # catch. Anything else means B1 was ordered fine and lost its slot to a transient conflict,
  # which says nothing about interleaving.
  B1_REASON=$(get_field "$B1" not_admitted_reason)
  if [[ "$B1_REASON" == outranked* ]]; then
    fail "A2 and A3 (both agent A) were admitted together and B1 was '$B1_REASON' — burst admission is not interleaving across agents (regression in interleaveByAgent?)"
  else
    pass "agent A took both slots only because B1 lost its own reservation ('${B1_REASON:-none}'), not because it was ordered behind A's queue — interleaving is intact"
  fi
else
  fail "unexpected admission outcome: A2=$A2_ADMITTED B1=$B1_ADMITTED A3=$A3_ADMITTED — investigate admission accounting"
fi

close_platform_experiment "$PE_ID"
for j in "$FILLER" "$A2" "$A3" "$B1"; do
  s=$(wait_for_status "$j" "COMPLETED,FAILED,EVICTED,REJECTED" 60 || true)
  [[ "$s" == "COMPLETED" || "$s" == "FAILED" || "$s" == "EVICTED" || "$s" == "REJECTED" ]] \
    || fail "cleanup did not make $j terminal (status=$s)"
done
finish
