#!/usr/bin/env python3
"""Anthropic entry point — the shared agent (see core.py) driven by the Claude Agent SDK.

Runs on claude_agent_sdk rather than a hand-rolled ReAct loop — it's Claude Code packaged as a
library, run on our own infra (same container, same platform APIs) instead of Anthropic-hosted.
This gives us, for free, what a hand-rolled loop had to reinvent badly: the agent loop itself, a
real bash/file/web tool surface (no bespoke run_bash/call_api — the model just curls the platform
API directly via the built-in Bash tool), and server-side context compaction for long runs.
Auth: Claude subscription (Claude Code login) or
ANTHROPIC_API_KEY, exactly as Claude Code itself resolves it.

Stop conditions are checked between messages in the stream, not via a real interrupt (the SDK
only supports interrupting a ClaudeSDKClient session, not a one-shot query()).
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
    ResultMessage,
    TextBlock,
    ToolUseBlock,
    query,
)

from hypothesisloop_agent import core

log = logging.getLogger("llm_agent")


def _log_message(message) -> None:
    if isinstance(message, AssistantMessage):
        for block in message.content:
            if isinstance(block, TextBlock) and block.text.strip():
                log.info("assistant: %s", block.text.strip()[:800])
            elif isinstance(block, ToolUseBlock):
                log.info("tool_use: %s(%s)", block.name, json.dumps(block.input, default=str)[:400])
    elif isinstance(message, ResultMessage):
        log.info(
            "result: subtype=%s turns=%d cost_usd=%s stop_reason=%s",
            message.subtype, message.num_turns, message.total_cost_usd, message.stop_reason,
        )


async def _run(setup: core.RunSetup, model: str) -> None:
    options = ClaudeAgentOptions(
        system_prompt=setup.system_prompt,
        model=model,
        permission_mode="bypassPermissions",
        max_turns=setup.max_turns or None,
        cwd=setup.workdir,
        # This process must only ever expose the standard Claude Code tool set — nothing pulled in
        # from whatever machine happens to run it, so a run is reproducible regardless of host.
        # Without these two, the bundled CLI still loads
        # user-level ~/.claude/settings.json and any configured MCP servers, which on a dev
        # machine that also runs interactive Claude Code sessions silently hands the agent extra
        # tools (Monitor, ScheduleWakeup, TaskCreate, ...) that only work inside a resumable
        # interactive session — a detached one-shot subprocess calling them just hangs forever.
        # Confirmed live: an agent called Monitor/ScheduleWakeup expecting a resumable session and
        # never got un-stuck. setting_sources=[] skips user/project/local settings entirely;
        # strict_mcp_config=True (with no mcp_servers passed) ignores any configured MCP servers.
        setting_sources=[],
        strict_mcp_config=True,
    )
    started_at = time.time()
    stop_flag = core.install_signal_handler()

    gen = query(prompt="Begin.", options=options)
    try:
        async for message in gen:
            _log_message(message)

            # A ResultMessage always marks the end of the session, whatever its subtype —
            # advancing the generator again after one raises (confirmed live, including on a
            # plain subtype="success" result), so this must be an unconditional stop.
            if isinstance(message, ResultMessage):
                log.info("stopping: session ended (%s)", message.subtype)
                break

            reason = core.stop_reason(setup, started_at, stop_flag)
            if reason:
                log.info("stopping: %s", reason)
                break
    finally:
        await gen.aclose()


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
