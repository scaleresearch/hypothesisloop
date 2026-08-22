#!/usr/bin/env bash
# GET /watch: the live change stream, exercised through hl-watch — the client agents actually use.
# Four properties, all about the stream and none about scheduling:
#   1. Order and completeness: a subscriber attached BEFORE submission sees QUEUED, SUBMITTED,
#      RUNNING, COMPLETED, each exactly once, in that order, with strictly increasing cursors.
#      Nothing here is satisfied by "the file was non-empty".
#   2. Replay: kill the connection while the job is RUNNING-bound, reconnect with the last cursor,
#      and the transition that happened while disconnected must arrive from replay. See the long
#      comment at that step for why this is the guarantee and not the documented limit.
#   3. hl-watch --until exits ON the terminal event, not on its timeout — asserted on the exit
#      code AND on how long after COMPLETED it took, so a client that hung to the timeout and then
#      exited 0 could not pass.
#   4. kinds scoping: a subscription naming one kind receives only that kind, and an unknown kind
#      is refused at the handshake rather than served an empty stream nobody can tell from a quiet
#      one.
# A NOTIFY inside a rolled-back transaction is deliberately NOT here: that is a property of one
# transaction, provable in registry/db unit tests and unreachable through the HTTP API.
#
# API-only, parallel-safe, one single-accelerator job.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/../lib/common.sh"
source "$DIR/../lib/api.sh"

HL_WATCH="${DIR}/../../agents/coordinator/experiments/hl-watch"
[[ -r "$HL_WATCH" ]] || { echo "hl-watch not found at $HL_WATCH" >&2; exit 2; }
# hl-watch is deliberately stdlib-only Python (the workload base images are arbitrary), so the
# same python3 every other helper in this suite uses is all it needs. Invoked through the
# interpreter rather than by its shebang so a non-executable checkout still runs.
command -v python3 >/dev/null || { echo "hl-watch needs python3" >&2; exit 2; }

# H100, not the suite default (L40). This scenario is about the event stream, so it wants its one
# job admitted promptly and holds it for barely a minute — but a scenario that names no type lands
# on L40's 8 units alongside every other such scenario, and a queueing delay there would eat the
# ceiling before the stream was ever exercised. A100's 8 units are claimed wholesale by
# burst-fair-round-robin and preemption-requeue, both CLUSTER_EXCLUSIVE. The two H100 nodes carry
# 16 units and their only other users (distributed-jobs, mixed-admission) are SLOW, i.e. absent
# from the default run. One accelerator for one job is the whole hardware footprint here.
ACCELERATOR_TYPE="nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
JOB_HOURS=0.02   # 72s of wall clock — long enough to reconnect mid-run, short enough to complete
                 # inside the per-scenario ceiling.
# Budget arithmetic: 0.02h * 1 accelerator * 1.0 AccH/h (H100 is the AccH baseline itself, see
# acch_rate in controlplane/settings/hypothesisloop.yaml) = 0.02 AccH reserved, and eviction and
# billing land on observed consumption, which for a job that runs its estimate is the same figure.
# One job is submitted here and never a second, so 0.2 AccH is a 10x headroom over the only debit
# this scenario makes — sized so that an overrunning job is billed rather than cut off mid-stream.
PE_BUDGET=0.2

AGENT="agent-watch-${RUN_ID}"
register_agent "$AGENT"
PE_ID=$(create_platform_experiment "watch-stream-${RUN_ID}" "$PE_BUDGET" 1)
signup_and_start "$PE_ID" "$AGENT"

# --- holding a socket from bash -------------------------------------------------------------
# A shell cannot hold a WebSocket across commands; that is precisely why hl-watch exists. So every
# subscriber below is an hl-watch process writing JSON lines to a file, and the assertions read
# the file. Killing the process IS killing the connection.
WATCH_PIDS=()

