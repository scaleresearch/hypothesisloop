"""Platform updates about the agent's own resources, delivered by the harness.

The harness runs `hl watch` whenever the agent ends its turn, and briefly after each of its tool
calls: the platform holds whatever happened while the agent worked and replays it from the cursor
hl saved, so a batch waits server-side instead of in this process, survives a restart, and is
delivered once whichever of the two reads it. An event is a pointer; a job that ended is handed
over with its metrics, the count of the agent's jobs still unfinished and the quota it has used,
read once per batch, so that learning of it costs the agent no call.
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

# How long an agent that ended its turn waits without any update before it is nudged: one that
# ended its turn with nothing in flight would otherwise wait for an update that never comes.
IDLE_NUDGE_S = 1800
IDLE_PROMPT = (
    "No platform updates for 30 minutes. Continue with the most useful work available, or end your "
    "turn to keep waiting for updates."
)


def watch_env(api_url: str, token: str, workdir: str) -> dict[str, str]:
    """The environment hl watch needs: the API, the token, which says to hl who is watching, and
    a state dir that outlives the process."""
    return {**os.environ, "API_URL": api_url, "HL_TOKEN": token,
            "HL_STATE_DIR": os.path.join(os.path.abspath(workdir), ".hl-state")}


def _agent(env: dict[str, str]) -> str:
    """The agent the token in env is, or "" for a coordinator's."""
    return config.token_scope(env["HL_TOKEN"])[2]


def watch_command(env: dict[str, str]) -> list[str]:
    """An agent watches its own resources. A coordinator watches the whole platform experiment,
    narrowed to what may need a decision. Both are woken as soon as hl returns a batch."""
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


async def _with_job_state(events: list[dict], env: dict[str, str], hl: str) -> list[dict]:
    """events, completed with what an agent told of an ended job would otherwise read: each
    COMPLETED job's declared metrics as its event's detail, and two closing events: the count of
    the agent's unfinished jobs and its quota used. Three hl calls per batch; what one could not
    read is left out. A coordinator's events are every agent's and are handed over as they are."""
    agent = _agent(env)
    if not agent or not any(e["kind"] == "experiment.status" for e in events):
        return events

    def completed(e: dict) -> bool:
        return e["kind"] == "experiment.status" and e["value"] == "COMPLETED"

    ids = list(dict.fromkeys(e["subject"] for e in events if completed(e)))
    detail = {}
    if ids:
        _, stdout = await _run_hl([hl, "job", "status", *ids], env, lambda: False)
        detail = {job["id"]: ", ".join(
            f"{m['name']} best {m['best']:.6g} last {m['last']:.6g}" + ("" if m["basis"] == "raw" else f" ({m['basis']})")
            for m in job.get("metrics", [])) for job in _events(stdout)}
    events = [dict(e, detail=detail[e["subject"]]) if completed(e) and e["subject"] in detail else e for e in events]
    _, stdout = await _run_hl([hl, "job", "stats", "--agent", agent], env, lambda: False)
    for stats in _events(stdout):
        counts = stats["by_status"]
        events.append({"kind": "unfinished_jobs",
                       "value": ", ".join(f"{counts[s]} {s}" for s in _UNFINISHED if counts.get(s)) or "none"})
    _, stdout = await _run_hl([hl, "quota"], env, lambda: False)
    for q in _events(stdout):
        events.append({"kind": "quota_used", "value": (
            f"{q['used_guaranteed_acch'] + q['used_burst_acch']:.3g} of {q['share_accelerator_hours']:g} AccH, "
            f"{q['remaining_accelerator_hours']:.3g} remaining for guaranteed and burst jobs together")})
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
    """Runs hl watch until it returns events, and returns them. Returns [] once idle_s passes
    without any, or as soon as stopped() is true. A failed hl watch is retried after retry_s."""
    command = command or watch_command(env)
    give_up = time.monotonic() + idle_s
    while not stopped() and time.monotonic() < give_up:
        code, stdout = await _run_hl(command, env, stopped)
        events = _events(stdout)
        if events:
            return await _with_job_state(events, env, command[0])
        if code not in (0, None):
            deadline = min(give_up, time.monotonic() + retry_s)
            while not stopped() and time.monotonic() < deadline:
                await asyncio.sleep(min(1.0, retry_s))
    return []


def event_line(event: dict, agent_id: str) -> str:
    """One compact line per event: kind, subject, value, detail, and who did it when not the agent."""
    parts = [event.get("kind", ""), event.get("subject", ""), event.get("value", ""), event.get("detail", "")]
    actor = event.get("agent_id", "")
    if actor and actor != agent_id:
        parts.append(f"by {actor}")
    return " ".join(part for part in parts if part)


def message(events: list[dict], agent_id: str) -> str:
    return "Platform updates:\n" + "\n".join(dict.fromkeys(event_line(e, agent_id) for e in events))
