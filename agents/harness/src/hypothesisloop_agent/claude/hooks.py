"""The hooks the CLI runs around the agent's tool calls: the updates handed over after each step,
and the refusals of what does not work here or is not a subagent's to do."""
from __future__ import annotations

import asyncio
import re
import time

from claude_agent_sdk import HookMatcher

from hypothesisloop_agent import core, updates
from hypothesisloop_agent.claude import activity, prompts, stream
from hypothesisloop_agent.claude.running import Running
from hypothesisloop_agent.claude.store import Store

# Refused for a Bash call with run_in_background: work that outlasts a command goes to a subagent.
BACKGROUND_COMMAND = ("Commands run in the foreground here. Work that should go on while you do something "
                      "else goes to a subagent started with run_in_background; its end starts your next turn.")
# A subagent starts none, and the CLI refuses it a leading `sleep` too.
SUBAGENT_BACKGROUND_COMMAND = "Commands run in the foreground here: wait for your jobs with `hl job wait ID`."
# A subagent is given TaskStop too, and could stop its siblings with it.
SUBAGENT_TASK_STOP = "Only the agent that started you stops a task."
# A subagent's interim SendMessages each started a turn of its lead over the lead's whole context:
# 41% of one executor's reads.
SUBAGENT_MESSAGE = ("Your final message, when you end, is your report: it reaches the agent that started you then, "
                    "so send it nothing before.")


def _updates_hook(setup: core.RunSetup, store: Store, running: Running, watch_env: dict[str, str], handing: asyncio.Lock):
    """A PostToolUse hook that hands the agent the updates that arrived while it worked. Every
    handover holds handing, so that each batch is handed over once."""
    agent_id = setup.cfg.agent_id

    async def hook(hook_input, tool_use_id, context) -> dict:
        # One drain per step: parallel tool calls share it, and a subagent's calls leave the
        # updates for the agent itself. One that finds a handover under way leaves its batch to
        # the next step.
        if handing.locked() or hook_input.get("agent_id"):
            return {}
        async with handing:
            env = updates.taking(watch_env, "draining")
            events = await updates.drain(env)
            told = await prompts.told(setup, store, running, events)
            updates.delivered(env)
        if not told:
            return {}
        forget_job_reads()
        for line in activity.event_lines(events, agent_id):
            print(line, flush=True)
        return {"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": told}}

    return hook


# The CLI answers a SendMessage it could not deliver with "use the agent ID", and an agent then
# tries each id. Its target was a subagent that a restart stopped, or another agent of the
# platform experiment, which SendMessage never reaches.
_UNDELIVERED = (
    "SendMessage reaches only the subagents running in this session: send none again. A subagent started "
    "before a restart was stopped with it: start it again with Agent, from its hypothesis, branch and jobs. "
    "Another agent of the platform experiment is reached with `hl hypothesis comment ID --text T` on one of "
    "its hypotheses: its owner and the coordinator get it as `comment.new` in their next update.")


async def _undelivered_hook(hook_input, tool_use_id, context) -> dict:
    """Tells an agent whose SendMessage reached nobody how the other agent gets what it wrote. The
    CLI reports that as a failed call or as an answer with success false."""
    event = hook_input["hook_event_name"]
    if event == "PostToolUse" and hook_input["tool_response"].get("success") is not False:
        return {}
    return {"hookSpecificOutput": {"hookEventName": event, "additionalContext": _UNDELIVERED}}


_UNDELIVERED_MATCHER = HookMatcher(matcher="SendMessage", hooks=[_undelivered_hook])


# A baseline agent slept 23 times around `hl job status` for a job being built, though the
# result arrives as an update: each wake was a model call over its whole
# context, 44% of what it spent. A command that waits is refused with how to wait.
_WAIT_LIMIT_S = 30
_SLEEP = re.compile(r"\bsleep((?:\s+\d+(?:\.\d+)?[smhd]?\b)+)")
_SECONDS = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}


def _waits(command: str) -> bool:
    if re.search(r"\bhl\s+job\s+wait\b", command):
        return True
    return sum(float(n) * _SECONDS[unit] for args in _SLEEP.findall(command)
               for n, unit in re.findall(r"(\d+(?:\.\d+)?)([smhd]?)", args)) >= _WAIT_LIMIT_S


# Refused sleeps, a baseline agent read `hl job status` and `metrics` of its running job every 3-5 s,
# each read a model call, saying it would end its turn. A read of a job repeated within
# _REREAD_S with no update delivered since is refused the same way: what changes arrives as one.
_REREAD_S = 120
_JOB_READ = re.compile(r"\bhl\s+job\s+(status|metrics|logs)((?:\s+[^\s|;&<>()]+(?=$|[\s|;&)]))*)")
_job_reads: dict[str, float] = {}


def forget_job_reads() -> None:
    """An update was delivered: every job may be read again."""
    _job_reads.clear()


def _repeated_read(command: str, now: float) -> str | None:
    """The read command repeats when every job read in it was read within _REREAD_S, else None,
    and the reads are noted."""
    reads = [" ".join(["hl job", sub, *args.split()]) for sub, args in _JOB_READ.findall(command)]
    if not reads:
        return None
    if all(read in _job_reads and now - _job_reads[read] < _REREAD_S for read in reads):
        return reads[0]
    _job_reads.update(dict.fromkeys(reads, now))
    return None