# start_watch OUT_FILE ARGS... -> backgrounds one hl-watch and sets WATCH_PID, registering it for
# teardown. stderr goes beside stdout in OUT_FILE.err so a refused handshake stays inspectable.
#
# The pid comes back in a global rather than on stdout, and await_watch reports through one too,
# because $(...) runs in a subshell: a process started there is not a child of this shell, and
# `wait` on a non-child returns 127 having waited for nothing. The exit code of the client is half
# of what this scenario asserts, so it has to be the real one.
start_watch() {
  local out="$1"; shift
  python3 "$HL_WATCH" --url "$API_URL" "$@" > "$out" 2> "${out}.err" &
  WATCH_PID=$!
  WATCH_PIDS+=("$WATCH_PID")
}

# hl-watch reconnects by itself on a broken stream, so an abandoned one keeps a socket open
# against the control plane for its whole timeout. A scenario killed by the suite's outer timeout
# would leave several of them running against every scenario that follows. Chained onto common.sh's
# on_exit rather than installed as a second EXIT trap: bash keeps only the latest handler per
# signal, and replacing that one would drop the platform-experiment cleanup it does.
kill_watchers() {
  local pid
  for pid in "${WATCH_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  return 0
}
trap 'kill_watchers; on_exit' EXIT

proc_gone() { ! kill -0 "$1" 2>/dev/null; }

# await_watch PID DESC TRIES -> waits for one hl-watch to exit and sets WATCH_RC to its exit code.
# A watcher that has not exited within TRIES is the failure this scenario is looking for (it hung),
# so it is killed and reported as 999 rather than waited out — waiting it out would spend the whole
# ceiling and report the hang as a timeout.
await_watch() {
  local pid="$1" desc="$2" tries="${3:-20}"
  WATCH_RC=0
  if ! wait_until "$desc" "$tries" 1 proc_gone "$pid"; then
    kill "$pid" 2>/dev/null || true
    WATCH_RC=999
    return 0
  fi
  wait "$pid" || WATCH_RC=$?
}

# status_values FILE JOB -> the value of every experiment.status event for JOB, one per line, in
# arrival order. Reading the stream the way a client does: parse the JSON, do not grep the text.
status_values() {
  py "
import json, sys
for line in open('$1'):
    line = line.strip()
    if not line.startswith('{'):
        continue
    e = json.loads(line)
    if e.get('kind') == 'experiment.status' and e.get('subject') == '$2':
        print(e['value'])
"
}

# event_kinds FILE -> the distinct event kinds present in a watcher's output, one per line.
event_kinds() {
  py "
import json
kinds = set()
for line in open('$1'):
    line = line.strip()
    if line.startswith('{'):
        kinds.add(json.loads(line).get('kind', ''))
for k in sorted(kinds):
    print(k)
"
}

# last_cursor FILE -> the cursor of the last event a watcher saw: exactly what a reconnecting
# client hands back as --since. Empty when it saw none.
last_cursor() {
  py "
import json
cursor = ''
for line in open('$1'):
    line = line.strip()
    if line.startswith('{'):
        cursor = json.loads(line).get('cursor', cursor)
print(cursor)
"
}

echo "  -- subscribe before submitting; the whole status sequence, in order, with no gap --"
# Both of these attach before the job exists, which is the point: an event stream that only works
# once you already know the id would not remove a single polling turn. Subscribing by
# platform_experiment_id is how an agent watches a job it is about to create.
#
# WATCH_ALL takes every kind; WATCH_STATUS names one. They run over the identical window so the
# kinds assertion later compares like with like.
WATCH_TIMEOUT=$(( $(scenario_seconds_left) - 10 ))
ALL_OUT="${TMPDIR_T}/watch-all.jsonl"
KINDS_OUT="${TMPDIR_T}/watch-kinds.jsonl"
SUBMITTED_OUT="${TMPDIR_T}/watch-until-submitted.jsonl"
start_watch "$ALL_OUT" --platform-experiment "$PE_ID" \
  --until 'status in COMPLETED,FAILED,EVICTED' --timeout "$WATCH_TIMEOUT"
ALL_PID=$WATCH_PID
start_watch "$KINDS_OUT" --platform-experiment "$PE_ID" --kinds experiment.status \
  --until 'status in COMPLETED,FAILED,EVICTED' --timeout "$WATCH_TIMEOUT"
