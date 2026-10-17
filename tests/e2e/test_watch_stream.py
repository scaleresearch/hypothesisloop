"""GET /watch, the live stream of changes, exercised through `hl watch`, the client agents use
(cli/internal/watchclient). Every change carries an id that only grows, and a client reads on
from an id with --after:
  1. A watcher attached before the job exists sees each status of it once, in order; --until
     exits on the terminal one; --kinds keeps only the kinds it names.
  2. A watcher that went away reads on with --after from the last id it saw and is given the
     change it missed, and nothing it had already seen.
  3. An unknown kind is refused at the handshake, rather than served an empty stream nobody can
     tell apart from a quiet one.
"""
from __future__ import annotations

import json
import subprocess
import time
from pathlib import Path

import pytest

from conftest import make_agent
from support.api import ENDED
from support.wait import DeadlineExceeded, Deadline, eventually

JOB_HOURS = 0.02  # 72s of wall clock: long enough to reconnect mid-run, short enough to finish
                  # comfortably inside a test's deadline.
# 0.02h of one core at 1.0 AccH/h is 0.02 AccH reserved. Each test submits one job, so 0.2 AccH is
# a 10x headroom over the only debit it makes: an overrunning job is billed, not cut off mid-stream.
PE_BUDGET = 0.2

pytestmark = pytest.mark.parallel

RANK = {"QUEUED": 0, "SUBMITTED": 1, "RUNNING": 2, "COMPLETED": 3}


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

    def last_id(self) -> int | None:
        events = self.events()
        return max(e["id"] for e in events) if events else None


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
            [str(api.hl_bin), "watch", *args],
            stdout=out_f, stderr=err_f, text=True, env={**api.hl_env(), "HL_STATE_DIR": str(tmp_path)},
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
    """Every status the job passed through is present exactly once, in the order of its life, each
    under an id above the one before."""
    events = [e for e in watcher.events() if e.get("kind") == "experiment.status" and e.get("subject") == job_id]
    values = [e["value"] for e in events]
    problems = []
    unknown = [v for v in values if v not in RANK]
    if unknown:
        problems.append("unexpected status " + ",".join(unknown))
    missing = [v for v in RANK if v not in values]
    if missing:
        problems.append("missing " + ",".join(missing))
    duplicated = sorted({v for v in values if values.count(v) > 1})
    if duplicated:
        problems.append("delivered twice: " + ",".join(duplicated))
    if [RANK[v] for v in values if v in RANK] != sorted(RANK[v] for v in values if v in RANK):
        problems.append("out of order")
    ids = [e["id"] for e in events]
    if ids != sorted(set(ids)):
        problems.append("ids that do not grow")
    return problems


@pytest.fixture
def watch_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "watch")
    pe_id = experiment(f"watch-stream-{run_id}", [agent], budget=PE_BUDGET, max_agents=1)
    return pe_id, agent


def test_a_job_is_seen_in_order_until_its_end_and_kinds_keep_what_they_name(api, watch_pe, watch, deadline):
    pe_id, agent = watch_pe
    timeout = str(max(30, int(deadline.remaining()) - 10))
    # Attached before the job exists, which is the point: an agent watches the platform experiment
    # for a job it is about to create.
    all_watcher = watch("--all", "--platform-experiment", pe_id, "--follow", "--timeout", timeout)
    kinds_watcher = watch("--all", "--platform-experiment", pe_id, "--kinds", "experiment.status", "--follow", "--timeout", timeout)
    await_connected("the watcher of every kind to connect", all_watcher, deadline)
    await_connected("the watcher of experiment.status to connect", kinds_watcher, deadline)

    job = api.submit_job(pe_id, agent, hours=JOB_HOURS)
    until_watcher = watch("--experiment", job, "--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", timeout)
    final = eventually(f"{job} to end", lambda: api.experiment(job), accept=lambda e: e["status"] in ENDED, deadline=deadline)
    assert final["status"] == "COMPLETED", f"job ended as {final['status']}; the assertions below describe a completed run"
    completed_at = time.monotonic()
    api.file_finding(job)

    # The exit code alone would be met by a client that sat out its timeout, the delay alone by
    # one that crashed when the job ended: together they say it woke on the change.
    rc = await_exit("hl watch --until to exit on the terminal status", until_watcher, deadline)
    assert rc == 0, f"hl watch --until exited {rc} (124=timed out, 999=never exited), stderr: {until_watcher.stderr_text()}"
    assert time.monotonic() - completed_at <= 15, "hl watch --until lingered past COMPLETED: it did not exit on the change"

    eventually("the finding to reach the watcher of every kind", all_watcher.event_kinds, accept=lambda kinds: "finding.new" in kinds, deadline=deadline)
    problems = order_problems(all_watcher, job)
    assert not problems, f"statuses {all_watcher.status_values(job)} are not one ordered run: {'; '.join(problems)}"
    assert kinds_watcher.event_kinds() == {"experiment.status"}, f"--kinds experiment.status also received: {kinds_watcher.event_kinds()}"
    assert all_watcher.event_kinds() - {"experiment.status"}, "only experiment.status changed: the --kinds check says nothing"


