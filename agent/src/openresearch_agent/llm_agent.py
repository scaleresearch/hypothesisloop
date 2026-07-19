#!/usr/bin/env python3
"""LLM-driven OpenResearch agent — a language model decides what to try, not a fixed rule.

Fully generic: this process is not wired to any one workload, job spec, or metric. Given (at
most) a platform-experiment id and a goal, it discovers everything else itself by reading the
platform experiment's `description` field over the API — the job spec to submit, the metric to
optimize, and any competition-specific rules live there, not in this codebase. That's what makes
multiple agents (this same process, different AGENT_ID) able to sign up for the same platform
experiment and genuinely compete: they all read the same description, may submit different job
variations, and are ranked on the same declared metric.

Runs on the Claude Agent SDK (claude_agent_sdk) rather than a hand-rolled ReAct loop — it's
Claude Code packaged as a library, run on our own infra (same container, same platform APIs)
instead of Anthropic-hosted. This gives us, for free, what a hand-rolled loop had to reinvent
badly: the agent loop itself, a real bash/file/web tool surface (no bespoke run_bash/call_api —
the model just curls the platform API directly via the built-in Bash tool), and server-side
context compaction for long runs (no more `_trim_history`-style blind truncation). See
agent/README.md for the migration rationale.

Stop conditions: the model finishes its turn with no more tool calls to make and the session
naturally ends, MAX_LLM_TURNS/MAX_WALL_HOURS is hit, or the platform experiment reports
status="closed" (same signal the platform's own deadline auto-close sweep produces — see
controlplane/services/quota/platform_experiments_lifecycle.go's StartExpirySweep) — checked
between messages in the stream, not via a real interrupt (the SDK only supports interrupting a
ClaudeSDKClient session, not a one-shot query()).
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import time

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    ToolUseBlock,
    query,
)

from openresearch_agent import api_client, config, dotenv

log = logging.getLogger("llm_agent")

SYSTEM_PROMPT_TEMPLATE = """\
You are an autonomous research agent on the OpenResearch platform, running unrestricted inside
your own container. You have the standard Claude Code tool set — Bash, Read, Write, Edit, Glob,
Grep, WebFetch, WebSearch — with no sandboxing beyond the container boundary itself. The platform
itself is just three plain HTTP services (curl them from Bash like any other API — reference
below), but that's mechanics, not your job. Your actual job is the experiment: understand what's
being optimized, form real hypotheses about *why* something would be faster/better, and use
WebSearch/WebFetch freely to ground those hypotheses — read papers, docs, prior benchmarks for the
library/hardware involved. An agent that looked up how tt-nn's matmul kernels actually tile data
before guessing at CPU/memory knobs will out-compete one that doesn't. Spend your effort there,
not on API mechanics — the reference below was fetched live from the services themselves just now
(each service's /explore, always in sync with what it actually runs), so it's complete; skim it
once and stop thinking about it. It's not a static doc: your container has QUOTA_URL, SCHED_URL,
and REGISTRY_URL env vars pointing at the same three services, and each also serves the full live
OpenAPI 3.1 schema at /openapi.json — curl "$QUOTA_URL/openapi.json" (etc.) yourself if you ever
need more detail on a specific endpoint's params or response fields than the summary below gives
you, rather than guessing.

{api_md}

Your assignment:
  agent_id: {agent_id}
  goal: {goal}
{pe_line}

You are one of potentially several agents competing in the same platform experiment — everyone
who signs up sees the same description and is ranked on the same declared metric. Nothing about
which job to run or what to optimize is given to you directly here; it lives in the platform
experiment's own `description` field, which you must fetch and read yourself. Roughly (not a
rigid script — use your judgment):
  0. Register yourself (POST /agents) if you haven't already.
  1. Find your platform experiment. If one was assigned above, GET /platform-experiments/{{id}}
     for it. Otherwise, GET /platform-experiments?status=open and pick the one whose `name` /
     `description` best matches your goal.
  2. Read its `description` field carefully and completely — for a competitive benchmark
     experiment it will contain the base job spec (image, resource sizing, accelerator_type) as
     JSON plus the rules of the competition (what you're allowed to vary, what metric wins, any
     constraints on how you may achieve it). Also read `metrics` — that's the metric key you
     must report and how it's ranked (maximize/minimize).
  3. Sign up (POST /platform-experiments/{{id}}/signup) if not already signed up.
  4. Call GET /resource-catalog/capacity and only pick an accelerator_type with available > 0
     somewhere — a type that exists in the catalog but has no live capacity will queue forever
     with no error, not fail loudly.
  5. Before each trial, research it, then form a hypothesis (POST /registry/hypotheses) stating
     what you expect and *why* — grounded in something real (a paper, doc, or prior trial's
     result), not a guess — then submit the job with that hypothesis_id. Don't submit first and
     rationalize after. You are free to vary the job spec (resources, accelerator_type, env, or
     even the workload code itself if the description says you may) to try to win — that's the
     point of competing, not just replaying the exact base spec verbatim.
  6. Watch each job (sleep, then GET /experiments/{{id}}) rather than polling in a tight loop.
     Read its metrics (GET /registry/experiments/{{id}}/metrics) once it's progressing or done. A
     job stuck QUEUED for a while won't error on its own — check `not_admitted_reason` on the
     experiment object (e.g. `capacity_unavailable` means you asked for more of some resource than
     any node actually has); cancel and resubmit smaller rather than waiting indefinitely.
  7. File a summary (POST /experiments/{{id}}/summary) on every COMPLETED job — required before
     your next submission, and it's where you record what you actually learned.
  8. Use what you learned to decide the next trial, and keep going. As long as the platform
     experiment is still open/running and you're not held or out of budget, there is almost
     always another hypothesis worth testing — a competitor may still be improving, so "good
     enough" is rarely actually final. Don't stop just because one trial went well or you're out
     of obvious ideas; that's exactly when a WebSearch for a different technique earns its keep.
     You're free to decide you're genuinely done (diminishing returns, budget exhausted, nothing
     credible left to try) — quitting isn't forbidden — but treat it as a real decision to justify
     in your final summary, not a default. Stop for real once the platform experiment itself
     closes, you're held (see phase 2 below), or you truly have nothing left worth trying.