KINDS_PID=$WATCH_PID
# The third subscriber is the one whose connection gets killed mid-run: it stops at SUBMITTED and
# is gone for the QUEUED->...->RUNNING window.
start_watch "$SUBMITTED_OUT" --platform-experiment "$PE_ID" \
  --until 'status in SUBMITTED' --timeout "$WATCH_TIMEOUT"
SUBMITTED_PID=$WATCH_PID
# The sockets are established asynchronously by three processes; give them their handshake before
# creating the row whose events they must not miss. This is not a timing assumption about the
# platform — if a watcher were still connecting, its own replay-free subscription would simply
# start late, which is the failure mode assertion 1 exists to catch.
sleep 3

JOB=$(submit_job "$PE_ID" "$AGENT" "guaranteed" "$JOB_HOURS" "$ACCELERATOR_TYPE" "1")

echo "  -- mid-run disconnect and cursor replay --"
# The SUBMITTED watcher exits on its own the moment the job is handed to the cluster: that is the
# connection dying mid-run, at a point where the job still has its whole life ahead of it.
await_watch "$SUBMITTED_PID" "the SUBMITTED subscriber to see the job admitted" "$ADMISSION_BUDGET_SECONDS"
SUBMITTED_RC=$WATCH_RC
[[ "$SUBMITTED_RC" == "0" ]] \
  && pass "subscriber attached before submission observed SUBMITTED live and exited on it" \
  || fail "subscriber waiting for SUBMITTED exited $SUBMITTED_RC (124=timeout, 999=hung), stderr: $(<"${SUBMITTED_OUT}.err")"

CURSOR=$(last_cursor "$SUBMITTED_OUT")
[[ -n "$CURSOR" && "$CURSOR" != "0" ]] \
  && pass "every event carries a cursor a client can reconnect with (last seen: $CURSOR)" \
  || fail "the stream gave the disconnected client no cursor to resume from"

# Now let the job reach RUNNING with nobody connected on that cursor. Polling the REST status here
# is deliberate: it establishes, independently of the stream, that the transition happened during
# the gap — so whatever the reconnecting client is handed for RUNNING cannot have arrived live.
S=$(wait_for_status "$JOB" "RUNNING,COMPLETED,FAILED,EVICTED" "$ADMISSION_BUDGET_SECONDS" || true)
if [[ "$S" != "RUNNING" ]]; then
  fail "job reached $S instead of RUNNING; the mid-run reconnect has nothing to replay"
