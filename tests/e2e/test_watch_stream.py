"""GET /watch: the live change stream, exercised through `hl watch` -- the client agents actually
use (the Go port of the former agents/coordinator/experiments/hl-watch; see cli/internal/watchclient).
Ported 1:1 from tests/scenarios/watch-stream.sh, split one test per independently-diagnosable
property, per tests/improve.md #7 step 4:
  1. Order and completeness: a subscriber attached BEFORE submission sees QUEUED, SUBMITTED,
     RUNNING, COMPLETED, each exactly once, in that order, with strictly increasing cursors.
     Nothing here is satisfied by "the file was non-empty". Combined with `--until` exiting ON the
     terminal event (not on its timeout) in the same test, because both assertions read the same
     subscriber over the same job's whole life -- splitting them would mean paying for a second job
     to re-prove a fact this one subscriber already establishes.
  2. Replay: kill the connection while the job is RUNNING-bound, reconnect with the last cursor,
     and the transition that happened while disconnected must arrive from replay. See the long
     comment at that step for why this is the guarantee and not the documented limit.
  3. kinds scoping: a subscription naming one kind receives only that kind.
  4. An unknown kind is refused at the handshake rather than served an empty stream nobody can tell
     apart from a quiet one -- this one needs no job at all (parsing happens before scope
     validation), so it is the API-only, no-accelerator split-off: pure pytest.mark.parallel.

A NOTIFY inside a rolled-back transaction is deliberately NOT here: that is a property of one
transaction, provable in registry/db unit tests and unreachable through the HTTP API.

Properties 1-3 each submit and run one real job (a subscriber's `--until` has to observe genuine
QUEUED->...->COMPLETED transitions), so they carry pytest.mark.accelerator on top of parallel:
single-accelerator, bounded-capacity, tolerant of queueing -- never exclusive. Property 4 touches
no accelerator at all.
"""
from __future__ import annotations

import json
import subprocess
import time
from pathlib import Path
from typing import Callable

import pytest

from conftest import make_agent
from support.wait import DeadlineExceeded, Deadline, eventually

# H100, not the suite default (L40). This module is about the event stream, so it wants its jobs
# admitted promptly and holds each for barely a minute -- but a scenario that names no type lands
# on L40's 8 units alongside every other such scenario, and a queueing delay there would eat the
# deadline before the stream was ever exercised. The two H100 nodes carry 16 units; nothing else
# in the default (non-slow, non-exclusive) lane claims them wholesale. One accelerator for one job
# is the whole hardware footprint of each test below.
ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
JOB_HOURS = 0.02  # 72s of wall clock -- long enough to reconnect mid-run, short enough to finish
                  # comfortably inside a test's deadline.
# Budget arithmetic: 0.02h * 1 accelerator * 1.0 AccH/h (H100 is the AccH baseline itself, see
# acch_rate in controlplane/settings/hypothesisloop.yaml) = 0.02 AccH reserved, and eviction and
# billing land on observed consumption, which for a job that runs its estimate is the same figure.
# Each test below submits one job and never a second, so 0.2 AccH is a 10x headroom over the only
# debit it makes -- sized so that an overrunning job is billed rather than cut off mid-stream.
PE_BUDGET = 0.2

pytestmark = pytest.mark.parallel

RANK = {"QUEUED": 0, "SUBMITTED": 1, "ADMITTED": 2, "RUNNING": 3, "COMPLETED": 4}


# --- holding a websocket from Python -----------------------------------------------------------
# hl watch is a long-running subprocess, exactly as `hl-watch` was: every subscriber below is an
# `hl watch` process writing JSON lines to a file, and the assertions read the file. Killing the
# process IS killing the connection. Output goes to files (not subprocess.PIPE) so nothing here
# can block forever on an unread pipe -- reads are bounded polls against the file, never a
# blocking read on the subprocess's own stdout/stderr.
class Watcher:
    def __init__(self, proc: subprocess.Popen, out_path: Path, err_path: Path):
        self.proc = proc
        self.out_path = out_path
        self.err_path = err_path

    def stdout_text(self) -> str:
        return self.out_path.read_text() if self.out_path.exists() else ""

    def stderr_text(self) -> str:
        return self.err_path.read_text() if self.err_path.exists() else ""

    def events(self) -> list[dict]:
        events = []
        for line in self.stdout_text().splitlines():
            line = line.strip()
            if line.startswith("{"):
                events.append(json.loads(line))
        return events

    def status_values(self, job_id: str) -> list[str]:
        return [
            e["value"] for e in self.events()
            if e.get("kind") == "experiment.status" and e.get("subject") == job_id
        ]

    def event_kinds(self) -> set[str]:
        return {e.get("kind", "") for e in self.events()}

    def last_cursor(self) -> int | None:
        events = self.events()
        return events[-1]["cursor"] if events else None


