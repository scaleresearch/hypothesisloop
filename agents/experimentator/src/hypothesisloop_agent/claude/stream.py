"""What the harness observes of the CLI's stream of messages, and says of it: the activity lines,
the agent's progress, the model provider's rate limit and the tasks the CLI runs in the background."""
from __future__ import annotations

import logging
import time

from claude_agent_sdk import RateLimitEvent, ResultMessage, SystemMessage

from hypothesisloop_agent.claude import activity
from hypothesisloop_agent.progress import Progress

log = logging.getLogger("llm_agent")

# Longest silence a healthy turn can produce between two CLI messages, a model call's wait for its
# first stream event included: Claude Code caps one Bash call at 10 min (BASH_MAX_TIMEOUT_MS) and
# one API request at 10 min (API_TIMEOUT_MS). Seen live: an agent got no model response after a
# tool result and sat silent until stopped.
TURN_SILENCE_TIMEOUT_S = 900

# What the platform reads this agent's health from, see progress.py. A turn silent for longer
# than the watchdog (turns.receive) allows is one the watchdog failed to end.
progress = Progress(quiet_limit_s=TURN_SILENCE_TIMEOUT_S + 60)

_composing = activity.Composing()

# The model provider's rate limit as the CLI last said it, for the whole account: a subagent's
# call that was refused refuses the agent's own, and the CLI says each change once.
_rate_limit = None
# When the model provider began to refuse this account's calls, until a turn has ended well since.
limit_since: float | None = None


def log_message(message, agent: str) -> None:
    """Prints the activity lines to stdout, notes what the agent is doing now and logs the rest."""
    global _rate_limit
    for line in activity.lines(message, agent, waited=progress.waited()):
        print(line, flush=True)
    if doing := activity.doing(message) or _composing.doing(message):
        progress.doing(doing)
    if isinstance(message, ResultMessage):
        log.info(
            "result: subtype=%s turns=%d cost_usd=%s stop_reason=%s",
            message.subtype, message.num_turns, message.total_cost_usd, message.stop_reason,
        )
    elif isinstance(message, RateLimitEvent):
        info = _rate_limit = message.rate_limit_info
        log.info(
            "rate_limit: status=%s type=%s utilization=%s resets_at=%s",
            info.status, info.rate_limit_type, info.utilization, info.resets_at,
        )
        progress.model(info.status, info.rate_limit_type, info.utilization, info.resets_at)


def refused_until() -> float | None:
    """When the model provider's limit resets, while it refuses this account's calls. None while
    it takes them, or when it said no reset time."""
    info = _rate_limit
    if info is None or info.status != "rejected" or not info.resets_at or info.resets_at <= time.time():
        return None
    return float(info.resets_at)


def limit_kind() -> str:
    """The provider's name for the limit it last said, "usage" when it named none."""
    return getattr(_rate_limit, "rate_limit_type", None) or "usage"


def refused_from_now() -> None:
    """Notes that the provider refuses calls, from now unless it already did."""
    global limit_since
    limit_since = limit_since or time.time()


def limit_lifted() -> None:
    global limit_since
    limit_since = None


def note_background(message, background: dict[str, dict]) -> None:
    """Keeps background to the tasks the CLI says run in the background, by id: subagents, and a
    command the CLI backgrounded by itself."""
    if isinstance(message, SystemMessage) and message.subtype == "background_tasks_changed":
        background.clear()
        background.update((task["task_id"], task) for task in message.data.get("tasks", []))
        progress.subagents(len(background))


def is_subagent(task: dict) -> bool:
    """Whether a background task the CLI lists is a subagent. The CLI names a subagent's type
    local_agent and a command's local_bash."""
    return task.get("task_type", "local_agent").endswith("agent")
