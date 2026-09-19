"""Platform updates about the agent's own resources, delivered by the harness between turns.

The harness runs `hl watch` in the foreground whenever the agent ends its turn: the platform holds
whatever happened while the agent worked and replays it from the cursor hl saved, so a batch
waits server-side instead of in this process, and survives a restart.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from collections.abc import Callable

log = logging.getLogger("llm_agent")

HL_WATCH = ["hl", "watch"]
_RETRY_S = 30.0

# How long an agent that ended its turn waits without any update before it is nudged: one that
# ended its turn with nothing in flight would otherwise wait for an update that never comes.
IDLE_NUDGE_S = 1800
IDLE_PROMPT = (
    "No platform updates for 30 minutes. Continue with the most useful work available, or end your "
    "turn to keep waiting for updates."
)


def watch_env(api_url: str, agent_id: str, platform_experiment_id: str, workdir: str) -> dict[str, str]:
    """The environment hl watch needs: identity, API, and a state dir that outlives the process."""
    return {**os.environ, "API_URL": api_url, "AGENT_ID": agent_id,
            "PLATFORM_EXPERIMENT_ID": platform_experiment_id,
            "HL_STATE_DIR": os.path.join(os.path.abspath(workdir), ".hl-state")}


async def _run_watch(command: list[str], env: dict[str, str], stopped: Callable[[], bool]) -> tuple[int | None, str]:
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
    if proc.returncode != 0:
        log.warning("hl watch exited %s: %s", proc.returncode, stderr.decode(errors="replace").strip()[-500:])
    return proc.returncode, stdout.decode(errors="replace")


async def next_batch(env: dict[str, str], stopped: Callable[[], bool], idle_s: float,
                     command: list[str] = HL_WATCH, retry_s: float = _RETRY_S) -> list[dict]:
    """Runs hl watch until it returns events, and returns them. Returns [] once idle_s passes
    without any, or as soon as stopped() is true. A failed hl watch is retried after retry_s."""
    give_up = time.monotonic() + idle_s
    while not stopped() and time.monotonic() < give_up:
        code, stdout = await _run_watch(command, env, stopped)
        events = [json.loads(line) for line in stdout.splitlines() if line.startswith("{")]
        if events:
            return events
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
    return "Platform updates:\n" + "\n".join(event_line(e, agent_id) for e in events)