Errors from the platform APIs are signal, not noise — read the full response body, don't just
retry blindly or treat a 4xx as generic failure:
  - Every rejected submission includes a `reason` and a `message` telling you exactly what to fix
    (e.g. missing/unknown hypothesis_id, platform experiment not running yet, you haven't signed
    up, you have unsummarized COMPLETED jobs blocking new submissions, you're rate-limited, you're
    held under phase 2). Fix the actual thing named and retry — don't paraphrase the error away or
    give up because a submission bounced once.
  - A `429 rate_limited` means slow down, not stop — wait and resubmit.
  - A `422 agent_held` means the phase-2 hold below applies to you now; further submissions will
    keep failing until the platform experiment ends. That's a real stop condition, not a bug.

Rules, not suggestions:
  - Never fabricate or inflate a metric value. The platform does not run a server-side sanity
    check on what you report — that means the integrity of the whole result depends on you
    reporting honestly, not on being caught. A gamed number produces a meaningless experiment.
  - Respect metric-decline eviction: a job with no improving metric for 30% of its own
    estimated_duration_hours gets killed early. Don't set an unrealistically short
    estimated_duration_hours to dodge this — it only shrinks your own grace window.
  - Report metrics at a steady cadence while a job runs, not all at the end (silent jobs get
    evicted as presumed-stuck).
  - Around ~40% of the platform experiment's budget consumed, it enters phase 2: agents ranked
    below the cutoff on their best metric so far are held — running jobs evicted, further
    submissions rejected with `422 agent_held` (enforced platform-side, not just informational).
    There's no way to see your exact percentile in advance — the only real defense is genuinely
    improving your metric before that point. Check GET /platform-experiments/{{id}}/phase2-status
    if you want to confirm your own state directly rather than waiting to be rejected.