@pytest.fixture
def watch(tmp_path: Path, api):
    """start_watch(*args) -> Watcher, backing one `hl watch` subprocess. Every watcher started
    through this fixture is killed at teardown regardless of outcome (pass, fail, or hang) --
    `hl watch` reconnects on its own on a broken stream, so an abandoned one keeps a socket open
    against the control plane for its whole --timeout; leaving one running would leave a socket
    open past the end of the test that started it."""
    started: list[tuple[subprocess.Popen, object, object]] = []
    counter = [0]

    def _start(*args: str) -> Watcher:
        counter[0] += 1
        out_path = tmp_path / f"watch-{counter[0]}.jsonl"
        err_path = tmp_path / f"watch-{counter[0]}.err"
        out_f = out_path.open("w")
        err_f = err_path.open("w")
        proc = subprocess.Popen(
            [str(api.hl_bin), "watch", "--url", api.base_url, *args],
            stdout=out_f, stderr=err_f, text=True,
        )
        started.append((proc, out_f, err_f))
        return Watcher(proc, out_path, err_path)

    yield _start

    for proc, out_f, err_f in started:
        if proc.poll() is None:
            proc.kill()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
        out_f.close()
        err_f.close()


def await_connected(description: str, watcher: Watcher, deadline: Deadline) -> None:
    eventually(
        description,
        watcher.stderr_text,
        accept=lambda text: "hl watch: connected" in text,
        deadline=deadline,
    )


def await_exit(description: str, watcher: Watcher, deadline: Deadline) -> int:
    """Wait for the watcher process to exit on its own, up to `deadline`. A watcher that has not
    exited within that budget is the failure this module is looking for (it hung) -- killed and
    reported as a synthetic 999 rather than waited out further, exactly like the bash scenario's
    `await_watch` (waiting it out to pytest's own timeout would just make the hang look like an
    unrelated deadline failure with a worse message)."""
    try:
        eventually(description, watcher.proc.poll, accept=lambda rc: rc is not None, deadline=deadline)
    except DeadlineExceeded:
        watcher.proc.kill()
        try:
            watcher.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass
        return 999
    return watcher.proc.returncode


def order_problems(watcher: Watcher, job_id: str) -> list[str]:
    """Mirrors watch-stream.sh's embedded ORDER_VERDICT python 1:1, just native now that the whole
    scenario is Python: every status the job passed through must be present exactly once, in
    ascending order of the transition it names, with strictly increasing cursors. ADMITTED is
    accepted between SUBMITTED and RUNNING because some placement paths use it; it is ranked, not
    ignored."""
    events = [e for e in watcher.events() if e.get("kind") == "experiment.status" and e.get("subject") == job_id]
    values = [e["value"] for e in events]
    problems = []
    unknown = [v for v in values if v not in RANK]
    if unknown:
        problems.append("unexpected status " + ",".join(unknown))
    missing = [v for v in ("QUEUED", "SUBMITTED", "RUNNING", "COMPLETED") if v not in values]
    if missing:
        problems.append("missing " + ",".join(missing))
    duplicated = sorted({v for v in values if values.count(v) > 1})
    if duplicated:
        problems.append("delivered twice: " + ",".join(duplicated))
    ranks = [RANK[v] for v in values if v in RANK]
    if ranks != sorted(ranks):
        problems.append("out of order")
    cursors = [e["cursor"] for e in events]
    if cursors != sorted(cursors) or len(set(cursors)) != len(cursors):
        problems.append("cursors not strictly increasing")
    return problems


@pytest.fixture
def watch_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "watch")
    pe_id = experiment(f"watch-stream-{run_id}", [agent], budget=PE_BUDGET, max_agents=1)
    return pe_id, agent


