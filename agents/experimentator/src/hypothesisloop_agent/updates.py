"""Platform updates about the agent's own resources, delivered by the harness.

The harness runs `hl watch` whenever the agent ends its turn, and briefly after each of its tool
calls: the platform holds whatever happened while the agent worked and replays it from the cursor
hl saved, so a batch waits server-side instead of in this process, survives a restart, and is
delivered once whichever of the two reads it. An event is a pointer; a job that ended is handed
over with the machine it ran on and its metrics, and to its agent with the count of its jobs
still unfinished and the quota it has used, read once per batch, so that learning of it costs
no call.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from collections.abc import Callable

from hypothesisloop_agent import config

log = logging.getLogger("llm_agent")

HL_WATCH = ["hl", "watch"]
_RETRY_S = 30.0
# A drain gives up after this long on a platform that does not answer.
_DRAIN_S = 5

# The kinds that wake whoever ended its turn, per role, and the one place that says so. An event
# of another kind stays on the platform (hl watch --wake) and is handed over with the next that
# wakes, after the next tool call, or once IDLE_NUDGE_S has passed. A coordinator has to learn
# within seconds that an agent finished, that a job is held by what waiting will not clear, that
# a stage advanced, that someone wrote a comment, that the brief changed or the run closed. An
# agent has to learn the same of its own jobs ending. Everything else is read when next acting.
WAKES = {
    "coordinator": ("agent.finished", "experiment.blocked", "stage.boundary", "comment.new",
                    "platform_experiment.status", "platform_experiment.description"),
    "agent": ("experiment.status", "experiment.blocked", "job.underperforming", "agent.cut", "stage.boundary",
              "comment.new", "platform_experiment.status", "platform_experiment.description"),
}

# How long an agent that ended its turn waits without any update before it is nudged: one that
# ended its turn with nothing in flight would otherwise wait for an update that never comes.
IDLE_NUDGE_S = 1800
IDLE_PROMPT = (
    "No platform updates for 30 minutes. Continue with the most useful work available, or end your "
    "turn to keep waiting for updates: reply in one line and call no tool."
)


def watch_env(api_url: str, token: str, statedir: str) -> dict[str, str]:
    """The environment hl watch needs: the API, the token, which says to hl who is watching, and
    a state dir that outlives the process."""
    return {**os.environ, "API_URL": api_url, "HL_TOKEN": token,
            "HL_STATE_DIR": os.path.join(os.path.abspath(statedir), ".hl-state")}


def _agent(env: dict[str, str]) -> str:
    """The agent the token in env is, or "" for a coordinator's."""
    return config.token_scope(env["HL_TOKEN"])[2]


def watch_command(env: dict[str, str]) -> list[str]:
    """An agent watches its own resources. A coordinator watches the whole platform experiment,
    narrowed to what may need a decision."""
    if _agent(env):
        return HL_WATCH
    return [*HL_WATCH, "--all", "--decisions"]


async def _run_hl(command: list[str], env: dict[str, str], stopped: Callable[[], bool],
                  warn: bool = True) -> tuple[int | None, str]:
    proc = await asyncio.create_subprocess_exec(*command, env=env, stdout=asyncio.subprocess.PIPE,
                                                stderr=asyncio.subprocess.PIPE)
    communicate = asyncio.ensure_future(proc.communicate())
    try:
        while not communicate.done():
            if stopped():
                proc.kill()
                await communicate
                return None, ""
            await asyncio.wait({communicate}, timeout=1)
        stdout, stderr = communicate.result()
    finally:
        if proc.returncode is None:
            proc.kill()
            await proc.wait()
    if proc.returncode != 0 and warn:
        log.warning("%s exited %s: %s", " ".join(command[:3]), proc.returncode, stderr.decode(errors="replace").strip()[-500:])
    return proc.returncode, stdout.decode(errors="replace")


def _events(stdout: str) -> list[dict]:
    return [json.loads(line) for line in stdout.splitlines() if line.startswith("{")]


_UNFINISHED = ("QUEUED", "SUBMITTED", "RUNNING")
_ENDED = ("COMPLETED", "FAILED", "EVICTED", "REJECTED", "CANCELLED")


def _machine(job: dict) -> str:
    """Where job ran, short: the physical machine of a rented instance, else the node, then the
    accelerator's model or, without one, the CPU's. `hl job list` prints the same as ran_on."""
    ran_on = job.get("ran_on")
    if not ran_on:
        return ""
    machine = ran_on.get("machine") or {}
    where = f"host {machine['host_id']}" if machine.get("host_id") else f"node {ran_on['node']}"
    return " ".join(part for part in (where, machine.get("accelerator_model") or machine.get("cpu_model")) if part)