async def _wait_hook(hook_input, tool_use_id, context) -> dict:
    command = hook_input["tool_input"].get("command", "")
    if hook_input["tool_input"].get("run_in_background"):
        reason = SUBAGENT_BACKGROUND_COMMAND if hook_input.get("agent_id") else BACKGROUND_COMMAND
    elif hook_input.get("agent_id"):
        # A subagent carries one idea to its jobs' results: waiting for them keeps nothing else
        # from the agent, which gets its updates meanwhile.
        return {}
    elif _waits(command):
        reason = core.WAIT
    elif read := _repeated_read(command, time.monotonic()):
        reason = (f"`{read}` was read under {_REREAD_S // 60} minutes ago and no update has come since: "
                  f"a change to the job arrives as one. {core.WAIT}")
    else:
        return {}
    return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                   "permissionDecisionReason": reason}}


def _refuse_subagent(reason: str):
    """A PreToolUse hook that refuses the tool to subagents with reason."""
    async def hook(hook_input, tool_use_id, context) -> dict:
        if not hook_input.get("agent_id"):
            return {}
        return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                       "permissionDecisionReason": reason}}

    return hook


_task_stop_hook = _refuse_subagent(SUBAGENT_TASK_STOP)
_subagent_message_hook = _refuse_subagent(SUBAGENT_MESSAGE)


# The most subagents an agent runs at once is max_subagents of its instructions. Executors ran 3
# to 10, and each one's end starts a turn of the lead's over its whole context, which then grew
# past its rotation as some were always live.
SUBAGENT_CAP = ("{live} of your subagents are at work, the most that run at once. Start this one when one "
                "of them ends: its end starts your next turn.")
# How long a subagent let through counts after its start returned, while the CLI does not list it.
_STARTING_S = 60


class Subagents:
    """The subagents of a session: those the CLI lists in the background, and those whose start was
    let through that it does not list yet. Its messages are read after the hooks of the same step
    run, so the starts of one step all come before the first of them is listed."""

    def __init__(self, background: dict[str, dict], clock=time.monotonic):
        self._background, self._clock = background, clock
        # By tool use id: the agent id its start returned and when, None until it returned.
        self._starting: dict[str, tuple[str | None, float | None]] = {}

    def live(self) -> int:
        listed = {task_id for task_id, task in self._background.items() if stream.is_subagent(task)}
        now = self._clock()
        self._starting = {use: (agent, at) for use, (agent, at) in self._starting.items()
                          if agent not in listed and (at is None or now - at < _STARTING_S)}
        return len(listed) + len(self._starting)

    def starting(self, tool_use_id: str) -> None:
        self._starting[tool_use_id] = (None, None)

    def started(self, tool_use_id: str, response) -> None:
        """A start returned with response, None for one that failed. Only a subagent launched in
        the background is still at work."""
        if tool_use_id not in self._starting:
            return
        if isinstance(response, dict) and response.get("status") == "async_launched":
            self._starting[tool_use_id] = (response.get("agentId"), self._clock())
        else:
            del self._starting[tool_use_id]


def _subagent_hooks(subagents: Subagents, running: Running) -> dict[str, HookMatcher]:
    """The hooks that refuse a subagent past the cap, by hook event."""

    async def before(hook_input, tool_use_id, context) -> dict:
        if (live := subagents.live()) < running.current.max_subagents:
            subagents.starting(tool_use_id or hook_input.get("tool_use_id", ""))
            return {}
        return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                       "permissionDecisionReason": SUBAGENT_CAP.format(live=live)}}

    async def after(hook_input, tool_use_id, context) -> dict:
        response = hook_input.get("tool_response") if hook_input["hook_event_name"] == "PostToolUse" else None
        subagents.started(tool_use_id or hook_input.get("tool_use_id", ""), response)
        return {}

    return {"PreToolUse": HookMatcher(matcher="Agent", hooks=[before]),
            "PostToolUse": HookMatcher(matcher="Agent", hooks=[after]),
            "PostToolUseFailure": HookMatcher(matcher="Agent", hooks=[after])}


def matchers(setup: core.RunSetup, store: Store, running: Running, watch_env: dict[str, str], handing: asyncio.Lock,
             subagents: Subagents) -> dict[str, list[HookMatcher]]:
    """Every hook of a connection, by hook event."""
    capped = _subagent_hooks(subagents, running)
    return {"PreToolUse": [HookMatcher(matcher="Bash", hooks=[_wait_hook]),
                           HookMatcher(matcher="TaskStop", hooks=[_task_stop_hook]),
                           HookMatcher(matcher="SendMessage", hooks=[_subagent_message_hook]), capped["PreToolUse"]],
            "PostToolUse": [HookMatcher(hooks=[_updates_hook(setup, store, running, watch_env, handing)]), _UNDELIVERED_MATCHER,
                            capped["PostToolUse"]],
            "PostToolUseFailure": [_UNDELIVERED_MATCHER, capped["PostToolUseFailure"]]}
