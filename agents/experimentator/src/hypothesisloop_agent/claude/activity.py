from __future__ import annotations

import json
import os
import re
import shlex
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

from hypothesisloop_agent import progress, updates

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


# What the platform shows of an agent beside its machine is built from these alone: a command's
# name, the words of an hl command, an id and a file's name. Nothing else of a tool call is safe to
# show a whole organisation: a command line or a path may hold a credential, and a file its data.
_WORD = re.compile(r"[a-z][a-z-]{0,30}")
# An id as the platform issues one (domain.NewID): a short prefix and 32 hex digits.
_ID = re.compile(r"[a-z]{2,8}-[0-9a-f]{32}")
_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,63}")
_PROGRAM = re.compile(r"[a-z0-9][a-z0-9.+-]{0,23}")
_SCRIPT = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,60}\.(?:sh|py)")
_ASSIGNMENT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=")
_SEPARATOR = re.compile(r"[();<>|&]+")
# Commands that only set the scene for the one after them.
_PREAMBLE = ("cd", "export", "set", "source", ".", "mkdir", "sleep", "timeout", "time", "env", "nohup")
_FILE_TOOLS = {"Read": "reading", "NotebookRead": "reading", "Write": "writing", "Edit": "editing", "MultiEdit": "editing",
               "NotebookEdit": "editing"}
_OTHER_TOOLS = {"Grep": "searching the repository", "Glob": "searching the repository", "WebSearch": "searching the web",
                "WebFetch": "reading a web page", "Task": "working with a subagent", "Agent": "working with a subagent",
                "TodoWrite": "planning its next steps"}


def _file(path) -> str:
    name = os.path.basename(path) if isinstance(path, str) else ""
    return name if _NAME.fullmatch(name) else "a file"


def _simple_commands(command: str) -> list[list[str]]:
    """The simple commands of a shell line, each as its words without leading assignments."""
    lexer = shlex.shlex(command, posix=True, punctuation_chars=True)
    lexer.whitespace_split = True
    try:
        words = list(lexer)
    except ValueError:
        words = command.split()
    commands, current = [], []
    for word in [*words, ";"]:
        if not _SEPARATOR.fullmatch(word):
            current.append(word)
            continue
        while current and _ASSIGNMENT.match(current[0]):
            current = current[1:]
        if current:
            commands.append(current)
        current = []
    return commands


def _command(command: str) -> str:
    commands = _simple_commands(command)
    if not commands:
        return "running a command"
    named = [(os.path.basename(words[0]), words[1:]) for words in commands]
    program, arguments = next((c for c in named if c[0] == "hl"), None) or next((c for c in named if c[0] not in _PREAMBLE), named[0])
    if not _PROGRAM.fullmatch(program) and not _SCRIPT.fullmatch(program):
        return "running a command"
    said = [program]
    if program == "hl":
        for argument in arguments:
            if len(said) < 4 and _WORD.fullmatch(argument) and not any(_ID.fullmatch(word) for word in said):
                said.append(argument)
            elif len(said) < 6 and _ID.fullmatch(argument):
                said.append(argument)
            else:
                break
    elif program == "git" and arguments and _WORD.fullmatch(arguments[0]):
        said.append(arguments[0])
    elif program.startswith("python") and arguments and arguments[0].endswith(".py"):
        said.append(_file(arguments[0]))
    return "running " + " ".join(said)


def _tool(block: ToolUseBlock) -> str:
    if block.name == "Bash":
        return _command(block.input.get("command") or "") if isinstance(block.input.get("command"), str) else "running a command"
    if block.name in _FILE_TOOLS:
        return f"{_FILE_TOOLS[block.name]} {_file(block.input.get('file_path') or block.input.get('notebook_path'))}"
    if block.name in _OTHER_TOOLS:
        return _OTHER_TOOLS[block.name]
    return f"using {block.name}" if _NAME.fullmatch(block.name) else "using a tool"


def doing(message) -> str:
    """What the agent is doing once message of its Claude stream has arrived, as the platform shows
    it beside the agent, "" when the message changes nothing. It is the one place that says it:
    the tool call in flight, and thinking again once a call has answered. A subagent's messages
    change nothing: its call is what the agent is doing."""
    if getattr(message, "parent_tool_use_id", None):
        return ""
    if isinstance(message, AssistantMessage):
        calls = [block for block in message.content if isinstance(block, ToolUseBlock)]
        return progress.sentence(_tool(calls[-1])) if calls else ""
    if isinstance(message, UserMessage) and isinstance(message.content, list) and any(isinstance(block, ToolResultBlock) for block in message.content):
        return progress.THINKING
    return ""