def _result(job: dict) -> str:
    return ", ".join([
        *(f"{m['name']} best {m['best']:.6g} last {m['last']:.6g}" + ("" if m["basis"] == "raw" else f" ({m['basis']})")
          for m in job.get("metrics", [])),
        *([f"ranks nowhere: {job['not_rankable_reason']}"] if job.get("rankable") is False else [])])


async def _with_job_state(events: list[dict], env: dict[str, str], hl: str) -> list[dict]:
    """events, completed with what whoever is told of an ended job would otherwise read: the
    machine it ran on, and for a COMPLETED one its declared metrics and, when it ranks nowhere,
    why, as its event's detail. An agent's batch closes with two more events, the count of its
    unfinished jobs and its quota used. One hl call each per batch; what one could not read is
    left out."""
    def ended(e: dict) -> bool:
        return e["kind"] == "experiment.status" and e["value"] in _ENDED

    ids = list(dict.fromkeys(e["subject"] for e in events if ended(e)))
    if not ids:
        return events
    _, stdout = await _run_hl([hl, "job", "status", *ids], env, lambda: False)
    jobs = {job["id"]: job for job in _events(stdout)}

    def told(e: dict) -> dict:
        job = jobs.get(e["subject"]) if ended(e) else None
        if job is None:
            return e
        return dict(e, machine=_machine(job), **({"detail": _result(job)} if e["value"] == "COMPLETED" else {}))

    events = [told(e) for e in events]
    agent = _agent(env)
    if not agent:
        return events
    _, stdout = await _run_hl([hl, "job", "stats", "--agent", agent], env, lambda: False)
    for stats in _events(stdout):
        counts = stats["by_status"]
        events.append({"kind": "unfinished_jobs",
                       "value": ", ".join(f"{counts[s]} {s}" for s in _UNFINISHED if counts.get(s)) or "none"})
    _, stdout = await _run_hl([hl, "quota"], env, lambda: False)
    for q in _events(stdout):
        events.append({"kind": "quota_used", "value": (
            f"{q['used_guaranteed_acch'] + q['used_burst_acch']:.3g} of {q['share_accelerator_hours']:g} AccH, "
            f"room for a job of {q['remaining_guaranteed_accelerator_hours']:.3g} guaranteed "
            f"or {q['remaining_burst_accelerator_hours']:.3g} burst")})
    return events


# A drain runs after every tool call, so an outage is logged by the first that fails, not by each.
_drain_failing = False


async def drain(env: dict[str, str], command: list[str] | None = None) -> list[dict]:
    """The events already waiting on the cursor, without waiting for more."""
    global _drain_failing
    command = command or watch_command(env)
    code, stdout = await _run_hl([*command, "--no-wait", "--timeout", str(_DRAIN_S)], env, lambda: False,
                                 warn=not _drain_failing)
    _drain_failing = code != 0
    return await _with_job_state(_events(stdout), env, command[0])


async def next_batch(env: dict[str, str], stopped: Callable[[], bool], idle_s: float,
                     command: list[str] | None = None, retry_s: float = _RETRY_S) -> list[dict]:
    """Runs hl watch until an event that wakes this role arrives, and returns it with what waited
    before it. Once idle_s passes without one, returns what waits, which may be nothing. Returns
    [] as soon as stopped() is true. A failed hl watch is retried after retry_s."""
    command = command or watch_command(env)
    waking = [*command, "--wake", ",".join(WAKES["agent" if _agent(env) else "coordinator"])]
    give_up = time.monotonic() + idle_s
    while not stopped() and time.monotonic() < give_up:
        code, stdout = await _run_hl(waking, env, stopped)
        events = _events(stdout)
        if events:
            return await _with_job_state(events, env, command[0])
        if code not in (0, None):
            deadline = min(give_up, time.monotonic() + retry_s)
            while not stopped() and time.monotonic() < deadline:
                await asyncio.sleep(min(1.0, retry_s))
    return [] if stopped() else await drain(env, command)


def event_line(event: dict, agent_id: str) -> str:
    """One compact line per event: kind, subject, value, detail, the machine an ended job ran on,
    and who did it when not the agent."""
    parts = [event.get("kind", ""), event.get("subject", ""), event.get("value", ""), event.get("detail", "")]
    if event.get("machine"):
        parts.append(f"on {event['machine']}")
    actor = event.get("agent_id", "")
    if actor and actor != agent_id:
        parts.append(f"by {actor}")
    return " ".join(part for part in parts if part)


def message(events: list[dict], agent_id: str) -> str:
    return "Platform updates:\n" + "\n".join(dict.fromkeys(event_line(e, agent_id) for e in events))
