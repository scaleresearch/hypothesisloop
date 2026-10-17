#!/usr/bin/env python3
"""The Claude entry point: the shared agent (core.py) on one ClaudeSDKClient session.

A turn that ends waits for the next platform updates (updates.py) and sends them on the same
connection, and a hook hands the agent the ones that arrive while it works. The session id is
kept on disk and the session is stored with the platform, so a restarted process or a new
machine resumes the conversation.
"""
from __future__ import annotations

import asyncio
import logging
import time

from hypothesisloop_agent import core
from hypothesisloop_agent.claude import connection, stream, turns
from hypothesisloop_agent.claude.running import Running
from hypothesisloop_agent.claude.store import Store

log = logging.getLogger("llm_agent")

# Backoff for reconnect attempts that aren't governed by an explicit RateLimitInfo.resets_at
# (e.g. a dropped connection, a 5xx from the API, transient CLI crash). Capped well below the
# quota backoff below since these are usually transient.
_RECONNECT_BACKOFF_INITIAL_S = 30
_RECONNECT_BACKOFF_MAX_S = 900

# An agent the model provider refuses waits until the reset time the provider gave, and its
# windows top out at 7 days. Each attempt before that would be one more refused turn in the session.
_MAX_LIMIT_WAIT_S = 7 * 86400


async def _pause(seconds: float, stop_flag: list) -> None:
    """Sleeps for seconds, or until a stop: a machine told to stop exits within a second."""
    deadline = time.monotonic() + seconds
    while not stop_flag[0] and (left := deadline - time.monotonic()) > 0:
        await asyncio.sleep(min(left, 1.0))


async def _wait_out_limit(resets_at: float, stop_flag: list) -> None:
    """Waits until the model provider's limit resets, saying so as what the agent is doing. The
    process stays up and its progress beats on, so the wait spends none of its restarts."""
    stream.refused_from_now()
    wait_s = max(1.0, min(resets_at - time.time(), _MAX_LIMIT_WAIT_S))
    kind = stream.limit_kind()
    until = time.strftime("%Y-%m-%d %H:%M", time.gmtime(resets_at))
    log.warning("the model provider refuses calls (%s limit): waiting %.0fs until it resets at %s UTC", kind, wait_s, until)
    stream.progress.doing(f"paused until {until} UTC: the model provider's {kind} limit is reached")
    await _pause(wait_s, stop_flag)


async def _run(setup: core.RunSetup, store: Store) -> None:
    stop_flag = core.install_signal_handler()
    beat = asyncio.ensure_future(stream.progress.run())
    storing = asyncio.ensure_future(store.keep_saved())
    try:
        await core.run_until_lost(setup, _run_until_stopped(setup, store, stop_flag), stop_flag)
    finally:
        beat.cancel()
        storing.cancel()


async def _run_until_stopped(setup: core.RunSetup, store: Store, stop_flag: list) -> None:
    backoff_s = _RECONNECT_BACKOFF_INITIAL_S
    await core.until_accepted(setup, stop_flag, said=lambda: stream.progress.doing("waiting for the plan to be accepted"))
    running = Running(setup)

    def reset_backoff() -> None:
        nonlocal backoff_s
        backoff_s = _RECONNECT_BACKOFF_INITIAL_S

    # Outer loop: keeps the agent alive across quota exhaustion, transient errors, and dropped
    # connections for the whole platform-experiment lifetime — a "no budget right now" condition
    # must pause and resume, never permanently end the run, since running out of quota is an
    # everyday occurrence on a multi-day competition, not an exceptional one.
    while True:
        reason = await core.stop_reason(setup, stop_flag)
        if reason:
            log.info("stopping: %s", reason)
            return

        resume_id, initial_prompt = store.restart_state()
        try:
            rotated = await connection.run(setup, store, running, resume_id, stop_flag, initial_prompt, reset_backoff)
        except Exception as e:
            if not turns.transport_error(e):
                raise
            log.warning("session lost its connection to the CLI (%s): reconnecting after backoff", e)
            rotated = False
        stream.progress.subagents(0)
        # The CLI has exited here and written all of its transcript. The store at the end of a
        # turn can come before the CLI wrote the turn's last reply, and after a planner's
        # answer or an agent's finish no turn follows to store it.
        await store.save()
        store.delete_retired()
        if rotated or await core.stop_reason(setup, stop_flag):
            continue

        # Whatever ended the session, a refused account is waited out until its stated reset.
        if refused_until := stream.refused_until():
            await _wait_out_limit(refused_until, stop_flag)
            reset_backoff()
        else:
            log.info("reconnecting in %ss", backoff_s)
            await _pause(backoff_s, stop_flag)
            backoff_s = min(backoff_s * 2, _RECONNECT_BACKOFF_MAX_S)


async def _main() -> None:
    started = time.monotonic()
    setup = await core.prepare()
    try:
        store = Store(setup)
        await store.restore()
        # With the line the CLI connects with, this says where a slow start went: what precedes
        # both is the machine, its image and the interpreter.
        log.info(
            "llm_agent %s starting (claude): pe=%s role=%s, prepared in %.1fs",
            setup.cfg.agent_id, setup.cfg.platform_experiment_id, setup.cfg.role, time.monotonic() - started,
        )
        await _run(setup, store)
        log.info("llm_agent %s stopped", setup.cfg.agent_id)
    finally:
        await setup.client.aclose()


def main() -> None:
    asyncio.run(_main())


if __name__ == "__main__":
    main()
