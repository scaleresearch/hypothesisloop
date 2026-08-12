#!/usr/bin/env python3
"""Anthropic entry point — the shared agent (see core.py) driven by the Claude Agent SDK.

Runs on claude_agent_sdk rather than a hand-rolled ReAct loop — it's Claude Code packaged as a
library, run on our own infra (same container, same platform APIs) instead of Anthropic-hosted.
This gives us, for free, what a hand-rolled loop had to reinvent badly: the agent loop itself, a
real bash/file/web tool surface (no bespoke run_bash/call_api — the model just curls the platform
API directly via the built-in Bash tool), and server-side context compaction for long runs.
Auth: Claude subscription (Claude Code login) or
ANTHROPIC_API_KEY, exactly as Claude Code itself resolves it.

Uses ClaudeSDKClient (bidirectional, resumable) rather than one-shot query(): a platform
experiment runs for hours to days, and a one-shot query() ends the whole process the moment the
model reaches a natural end_turn — which happens constantly, e.g. any time the model "waits" for
a background job instead of blocking on it. This session survives that: an end_turn just gets a
follow-up nudge on the same connection (same context, same prompt cache) instead of the process
exiting. The session_id is persisted to disk so that even a genuine process crash/redeploy resumes
the same conversation (via `resume=`) rather than starting over cold.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import time

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    RateLimitEvent,
    ResultMessage,
    TextBlock,
    ToolUseBlock,
)

from hypothesisloop_agent import core

log = logging.getLogger("llm_agent")

# These are interactive-harness features (a resumable Claude Code session with a human/UI on the
# other end) that don't do anything meaningful inside this worker, but the SDK's default tool set
# still advertises them — and the model, seeing them, reasonably tries to use them to "wait" for a
# background job. Confirmed live: an agent called ScheduleWakeup expecting to be resumed later and
# just ended its turn instead, because nothing here ever fires that wakeup. Strip them so the model
# never reaches for a tool that can't do what its name promises in this environment.
DISALLOWED_TOOLS = ["ScheduleWakeup", "Monitor", "TaskCreate", "TaskUpdate", "TaskGet", "TaskList",
                     "TaskOutput", "TaskStop"]


def _log_message(message) -> bool:
    """Logs message and returns True iff this message was a tool use (see idle-nudge backoff)."""
    used_tool = False
    if isinstance(message, AssistantMessage):
        for block in message.content:
            if isinstance(block, TextBlock) and block.text.strip():
                log.info("assistant: %s", block.text.strip()[:800])
            elif isinstance(block, ToolUseBlock):
                log.info("tool_use: %s(%s)", block.name, json.dumps(block.input, default=str)[:400])
                used_tool = True
    elif isinstance(message, ResultMessage):
        log.info(
            "result: subtype=%s turns=%d cost_usd=%s stop_reason=%s",
            message.subtype, message.num_turns, message.total_cost_usd, message.stop_reason,
        )
    elif isinstance(message, RateLimitEvent):
        info = message.rate_limit_info
        log.info(
            "rate_limit: status=%s type=%s utilization=%s resets_at=%s",
            info.status, info.rate_limit_type, info.utilization, info.resets_at,
        )
    return used_tool


def _session_file(setup: core.RunSetup) -> str:
    return os.path.join(setup.workdir, ".claude_session_id")


def _load_resume_id(setup: core.RunSetup) -> str | None:
    path = _session_file(setup)
    try:
        with open(path) as f:
            session_id = f.read().strip()
        return session_id or None
    except FileNotFoundError:
        return None


def _save_resume_id(setup: core.RunSetup, session_id: str | None) -> None:
    if not session_id:
        return
    with open(_session_file(setup), "w") as f:
        f.write(session_id)


# Nudge sent on the same connection whenever the model ends its turn without a real stop
# condition being true (see core.stop_reason) — keeps the conversation (and its prompt cache)
# alive across what would otherwise be process-ending idle exits.
CONTINUE_PROMPT = (
    "Your turn ended but the platform experiment is still open and you are not held. Nothing "
    "resumes this process on a timer — a background wakeup you scheduled will NOT wake it — so "
    "check current platform state and continue with the most useful work available rather than "
    "ending your turn to wait idle."
)


# Backoff for reconnect attempts that aren't governed by an explicit RateLimitInfo.resets_at
# (e.g. a dropped connection, a 5xx from the API, transient CLI crash). Capped well below the
# quota backoff below since these are usually transient.
_RECONNECT_BACKOFF_INITIAL_S = 30
_RECONNECT_BACKOFF_MAX_S = 900

# Anthropic's stated rate-limit windows top out at 7 days; cap how long we'll sleep on a single
# resets_at read in case of a bogus/far-future timestamp, and re-check state after waking rather
# than trusting one read for arbitrarily long.
_MAX_SINGLE_SLEEP_S = 3600

# Idle-nudge backoff: when the model ends its turn with no tool use at all (nothing to react to
# -- no job polled, no API touched), doubling the pre-nudge sleep avoids burning a full model turn
# every ~15-20s once an agent has converged and is just repeating "nothing changed, holding
# steady" (observed costing real tokens over an 8h run once an agent finishes early -- see
# improvements.md). Any turn that does use a tool resets the backoff immediately, since that's a
# real reaction to new state, not idle polling.
_IDLE_NUDGE_BACKOFF_INITIAL_S = 15
_IDLE_NUDGE_BACKOFF_MAX_S = 900


async def _run_one_session(setup: core.RunSetup, model: str, resume_id: str | None,
                            started_at: float, stop_flag: list) -> tuple[str | None, float | None]:
    """Runs one ClaudeSDKClient connection until it ends (real stop, error, or rate limit).

    Returns (updated resume_id, rate_limit resets_at unix seconds or None). Raising means an
    unexpected error the caller should back off and retry on; returning normally with
    resets_at=None and the stop condition true means a real stop.
    """
    options = ClaudeAgentOptions(
        system_prompt=setup.system_prompt,
        model=model,
        permission_mode="bypassPermissions",
        # No max_turns cap: this session is meant to run for the platform experiment's whole
        # lifetime (hours to days), not a bounded number of turns. The only real stop conditions
        # are MAX_WALL_HOURS, a signal, or the platform experiment closing — see stop_reason().
        max_turns=None,
        cwd=setup.workdir,
        resume=resume_id,
        # This process must only ever expose the standard Claude Code tool set — nothing pulled in
        # from whatever machine happens to run it, so a run is reproducible regardless of host.
        # Without these two, the bundled CLI still loads
        # user-level ~/.claude/settings.json and any configured MCP servers, which on a dev
        # machine that also runs interactive Claude Code sessions silently hands the agent extra
        # tools (Monitor, ScheduleWakeup, TaskCreate, ...) that only work inside a resumable
        # interactive session — a detached one-shot subprocess calling them just hangs forever.
        setting_sources=[],
        strict_mcp_config=True,
        disallowed_tools=DISALLOWED_TOOLS,
    )

    rate_limited_resets_at: float | None = None

    async with ClaudeSDKClient(options=options) as client:
        first_prompt = "Continue where you left off." if resume_id else "Begin."
        await client.query(first_prompt)
        idle_nudge_backoff_s = _IDLE_NUDGE_BACKOFF_INITIAL_S
        while True:
            got_result = False
            turn_is_error = False
            turn_used_tool = False
            async for message in client.receive_response():
                if _log_message(message):
                    turn_used_tool = True
                if isinstance(message, RateLimitEvent) and message.rate_limit_info.status == "rejected":
                    rate_limited_resets_at = message.rate_limit_info.resets_at
                if isinstance(message, ResultMessage):
                    got_result = True
                    turn_is_error = message.is_error
                    session_id = getattr(message, "session_id", None) or resume_id
                    _save_resume_id(setup, session_id)
                    resume_id = session_id or resume_id

            if rate_limited_resets_at:
                log.info("stopping session: rate-limited, resets_at=%s", rate_limited_resets_at)
                return resume_id, rate_limited_resets_at

            if not got_result:
                # The stream ended without a ResultMessage — typically the CLI process itself
                # died mid-turn (e.g. an authentication or usage-limit error the SDK couldn't
                # turn into a clean ResultMessage). Treat as a transient error to retry, not a
                # reason to stop the whole agent: a quota outage must not end the run, it must
                # pause it.
                log.warning("stream ended without a ResultMessage — treating as transient")
                return resume_id, None

            if turn_is_error:
                # is_error=True with subtype="success" is the CLI's signal for an underlying
                # failure it couldn't otherwise surface (auth not set up, a bad API response,
                # ...) — e.g. "Not logged in · Please run /login". The turn still "completes"
                # instantly with no tool use, so falling through to the normal continue-nudge
                # path spins this loop as fast as the CLI can be invoked, forever. Route it
                # through the same backoff as a transient error instead of re-querying immediately.
                log.warning("turn completed with an underlying error (is_error) — treating as transient")
                return resume_id, None

            reason = core.stop_reason(setup, started_at, stop_flag)
            if reason:
                log.info("stopping: %s", reason)
                return resume_id, None

            # The model ended its turn but no real stop condition holds — nudge it forward on
            # the same connection instead of letting the process exit. This is what actually
            # lets a single session span hours/days: every other "stop" here was actually just
            # the model treating an ordinary pause as the end of the whole run.
            if turn_used_tool:
                idle_nudge_backoff_s = _IDLE_NUDGE_BACKOFF_INITIAL_S
            else:
                log.info("turn ended with no tool use — idle, sleeping %.0fs before nudging", idle_nudge_backoff_s)
                await asyncio.sleep(idle_nudge_backoff_s)
                idle_nudge_backoff_s = min(idle_nudge_backoff_s * 2, _IDLE_NUDGE_BACKOFF_MAX_S)
            log.info("turn ended, no stop condition met — continuing session")
            await client.query(CONTINUE_PROMPT)


async def _run(setup: core.RunSetup, model: str) -> None:
    resume_id = _load_resume_id(setup)
    started_at = time.time()
    stop_flag = core.install_signal_handler()
    backoff_s = _RECONNECT_BACKOFF_INITIAL_S

    # Outer loop: keeps the agent alive across quota exhaustion, transient errors, and dropped
    # connections for the whole platform-experiment lifetime — a "no budget right now" condition
    # must pause and resume, never permanently end the run, since running out of quota is an
    # everyday occurrence on a multi-day competition, not an exceptional one.
    while True:
        reason = core.stop_reason(setup, started_at, stop_flag)
        if reason:
            log.info("stopping: %s", reason)
            return

        try:
            resume_id, resets_at = await _run_one_session(setup, model, resume_id, started_at, stop_flag)
        except Exception:
            log.exception("session ended with an unexpected error — will retry after backoff")
            resets_at = None
        else:
            if resets_at is None:
                # Clean stop (real stop condition, or a transient stream-ended-early case already
                # logged inside _run_one_session) — re-check stop_reason at the top of the loop.
                if core.stop_reason(setup, started_at, stop_flag):
                    return
                # Fall through to a short backoff before reconnecting (transient case).

        if resets_at:
            sleep_s = max(1.0, min(resets_at - time.time(), _MAX_SINGLE_SLEEP_S))
            log.info("quota/rate limit hit — sleeping %.0fs before resuming session %s", sleep_s, resume_id)
            await asyncio.sleep(sleep_s)
            backoff_s = _RECONNECT_BACKOFF_INITIAL_S  # a real rate-limit wait resets the backoff
        else:
            log.info("reconnecting in %ss (session %s)", backoff_s, resume_id)
            await asyncio.sleep(backoff_s)
            backoff_s = min(backoff_s * 2, _RECONNECT_BACKOFF_MAX_S)


def main() -> None:
    setup = core.prepare()
    model = os.environ.get("ANTHROPIC_MODEL", "claude-sonnet-5")
    log.info(
        "llm_agent %s starting (claude): pe=%s model=%s",
        setup.cfg.agent_id, setup.cfg.platform_experiment_id, model,
    )
    asyncio.run(_run(setup, model))
    log.info("llm_agent %s stopped", setup.cfg.agent_id)


if __name__ == "__main__":
    main()