else
  # Reconnect from the cursor of the last event the killed connection saw.
  #
  # Why RUNNING, and why this is not a test of the documented limit: replay carries no event log.
  # It re-derives events from the rows themselves, so it returns the status the row is IN, plus the
  # waypoints the row timestamps (queued_at, submitted_at). A status entered AND left entirely
  # inside the missed window collapses into the current one — stated plainly in
  # db.EventsStore.Replay, and a deliberate consequence of not writing the same state twice.
  # Asserting that such a collapsed status replays would be asserting something the design says it
  # will not do; the scenario would be wrong, not the code. RUNNING is the case the feature does
  # guarantee: the job is still RUNNING at reconnect, the transition into it happened while the
  # client was away, and the client must be told. That is a real gap being closed, not a limit.
  REPLAY_OUT="${TMPDIR_T}/watch-replay.jsonl"
  REPLAY_STARTED_AT=$(date +%s)
  start_watch "$REPLAY_OUT" --experiment "$JOB" --since "$CURSOR" \
    --until 'status in RUNNING' --timeout 30
  await_watch "$WATCH_PID" "the reconnected subscriber to replay what it missed" 35
  REPLAY_RC=$WATCH_RC
  REPLAY_ELAPSED=$(( $(date +%s) - REPLAY_STARTED_AT ))

  REPLAYED=$(status_values "$REPLAY_OUT" "$JOB")
  if [[ "$REPLAY_RC" != "0" ]]; then
    fail "reconnect with cursor $CURSOR never delivered the missed RUNNING transition (exit $REPLAY_RC), stderr: $(<"${REPLAY_OUT}.err")"
  else
    # Delivered before the stream could have produced anything new — the job was already RUNNING
    # when this process connected, so the event can only have come from replay.
    [[ "$REPLAY_ELAPSED" -le 10 ]] \
      && pass "reconnect replayed the missed transition immediately (${REPLAY_ELAPSED}s), not by waiting for a live event" \
      || fail "reconnect took ${REPLAY_ELAPSED}s to deliver a transition that had already happened — that is polling, not replay"
    [[ "$(echo "$REPLAYED" | head -1)" == "RUNNING" ]] \
      && pass "the first status the reconnected client is handed is RUNNING — the transition it missed" \
      || fail "expected RUNNING first after reconnect, got: $(echo "$REPLAYED" | tr '\n' ' ')"
    # Replay must return what was missed and nothing else. QUEUED and SUBMITTED were both seen
    # before the disconnect and both carry cursors <= the one handed back, so re-delivering either
    # would mean the cursor does not actually bound the replay.
    if echo "$REPLAYED" | grep -qx 'QUEUED\|SUBMITTED'; then
      fail "replay re-sent an event the client had already seen: $(echo "$REPLAYED" | tr '\n' ' ')"
    else
      pass "replay returned only events after the cursor — no duplicate of what was already seen"
    fi
    BAD_CURSORS=$(py "
import json
print(sum(1 for line in open('$REPLAY_OUT')
          if line.strip().startswith('{') and json.loads(line).get('cursor', 0) <= $CURSOR))
")
    [[ "$BAD_CURSORS" == "0" ]] \
      && pass "every replayed event carries a cursor strictly greater than the one resumed from" \
      || fail "$BAD_CURSORS replayed events carried a cursor at or before the resume point $CURSOR"
  fi
fi

echo "  -- --until exits on the terminal state, not on the timeout --"
S=$(wait_for_status "$JOB" "COMPLETED,FAILED,EVICTED" "$(completion_wait_tries "$JOB_HOURS")" || true)
COMPLETED_AT=$(date +%s)
[[ "$S" == "COMPLETED" ]] || fail "job ended as $S; the terminal-state assertions below describe a completed run"
# Required after any job waited to COMPLETED: only a COMPLETED job gates the agent's next
# submission on a filed summary. This scenario submits no second job, but leaving the gate closed
# would be a trap for whatever edits it next.
file_finding "$JOB"

await_watch "$ALL_PID" "hl-watch --until to exit on the terminal event" 20
ALL_RC=$WATCH_RC
ALL_EXIT_DELAY=$(( $(date +%s) - COMPLETED_AT ))
# Two independent halves, and both are needed. The exit code alone would be satisfied by a client
# that sat until its timeout and happened to return 0; the delay alone would be satisfied by one
# that crashed the moment the job finished. Together they say: it woke on the event.
[[ "$ALL_RC" == "0" ]] \
  && pass "hl-watch --until exited 0 on the terminal state (timeout would be 124)" \
  || fail "hl-watch --until exited $ALL_RC with a ${WATCH_TIMEOUT}s timeout (124=timed out, 999=never exited)"
[[ "$ALL_EXIT_DELAY" -le 15 ]] \
  && pass "it exited ${ALL_EXIT_DELAY}s after COMPLETED, with a ${WATCH_TIMEOUT}s timeout it never came near" \
  || fail "hl-watch lingered ${ALL_EXIT_DELAY}s past COMPLETED — it did not exit on the event"

# The ordering assertion, on the subscriber that was attached for the entire life of the job.
# Every status the job passed through must be present exactly once, in ascending order of the
# transition it names, with strictly increasing cursors. ADMITTED is accepted between SUBMITTED and
# RUNNING because some placement paths use it; it is ranked, not ignored.
SEQ=$(status_values "$ALL_OUT" "$JOB" | tr '\n' ' ')
ORDER_VERDICT=$(py "
import json
RANK = {'QUEUED': 0, 'SUBMITTED': 1, 'ADMITTED': 2, 'RUNNING': 3, 'COMPLETED': 4}
events = []
for line in open('$ALL_OUT'):
    line = line.strip()
    if not line.startswith('{'):
        continue
    e = json.loads(line)
    if e.get('kind') == 'experiment.status' and e.get('subject') == '$JOB':
        events.append(e)
values = [e['value'] for e in events]
problems = []
unknown = [v for v in values if v not in RANK]
if unknown:
    problems.append('unexpected status ' + ','.join(unknown))
missing = [v for v in ('QUEUED', 'SUBMITTED', 'RUNNING', 'COMPLETED') if v not in values]
if missing:
    problems.append('missing ' + ','.join(missing))
duplicated = sorted({v for v in values if values.count(v) > 1})
if duplicated:
    problems.append('delivered twice: ' + ','.join(duplicated))
ranks = [RANK[v] for v in values if v in RANK]
if ranks != sorted(ranks):
    problems.append('out of order')
cursors = [e['cursor'] for e in events]
if cursors != sorted(cursors) or len(set(cursors)) != len(cursors):
    problems.append('cursors not strictly increasing')
print('; '.join(problems) if problems else 'OK')
")
[[ "$ORDER_VERDICT" == "OK" ]] \
  && pass "QUEUED -> SUBMITTED -> RUNNING -> COMPLETED arrived in order, once each, cursors ascending [$SEQ]" \
  || fail "status sequence [$SEQ] is not a complete ordered run: $ORDER_VERDICT"

echo "  -- kinds scoping --"
await_watch "$KINDS_PID" "the experiment.status-only subscriber to exit" 20
KINDS_RC=$WATCH_RC
[[ "$KINDS_RC" == "0" ]] \
  && pass "a subscription narrowed to experiment.status still receives the events it asked for" \
  || fail "the experiment.status-only subscriber exited $KINDS_RC — narrowing the kinds broke the stream"
ALL_KINDS=$(event_kinds "$ALL_OUT")
NARROW_KINDS=$(event_kinds "$KINDS_OUT")
OTHER_KINDS=$(echo "$ALL_KINDS" | grep -vx 'experiment.status' || true)
if [[ -z "$OTHER_KINDS" ]]; then
  # Not a failure of scoping, but say so out loud: with only one kind on the wire, "received only
  # that kind" would be true of a filter that does nothing at all.
  echo "  [INFO] only experiment.status events occurred on this platform experiment; the scoping check below is vacuous"
else
  LEAKED=$(echo "$NARROW_KINDS" | grep -vx 'experiment.status' || true)
  [[ -z "$LEAKED" ]] \
    && pass "kinds=experiment.status received none of the other kinds the unfiltered subscriber saw ($(echo "$OTHER_KINDS" | tr '\n' ' '))" \
    || fail "kinds=experiment.status also received: $(echo "$LEAKED" | tr '\n' ' ')"
fi

# An unrecognised kind must be refused. Silently serving an empty stream would be the exact failure
# /watch exists to end: a client waiting forever on something that looks healthy. So the assertion
# is on the refusal, never merely on "no events arrived" — a quiet stream looks identical.
BOGUS_OUT="${TMPDIR_T}/watch-bogus.jsonl"
start_watch "$BOGUS_OUT" --platform-experiment "$PE_ID" --kinds not.a.real.kind --timeout 5
await_watch "$WATCH_PID" "the unknown-kind subscription to be refused" 15
BOGUS_RC=$WATCH_RC
# Matched on the refused handshake and its status line, not on the server's message: hl-watch
# prints the response's status line and headers and discards the JSON body, so the "unknown kind
# X" text the API actually returns never reaches the client's output. Nor does the refusal change
# its exit code — with no --until, hl-watch retries a permanently-refused subscription until the
# timeout and then exits 0. Both are recorded here because a scenario asserting on either would be
# asserting something the CLI does not currently give it.
if grep -q 'handshake refused' "${BOGUS_OUT}.err" && grep -q '400' "${BOGUS_OUT}.err"; then
  pass "an unknown kind is refused at the handshake (HTTP 400), not served an empty stream"
else
  fail "unknown kind was not visibly refused (exit $BOGUS_RC); stderr: $(<"${BOGUS_OUT}.err")"
fi
[[ ! -s "$BOGUS_OUT" ]] \
  && pass "a refused subscription yields no events at all" \
  || fail "a subscription naming an unknown kind still delivered events: $(<"$BOGUS_OUT")"

finish