@pytest.mark.accelerator
def test_watch_before_submit_sees_ordered_sequence_and_until_exits_on_terminal(api, watch_pe, watch, deadline):
    pe_id, agent = watch_pe

    # Attaches before the job exists, which is the point: an event stream that only works once you
    # already know the id would not remove a single polling turn. Subscribing by
    # platform_experiment_id is how an agent watches a job it is about to create.
    all_watcher = watch(
        "--platform-experiment", pe_id,
        "--until", "status in COMPLETED,FAILED,EVICTED",
        "--timeout", str(max(30, int(deadline.remaining()) - 10)),
    )
    await_connected("watcher (all kinds) handshake", all_watcher, deadline)

    job = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={"accelerator_type": ACCELERATOR_TYPE})

    final = eventually(
        f"{job} to complete",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"job ended as {final['status']}; the terminal-state assertions below describe a completed run"
    completed_at = time.monotonic()
    # Required after any job waited to COMPLETED: only a COMPLETED job gates the agent's next
    # submission on a filed summary.
    api.file_finding(job)

    rc = await_exit("hl watch --until to exit on the terminal event", all_watcher, deadline)
    exit_delay = time.monotonic() - completed_at
    # Two independent halves, and both are needed. The exit code alone would be satisfied by a
    # client that sat until its timeout and happened to return 0; the delay alone would be
    # satisfied by one that crashed the moment the job finished. Together they say: it woke on the
    # event.
    assert rc == 0, f"hl watch --until exited {rc} (124=timed out, 999=never exited), stderr: {all_watcher.stderr_text()}"
    assert exit_delay <= 15, f"hl watch lingered {exit_delay:.1f}s past COMPLETED -- it did not exit on the event"

    seq = " ".join(all_watcher.status_values(job))
    problems = order_problems(all_watcher, job)
    assert not problems, f"status sequence [{seq}] is not a complete ordered run: {'; '.join(problems)}"


@pytest.mark.accelerator
def test_watch_kinds_scoping(api, watch_pe, watch, deadline):
    pe_id, agent = watch_pe

    timeout = str(max(30, int(deadline.remaining()) - 10))
    all_watcher = watch(
        "--platform-experiment", pe_id, "--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", timeout,
    )
    kinds_watcher = watch(
        "--platform-experiment", pe_id, "--kinds", "experiment.status",
        "--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", timeout,
    )
    await_connected("watcher (all kinds) handshake", all_watcher, deadline)
    await_connected("watcher (experiment.status) handshake", kinds_watcher, deadline)

    job = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={"accelerator_type": ACCELERATOR_TYPE})

    eventually(
        f"{job} to complete",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    api.file_finding(job)

    all_rc = await_exit("hl watch --until to exit on the terminal event (unfiltered)", all_watcher, deadline)
    assert all_rc == 0, f"unfiltered watcher exited {all_rc}, stderr: {all_watcher.stderr_text()}"
    kinds_rc = await_exit("the experiment.status-only subscriber to exit", kinds_watcher, deadline)
    assert kinds_rc == 0, (
        f"the experiment.status-only subscriber exited {kinds_rc} -- narrowing the kinds broke the "
        f"stream, stderr: {kinds_watcher.stderr_text()}"
    )

    all_kinds = all_watcher.event_kinds()
    narrow_kinds = kinds_watcher.event_kinds()
    other_kinds = all_kinds - {"experiment.status"}
    if not other_kinds:
        # Not a failure of scoping, but say so out loud: with only one kind on the wire, "received
        # only that kind" would be true of a filter that does nothing at all.
        pytest.skip("only experiment.status events occurred on this platform experiment; the scoping check is vacuous")
    leaked = narrow_kinds - {"experiment.status"}
    assert not leaked, f"kinds=experiment.status also received: {sorted(leaked)}"


@pytest.mark.accelerator
def test_watch_mid_run_disconnect_and_cursor_replay(api, watch_pe, watch, deadline):
    pe_id, agent = watch_pe

    submitted_watcher = watch(
        "--platform-experiment", pe_id, "--until", "status in SUBMITTED",
        "--timeout", str(max(30, int(deadline.remaining()) - 10)),
    )
    await_connected("watcher (until SUBMITTED) handshake", submitted_watcher, deadline)

    job = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={"accelerator_type": ACCELERATOR_TYPE})

    # The SUBMITTED watcher exits on its own the moment the job is handed to the cluster: that is
    # the connection dying mid-run, at a point where the job still has its whole life ahead of it.
    submitted_rc = await_exit("the SUBMITTED subscriber to see the job admitted", submitted_watcher, deadline)
    assert submitted_rc == 0, (
        f"subscriber waiting for SUBMITTED exited {submitted_rc} (124=timeout, 999=hung), "
        f"stderr: {submitted_watcher.stderr_text()}"
    )

    cursor = submitted_watcher.last_cursor()
    assert cursor not in (None, 0), "the stream gave the disconnected client no cursor to resume from"

    # Now let the job reach RUNNING with nobody connected on that cursor. Polling the REST status
    # here is deliberate: it establishes, independently of the stream, that the transition
    # happened during the gap -- so whatever the reconnecting client is handed for RUNNING cannot
    # have arrived live.
    reached = eventually(
        f"{job} to reach RUNNING with nobody watching",
        lambda: api.experiment(job)["status"],
        accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert reached == "RUNNING", f"job reached {reached} instead of RUNNING; the mid-run reconnect has nothing to replay"

    # Reconnect from the cursor of the last event the killed connection saw.
    #
    # Why RUNNING, and why this is not a test of the documented limit: replay carries no event
    # log. It re-derives events from the rows themselves, so it returns the status the row is IN,
    # plus the waypoints the row timestamps (queued_at, submitted_at). A status entered AND left
    # entirely inside the missed window collapses into the current one -- stated plainly in
    # db.EventsStore.Replay, and a deliberate consequence of not writing the same state twice.
    # Asserting that such a collapsed status replays would be asserting something the design says
    # it will not do; the scenario would be wrong, not the code. RUNNING is the case the feature
    # does guarantee: the job is still RUNNING at reconnect, the transition into it happened while
    # the client was away, and the client must be told. That is a real gap being closed, not a
    # limit.
    replay_started_at = time.monotonic()
    replay_watcher = watch(
        "--experiment", job, "--since", str(cursor), "--until", "status in RUNNING", "--timeout", "30",
    )
    replay_rc = await_exit("the reconnected subscriber to replay what it missed", replay_watcher, Deadline.in_seconds(35))
    replay_elapsed = time.monotonic() - replay_started_at

    assert replay_rc == 0, (
        f"reconnect with cursor {cursor} never delivered the missed RUNNING transition (exit {replay_rc}), "
        f"stderr: {replay_watcher.stderr_text()}"
    )
    # Delivered before the stream could have produced anything new -- the job was already RUNNING
    # when this process connected, so the event can only have come from replay.
    assert replay_elapsed <= 10, (
        f"reconnect took {replay_elapsed:.1f}s to deliver a transition that had already happened -- that is polling, not replay"
    )

    replayed = replay_watcher.status_values(job)
    assert replayed and replayed[0] == "RUNNING", (
        f"expected RUNNING first after reconnect, got: {replayed}"
    )
    # Replay must return what was missed and nothing else. QUEUED and SUBMITTED were both seen
    # before the disconnect and both carry cursors <= the one handed back, so re-delivering either
    # would mean the cursor does not actually bound the replay.
    assert not ({"QUEUED", "SUBMITTED"} & set(replayed)), (
        f"replay re-sent an event the client had already seen: {replayed}"
    )
    bad_cursors = [e for e in replay_watcher.events() if e.get("cursor", 0) <= cursor]
    assert not bad_cursors, (
        f"{len(bad_cursors)} replayed events carried a cursor at or before the resume point {cursor}"
    )


def test_watch_unknown_kind_refused_at_handshake(api, pe, run_id, watch, deadline):
    """No job needed at all: kind parsing happens before scope validation, so this needs only a
    platform experiment to name -- the API-only, no-accelerator split of this module."""
    pe_id = pe(f"watch-stream-bogus-{run_id}", PE_BUDGET, 1)

    bogus_watcher = watch("--platform-experiment", pe_id, "--kinds", "not.a.real.kind", "--timeout", "5")
    rc = await_exit("the unknown-kind subscription to be refused", bogus_watcher, Deadline.in_seconds(15))

    # Three things have to be true at once, and each one alone is satisfiable by a broken client:
    # the exit code is 2 (fatal, not retried to the timeout and then reported as success), the
    # status line says 400, and the platform's own message naming the offending kind reached the
    # caller. A client that printed the refusal but exited 0 would leave an agent branching on
    # success; one that exited 2 with a bare "400" would leave it guessing which of kind, id or
    # agent was wrong.
    stderr = bogus_watcher.stderr_text()
    assert rc == 2, f"unknown kind was not visibly refused (exit {rc}); stderr: {stderr}"
    assert "refused this subscription" in stderr
    assert "400" in stderr
    assert "not.a.real.kind" in stderr

    assert bogus_watcher.stdout_text() == "", (
        f"a subscription naming an unknown kind still delivered events: {bogus_watcher.stdout_text()}"
    )