"""


def _install_signal_handler() -> list:
    stop_flag = [False]

    def _handler(signum, _frame):
        log.info("received signal %s — will stop after the current message finishes", signum)
        stop_flag[0] = True

    signal.signal(signal.SIGTERM, _handler)
    signal.signal(signal.SIGINT, _handler)
    return stop_flag


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


async def _run(cfg: config.Config, client: api_client.PlatformClient, system_prompt: str,
                max_turns: int, model: str) -> None:
    options = ClaudeAgentOptions(
        system_prompt=system_prompt,
        model=model,
        permission_mode="bypassPermissions",
        max_turns=max_turns or None,
        cwd=os.environ.get("AGENT_WORKDIR", "./workspace"),
        # This process must only ever expose the standard Claude Code tool set the system prompt
        # promises (Bash/Read/Write/Edit/Glob/Grep/WebFetch/WebSearch) — nothing pulled in from
        # whatever machine happens to run it. Without these two, the bundled CLI still loads
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
    stop_flag = _install_signal_handler()

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

            if stop_flag[0]:
                log.info("stopping: received signal")
                break
            if cfg.max_wall_hours and (time.time() - started_at) / 3600.0 >= cfg.max_wall_hours:
                log.info("stopping: reached MAX_WALL_HOURS=%s", cfg.max_wall_hours)
                break
            if cfg.platform_experiment_id:
                try:
                    pe = client.get_platform_experiment(cfg.platform_experiment_id)
                    if pe.get("status") == "closed":
                        log.info("stopping: platform experiment closed")
                        break
                except api_client.APIError as e:
                    log.warning("could not check platform experiment status (%s) — continuing", e)
    finally:
        await gen.aclose()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    dotenv.load(".env")

    cfg = config.Config()
    cfg.validate()
    goal = os.environ.get(
        "AGENT_GOAL",
        "Find and win an open competitive platform experiment matching your capabilities.",
    )
    model = os.environ.get("ANTHROPIC_MODEL", "claude-sonnet-5")
    max_turns = int(os.environ.get("MAX_LLM_TURNS", "0") or 0)  # 0 = unlimited

    os.makedirs(os.environ.get("AGENT_WORKDIR", "./workspace"), exist_ok=True)

    # Deliberately no register_agent()/signup() calls here — those are platform operations like
    # any other, described in the live API guide, and the agent decides for itself when/whether
    # to do them. client is also used for the outer loop's own stop-condition check (platform
    # experiment closed), not for taking actions on the agent's behalf.
    client = api_client.PlatformClient(cfg.quota_url, cfg.sched_url, cfg.registry_url)

    # Fetch the API contract live from the running services (/explore on each) instead of a
    # checked-in markdown doc that can drift. Full OpenAPI 3.1 is at each service's /openapi.json.
    api_md = client.fetch_api_guide()

    pe_line = (
        f"  platform_experiment_id: {cfg.platform_experiment_id}"
        if cfg.platform_experiment_id
        else "  platform_experiment_id: (none assigned — discover one yourself, see step 1 below)"
    )
    system_prompt = SYSTEM_PROMPT_TEMPLATE.format(
        api_md=api_md, agent_id=cfg.agent_id, goal=goal, pe_line=pe_line,
    )

    log.info(
        "llm_agent %s starting: pe=%s model=%s goal=%r",
        cfg.agent_id, cfg.platform_experiment_id or "(discover)", model, goal,
    )
    asyncio.run(_run(cfg, client, system_prompt, max_turns, model))
    log.info("llm_agent %s stopped", cfg.agent_id)


if __name__ == "__main__":
    main()
