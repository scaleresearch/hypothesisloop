from __future__ import annotations

import json
import re
from datetime import datetime, timezone

from claude_agent_sdk import (
    AssistantMessage,
    RateLimitEvent,
    SystemMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from hypothesisloop_agent import updates

WIDTH = 200
_TOOL_INPUT_KEYS = ("command", "file_path", "pattern", "url", "query", "description")
# A model call that answers later than this is said: one took 397 s for 669 tokens and another
# 649 s for 548, each a single request, and showed only as silence.
SLOW_CALL_S = 60


def _one_line(text: str) -> str:
    text = " ⏎ ".join(" ".join(line.split()) for line in text.splitlines() if line.strip())
    return text if len(text) <= WIDTH else text[:WIDTH - 3] + "..."


def _tool_summary(block: ToolUseBlock) -> str:
    for key in _TOOL_INPUT_KEYS:
        if isinstance(block.input.get(key), str):
            return f"{block.name}: {block.input[key]}"
    return f"{block.name}: {json.dumps(block.input, default=str)}"


def _error_summary(block: ToolResultBlock) -> str:
    content = block.content
    if isinstance(content, list):
        content = "\n".join(part.get("text", "") for part in content if isinstance(part, dict))
    lines = [line for line in re.sub(r"</?tool_use_error>", "", content or "").splitlines() if line.strip()]
    prefix = ""
    if lines and (code := re.fullmatch(r"Exit code (\d+)", lines[0].strip())):
        prefix, lines = f"exit {code[1]}: ", lines[1:]
    if lines and lines[0].strip() == "Traceback (most recent call last):":
        lines = lines[-1:]
    return prefix + lines[0] if lines else prefix or "(no output)"


def _stamp(now: datetime | None) -> str:
    return (now or datetime.now(timezone.utc)).strftime("%Y-%m-%dT%H:%M:%SZ")


def event_lines(events: list[dict], agent: str, now: datetime | None = None) -> list[str]:
    """One line per platform update the harness handed the agent."""
    return [f"{_stamp(now)} {agent} event {_one_line(updates.event_line(e, agent))}" for e in events]


def _retry(data: dict) -> str:
    return (f"model call failed ({data.get('error_status') or 'no status'} {data.get('error')}): "
            f"attempt {data.get('attempt')} of {data.get('max_retries')} in {(data.get('retry_delay_ms') or 0) / 1000:.0f} s")


def _limit(info) -> str:
    resets = f", resets {_stamp(datetime.fromtimestamp(info.resets_at, timezone.utc))}" if info.resets_at else ""
    return f"rate limit {info.status} ({info.rate_limit_type}){resets}"


def lines(message, agent: str, now: datetime | None = None, waited: float = 0.0) -> list[str]:
    """One human-readable line per meaningful action in a Claude stream message, and one for
    what delayed it: a model call the CLI retries, a rate limit, an answer awaited for waited
    seconds."""
    stamp = _stamp(now)
    entries = []
    if isinstance(message, SystemMessage) and message.subtype == "api_retry":
        entries.append(("retry", _retry(message.data)))
    elif isinstance(message, RateLimitEvent) and message.rate_limit_info.status != "allowed":
        entries.append(("limit", _limit(message.rate_limit_info)))
    elif isinstance(message, AssistantMessage):
        if waited >= SLOW_CALL_S:
            entries.append(("slow", f"model call answered after {waited:.0f} s with {(message.usage or {}).get('output_tokens')} output tokens"))
        for block in message.content:
            if isinstance(block, TextBlock) and block.text.strip():
                entries.append(("say", block.text))
            elif isinstance(block, ToolUseBlock):
                entries.append(("tool", _tool_summary(block)))
    elif isinstance(message, UserMessage) and isinstance(message.content, list):
        for block in message.content:
            if isinstance(block, ToolResultBlock) and block.is_error:
                entries.append(("error", _error_summary(block)))
    return [f"{stamp} {agent} {kind:<5} {_one_line(text)}" for kind, text in entries]