def test_a_watcher_that_went_away_reads_on_after_the_last_id_it_saw(api, watch_pe, watch, deadline):
    pe_id, agent = watch_pe
    job = api.submit_job(pe_id, agent, hours=JOB_HOURS)
    # It exits on its own once the job is handed to a cluster: a connection gone with the job's
    # life ahead of it.
    gone = watch("--experiment", job, "--until", "status in SUBMITTED", "--timeout", "60")
    rc = await_exit("the watcher until SUBMITTED to see the job admitted", gone, deadline)
    assert rc == 0, f"the watcher until SUBMITTED exited {rc}, stderr: {gone.stderr_text()}"
    last = gone.last_id()
    assert last, "the stream gave the watcher no id to read on from"

    # The job runs while nobody watches. The REST read says so independently of the stream, so
    # what the next watcher is told of RUNNING cannot have reached it live.
    reached = eventually(f"{job} to run with nobody watching", lambda: api.experiment(job)["status"],
                         accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"), deadline=deadline)
    assert reached == "RUNNING", f"job reached {reached} instead of RUNNING; there is nothing missed to read on to"

    started = time.monotonic()
    back = watch("--experiment", job, "--after", str(last), "--until", "status in RUNNING", "--timeout", "30")
    rc = await_exit("the watcher that came back to be told what it missed", back, Deadline.in_seconds(35))
    assert rc == 0, f"reading on after {last} never delivered the missed RUNNING (exit {rc}), stderr: {back.stderr_text()}"
    assert time.monotonic() - started <= 10, "the change that was missed came late: that is polling, not reading on"
    assert back.status_values(job) == ["RUNNING"], f"reading on after {last} delivered {back.status_values(job)}, want RUNNING alone"
    assert all(e["id"] > last for e in back.events()), f"reading on after {last} repeated a change: {back.events()}"
    api.cancel_job(job)


def test_watch_unknown_kind_refused_at_handshake(pe, run_id, watch):
    """Kinds are read before anything else, so this needs only a platform experiment to name."""
    pe_id = pe(f"watch-stream-bogus-{run_id}", PE_BUDGET, 1)

    bogus_watcher = watch("--all", "--platform-experiment", pe_id, "--kinds", "not.a.real.kind", "--timeout", "5")
    rc = await_exit("the unknown-kind subscription to be refused", bogus_watcher, Deadline.in_seconds(15))

    # Three things have to be true at once, and each one alone is satisfiable by a broken client:
    # the exit code is 2 (fatal, not retried to the timeout and then reported as success), the
    # status line says 422, and the platform's own message naming the offending kind reached the
    # caller. A client that printed the refusal but exited 0 would leave an agent branching on
    # success; one that exited 2 with a bare "422" would leave it guessing which of kind, id or
    # agent was wrong.
    stderr = bogus_watcher.stderr_text()
    assert rc == 2, f"unknown kind was not visibly refused (exit {rc}); stderr: {stderr}"
    assert "refused this subscription" in stderr
    assert "422" in stderr
    assert "not.a.real.kind" in stderr

    assert bogus_watcher.stdout_text() == "", (
        f"a subscription naming an unknown kind still delivered events: {bogus_watcher.stdout_text()}"
    )
