#!/usr/bin/env python3
"""OpenAI entry point — the shared agent (see core.py) driven by the official Codex Python SDK.

Runs on openai_codex (the Codex CLI packaged as a library, talking JSON-RPC to the bundled
app-server binary) — the OpenAI-side mirror of what claude/run.py gets from claude_agent_sdk:
the agent loop, Codex's native shell/file/web tool surface, and Codex-side context compaction
for long runs. Auth: ChatGPT subscription (run `codex login` once in the container/image, or
use login_chatgpt_device_code() for headless) or an OpenAI API key.

Backend-specific choices, mirroring the Claude entry point's bypassPermissions setup:
  - ApprovalMode.deny_all: never pause to ask a human for approval — anything the sandbox
    policy wouldn't allow is simply denied, keeping the run fully autonomous.
  - Sandbox.full_access: no filesystem sandboxing beyond the container boundary itself, same
    trust model as permission_mode="bypassPermissions" on the Claude side.
  - The shared system prompt is passed as developer_instructions (additive), not
    base_instructions (which would replace Codex's own harness prompt and degrade its tool use).
  - MAX_LLM_TURNS has no Codex equivalent (one thread.turn() is one long agentic turn, not a
    countable message budget) and is ignored here; MAX_WALL_HOURS is the portable limit.

Stop conditions are checked between streamed notifications; unlike the Claude one-shot query(),
Codex turns support a real interrupt(), which we use.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import time

from openai_codex import ApprovalMode, AsyncCodex, Sandbox

from hypothesisloop_agent import core

log = logging.getLogger("llm_agent")


def _log_notification(note) -> None:
    # Notifications are generated protocol models that vary by SDK version — log the type name
    # plus a compact payload rather than depending on any one model's fields.
    try:
        payload = json.dumps(note.model_dump(mode="json", exclude_none=True), default=str)[:400]
    except Exception:
        payload = repr(note)[:400]
    log.info("codex: %s %s", type(note).__name__, payload)


async def _run(setup: core.RunSetup, model: str) -> None:
    started_at = time.time()
    stop_flag = core.install_signal_handler()

    async with AsyncCodex() as codex:
        thread = await codex.thread_start(
            model=model or None,
            approval_mode=ApprovalMode.deny_all,
            sandbox=Sandbox.full_access,
            developer_instructions=setup.system_prompt,
            cwd=os.path.abspath(setup.workdir),
        )
        turn = await thread.turn("Begin.")
        interrupted = False
        async for note in turn.stream():
            _log_notification(note)
            reason = core.stop_reason(setup, started_at, stop_flag)
            if reason:
                log.info("stopping: %s", reason)
                await turn.interrupt()
                interrupted = True
                break

        result = await turn.run()  # collect the (possibly interrupted) turn's final result
        log.info(
            "result: status=%s duration_ms=%s error=%s%s",
            result.status, result.duration_ms, result.error,
            " (interrupted)" if interrupted else "",
        )
        if result.final_response:
            log.info("assistant: %s", result.final_response.strip()[:800])


def main() -> None:
    setup = core.prepare()
    model = os.environ.get("CODEX_MODEL", "gpt-5.4")
    log.info(
        "llm_agent %s starting (codex): pe=%s model=%s",
        setup.cfg.agent_id, setup.cfg.platform_experiment_id, model,
    )
    asyncio.run(_run(setup, model))
    log.info("llm_agent %s stopped", setup.cfg.agent_id)


if __name__ == "__main__":
    main()
