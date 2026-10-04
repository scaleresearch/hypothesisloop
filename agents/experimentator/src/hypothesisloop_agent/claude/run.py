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
import os
import re
import time
from pathlib import Path

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    CLIConnectionError,
    HookMatcher,
    ProcessError,
    RateLimitEvent,
    ResultMessage,
    StreamEvent,
    SystemMessage,
)

from hypothesisloop_agent import config, core, progress, proposal, session, updates
from hypothesisloop_agent.claude import activity

log = logging.getLogger("llm_agent")

# These are interactive-harness features (a resumable Claude Code session with a human/UI on the
# other end) that don't do anything meaningful inside this worker, but the SDK's default tool set
# still advertises them — and the model, seeing them, reasonably tries to use them to "wait" for a
# background job. Confirmed live: an agent called ScheduleWakeup expecting to be resumed later and
# just ended its turn instead, because nothing here ever fires that wakeup. Strip them so the model
# never reaches for a tool that can't do what its name promises in this environment.
DISALLOWED_TOOLS = ["ScheduleWakeup", "Monitor", "TaskCreate", "TaskUpdate", "TaskGet", "TaskList",
                     "TaskOutput", "TaskStop"]


# The CLI's Write refuses an existing file the session has not opened with its Read, and no
# setting lifts that: an agent that had only `cat` a file was refused its Write.
_FILE_TOOLS_NOTE = "\n\nWrite and Edit refuse an existing file you have not opened with Read: `cat` does not count."


def _log_message(message, agent: str) -> None:
    """Prints the activity lines to stdout, notes what the agent is doing now and logs the rest."""
    for line in activity.lines(message, agent, waited=_progress.waited()):
        print(line, flush=True)
    if doing := activity.doing(message) or _composing.doing(message):
        _progress.doing(doing)
    if isinstance(message, ResultMessage):
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


def _updates_hook(setup: core.RunSetup, watch_env: dict[str, str]):
    """A PostToolUse hook that hands the agent the updates that arrived while it worked."""
    draining = asyncio.Lock()
    agent_id = setup.cfg.agent_id

    async def hook(hook_input, tool_use_id, context) -> dict:
        # One drain per step: parallel tool calls share it, and a subagent's calls leave the
        # updates for the agent itself.
        if draining.locked() or hook_input.get("agent_id"):
            return {}
        async with draining:
            events = await updates.drain(watch_env)
        told = await _told(setup, events)
        if not told:
            return {}
        for line in activity.event_lines(events, agent_id):
            print(line, flush=True)
        return {"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": told}}

    return hook


# SendMessage reaches this session's own subagents and no agent of the platform experiment. The
# CLI answers one it could not deliver with "use the agent ID", and an agent then tries each id.
_UNDELIVERED = (
    "No message reaches another agent of the platform experiment, under any name or id: send none again. "
    "Write it with `hl hypothesis comment ID --text T` on a hypothesis of that agent: its owner and the "
    "coordinator get it as `comment.new` in their next update.")


async def _undelivered_hook(hook_input, tool_use_id, context) -> dict:
    """Tells an agent whose SendMessage reached nobody how the other agent gets what it wrote. The
    CLI reports that as a failed call or as an answer with success false."""
    event = hook_input["hook_event_name"]
    if event == "PostToolUse" and hook_input["tool_response"].get("success") is not False:
        return {}
    return {"hookSpecificOutput": {"hookEventName": event, "additionalContext": _UNDELIVERED}}


_UNDELIVERED_MATCHER = HookMatcher(matcher="SendMessage", hooks=[_undelivered_hook])


# A baseline agent slept 23 times around `hl job status` for a job being built, though hl job
# submit had said the result arrives as an update: each wake was a model call over its whole
# context, 44% of what it spent. A command that waits is refused with how to wait.
_WAIT_LIMIT_S = 30
_SLEEP = re.compile(r"\bsleep((?:\s+\d+(?:\.\d+)?[smhd]?\b)+)")
_SECONDS = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}


def _waits(command: str) -> bool:
    if re.search(r"\bhl\s+job\s+wait\b", command):
        return True
    return sum(float(n) * _SECONDS[unit] for args in _SLEEP.findall(command)
               for n, unit in re.findall(r"(\d+(?:\.\d+)?)([smhd]?)", args)) >= _WAIT_LIMIT_S


async def _wait_hook(hook_input, tool_use_id, context) -> dict:
    if not _waits(hook_input["tool_input"].get("command", "")):
        return {}
    return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                   "permissionDecisionReason": core.WAIT}}


def _session_file(setup: core.RunSetup) -> str:
    return os.path.join(setup.statedir, _SESSION_ID_FILE)


def _handoff_file(setup: core.RunSetup) -> str:
    return os.path.join(setup.statedir, _HANDOFF_FILE)


def _load_resume_id(setup: core.RunSetup) -> str | None:
    path = _session_file(setup)
    try:
        with open(path) as f:
            session_id = f.read().strip()
        return session_id or None
    except FileNotFoundError:
        return None


def _save_resume_id(setup: core.RunSetup, session_id: str) -> None:
    with open(_session_file(setup), "w") as f:
        f.write(session_id)


def _load_handoff(setup: core.RunSetup) -> str | None:
    try:
        with open(_handoff_file(setup)) as f:
            handoff = f.read().strip()
        return handoff or None
    except FileNotFoundError:
        return None


def _remove_file(path: str) -> None:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass


# Backoff for reconnect attempts that aren't governed by an explicit RateLimitInfo.resets_at
# (e.g. a dropped connection, a 5xx from the API, transient CLI crash). Capped well below the
# quota backoff below since these are usually transient.
_RECONNECT_BACKOFF_INITIAL_S = 30
_RECONNECT_BACKOFF_MAX_S = 900

# Anthropic's stated rate-limit windows top out at 7 days; cap how long we'll sleep on a single
# resets_at read in case of a bogus/far-future timestamp, and re-check state after waking rather
# than trusting one read for arbitrarily long.
_MAX_SINGLE_SLEEP_S = 3600

# Claude Code normally auto-compacts near its model-dependent context limit, but that path does
# not fire reliably for this SDK's long-lived sequence of query()/receive_response() calls (the
# production transcript reached 650K cache-read tokens with zero compact boundaries). The SDK has
# no public compact method or context-limit option, so checkpoint into a fresh, non-resumed
# session at a fixed ceiling. The last assistant message's usage measures the context of the latest
# model call; ResultMessage usage sums every call in the turn and overstates it many times over.
_CONTEXT_COMPACTION_THRESHOLD_TOKENS = 400_000
_COMPACTION_PROMPT = (
    "We must rotate to a fresh session because this session has reached its context budget. "
    "Produce a concise but complete handoff for the next instance of yourself: current platform "
    "experiment state, hypotheses and evidence, actions/results so far, important files or jobs, "
    "decisions and rejected paths, and the exact next useful steps. Do not do more research or "
    "use tools; output only the handoff."
)


# Longest silence a healthy turn can produce between two CLI messages: Claude Code caps one Bash
# call at 10 min (BASH_MAX_TIMEOUT_MS) and one API request at 10 min (API_TIMEOUT_MS). Seen live:
# an agent got no model response after a tool result and sat silent until stopped.
_TURN_SILENCE_TIMEOUT_S = 900

# Longest a model call may go without a message, from its request to its last stream event. A
# healthy call sends one at least every 1.5 s, thinking included (an empty thinking_delta per ~50
# tokens, measured on the bundled CLI); one call sat 6 min on 2790 tokens. A stalled call is
# interrupted and asked for again once.
_CALL_STALL_S = 120
_STALL_PROMPT = f"Your last model call sent nothing for {_CALL_STALL_S} s and was interrupted. Go on from where it stopped."


# On a stop a turn in flight is interrupted and the agent, working or idle, gets one turn of this
# length to write down what it already knows. `podman stop` must allow for it: -t 90. Jobs still
# in flight are left running and without a summary: nothing of theirs can be written yet. An agent
# whose own action ended its run owes nothing: its turn in flight gets this long to end by itself
# and no turn follows.
_SHUTDOWN_TURN_S = 60
_LAST_TURN = (
    "This is your last turn, about a minute long. Start nothing new and "
    "leave unfinished jobs running: they cannot be summarised yet. File the summaries you owe on "
    "finished jobs{verdicts}, then end your turn. "
    "A refusal with reason `experiment_closed` ends it too: your run is over, so retry nothing."
)
_SHUTDOWN_PROMPT = _LAST_TURN.format(verdicts=" and set the verdicts your evidence already supports")
# A baseline agent registers no hypothesis, so it has no verdict to set.
_BASELINE_SHUTDOWN_PROMPT = _LAST_TURN.format(verdicts="")


_COORDINATOR_SHUTDOWN_PROMPT = (
    "This is your last turn, about a minute long. Start nothing new. "
    "Write the summary with everything you have found (`hl platform-experiments summary`), and "
    "where it says why the run ended, say what is stated above. Then end your turn."
)


def _shutdown_prompt(setup: core.RunSetup) -> str | None:
    """The last turn's instruction, opened with why the machine stops as the platform has it now.
    None for an agent whose own action ended its run: it gets no last turn."""
    cause = core.stop_cause(setup)
    if cause is None:
        return None
    if config.coordinates(setup.cfg):
        return f"{cause} {_COORDINATOR_SHUTDOWN_PROMPT}"
    return f"{cause} {_BASELINE_SHUTDOWN_PROMPT if config.measures_baseline(setup.cfg) else _SHUTDOWN_PROMPT}"


class TurnStalled(Exception):
    pass


def _transport_error(error: Exception) -> bool:
    """Whether error is a lost connection to the CLI, which a new session repairs. The SDK
    raises a CLI that died mid-stream, and a control request it never answered, as Exception
    itself, so that exact class counts and no subclass of it does."""
    return isinstance(error, (CLIConnectionError, ProcessError, TurnStalled)) or type(error) is Exception


# What the platform reads this agent's health from, see progress.py. A turn silent for longer
# than the watchdog below allows is one the watchdog failed to end.
_progress = progress.Progress(quiet_limit_s=_TURN_SILENCE_TIMEOUT_S + 60)

_composing = activity.Composing()

# Where the session is kept so that it outlives this machine. Set by main().
_sessions: session.Sessions | None = None

_SESSION_ID_FILE = ".claude_session_id"
_HANDOFF_FILE = ".claude_compaction_handoff"
# The session a turn is in flight in, known from its first message on. It travels with the
# snapshot, so a machine lost in the middle of a turn, the first one included, is resumed there.
_LIVE_SESSION_FILE = ".claude_live_session_id"
# Where hl keeps the cursor of the agent's update feed (updates.watch_env).
_WATCH_STATE_DIR = ".hl-state"
# How often a session is handed to the platform while a turn is in flight. A turn can last as
# long as a job does, and nothing unchanged is sent.
_SESSION_STORE_INTERVAL_S = 30
# How long stores fail in a row before the agent's activity line says so: a platform that is
# away for a moment is no news.
_NOT_STORED_SAID_S = 90
_not_stored_since: float | None = None

_live_session_id: str | None = None
_store_lock = asyncio.Lock()


def _seen_session(setup: core.RunSetup, message) -> None:
    """Notes the session a message belongs to as the one in flight."""
    global _live_session_id
    session_id = getattr(message, "session_id", None)
    if session_id is None and isinstance(getattr(message, "data", None), dict):
        session_id = message.data.get("session_id")
    if session_id and session_id != _live_session_id:
        _live_session_id = session_id
        with open(os.path.join(setup.statedir, _LIVE_SESSION_FILE), "w") as f:
            f.write(session_id)


def _forget_live_session(setup: core.RunSetup) -> None:
    global _live_session_id
    _live_session_id = None
    _remove_file(os.path.join(setup.statedir, _LIVE_SESSION_FILE))


def _model_home() -> str:
    return config.required("CLAUDE_CONFIG_DIR", "the directory the model CLI keeps its sessions in")


def _transcript_files(session_id: str | None) -> list[str]:
    """The model CLI's own record of the session, relative to its home: the transcript and
    whatever it keeps in a directory of the session's name beside it."""
    if not session_id:
        return []
    home = Path(_model_home())
    found = []
    for transcript in home.glob(f"projects/*/{session_id}.jsonl"):
        found.append(transcript)
        found.extend(path for path in transcript.with_suffix("").rglob("*") if path.is_file())
    return [str(path.relative_to(home)) for path in found]


async def _store_session(setup: core.RunSetup) -> None:
    """Hands the session as it stands to the platform, unless it already holds this one."""
    if _sessions is None:
        return
    statedir = Path(setup.statedir)
    files = [_SESSION_ID_FILE, _HANDOFF_FILE, _LIVE_SESSION_FILE]
    files += [str(path.relative_to(statedir)) for path in (statedir / _WATCH_STATE_DIR).rglob("*") if path.is_file()]
    transcripts = {name for session_id in (_load_resume_id(setup), _live_session_id) for name in _transcript_files(session_id)}
    async with _store_lock:
        try:
            await asyncio.to_thread(_sessions.save, files, sorted(transcripts))
        except session.NotStored as why:
            _not_stored(str(why))
        else:
            _not_stored("")


def _not_stored(why: str) -> None:
    """Notes a store that failed for why, "" for one that did not. The agent works on and the
    next interval stores again. A failure that persists is said in the agent's activity line
    until a store succeeds: a replacement machine would resume from an older session, silently."""
    global _not_stored_since
    if not why:
        _not_stored_since = None
        _progress.trouble("")
        return
    if _not_stored_since is None:
        _not_stored_since = time.monotonic()
    lasted = time.monotonic() - _not_stored_since
    persists = lasted >= _NOT_STORED_SAID_S
    log.log(logging.ERROR if persists else logging.WARNING,
            "session not stored for %.0fs: %s. A replacement machine would resume from the last one stored. Storing again in %ds",
            lasted, why, _SESSION_STORE_INTERVAL_S)
    if persists:
        _progress.trouble(f"session not stored: {why}")


async def _keep_session_stored(setup: core.RunSetup) -> None:
    while True:
        await asyncio.sleep(_SESSION_STORE_INTERVAL_S)
        if _live_session_id or _not_stored_since is not None:
            await _store_session(setup)


# Said first to a session that was restored on a machine it did not run on, until a turn there
# has ended well. The conversation remembers files this machine never had.
_NEW_MACHINE_NOTE = (
    "The machine you ran on was replaced. Your conversation is the one you had, and nothing else "
    "came with it: files you wrote and did not push are gone, and so is every process you started. ")
_NEW_MACHINE_RESEARCHER = (
    "Clone the repository again, run `hl hypothesis checkout ID` for the hypothesis you work on, "
    "and look at the state of your jobs and hypotheses with hl before you go on.")
_NEW_MACHINE_BASELINE = (
    "Clone the repository again, check out your branch, and look at the state of your jobs with "
    "hl before you go on.")
# A coordinator has no branch and writes nothing to the repository.
_NEW_MACHINE_COORDINATOR = (
    "Keep the summary's file again from `hl platform-experiments get`, clone the repository only "
    "if you need to read it, and look at the state of the jobs and hypotheses with hl before you "
    "go on.")
# A planner has no job and no hypothesis: what it had was its clone and the proposal it was writing.
_NEW_MACHINE_PLANNER = (
    "Clone the repository again, read the draft with `hl platform-experiments get`, and write "
    "your proposal's file again from what you had worked out before you go on.")


def _new_machine_note(cfg) -> str:
    if config.coordinates(cfg):
        return _NEW_MACHINE_NOTE + _NEW_MACHINE_COORDINATOR
    if config.plans(cfg):
        return _NEW_MACHINE_NOTE + _NEW_MACHINE_PLANNER
    return _NEW_MACHINE_NOTE + (_NEW_MACHINE_BASELINE if config.measures_baseline(cfg) else _NEW_MACHINE_RESEARCHER)


# A planner's machine is released some minutes after its answer and a revision asked later
# starts another. Told only that its machine was replaced and to continue, a planner had to find
# the revision by itself, and with the file of its proposal gone it wrote the whole proposal again.
_REVISION_PROMPT = (
    "The person read your answer and asks for this revision:\n\n{revision}\n\n"
    "Read the draft and the capacity again and answer with a new proposal.")
_REVISION_NEW_MACHINE = " Your machine was released with your answer: a file you wrote on it is gone."
_REVISION_PROPOSAL = " Your last proposal, as the platform stores it, is in {path}: edit that file and propose it."
_PROPOSAL_FILE = "proposal.yaml"


def _revision_prompt(setup: core.RunSetup, draft: dict) -> str:
    """What a planner is told of the revision it has to answer, with the proposal the draft holds
    put back as a file. A draft it declined holds none."""
    prompt = _REVISION_PROMPT.format(revision=draft["revisions"][-1]["message"]) + (_REVISION_NEW_MACHINE if _on_new_machine else "")
    if not draft.get("proposal"):
        return prompt
    path = os.path.join(os.path.abspath(setup.statedir), _PROPOSAL_FILE)
    with open(path, "w") as f:
        f.write(proposal.render(draft["proposal"]))
    return prompt + _REVISION_PROPOSAL.format(path=path)


async def _told(setup: core.RunSetup, events: list[dict]) -> str:
    """What the agent is told of a batch of updates, "" for nothing. A revision asked of a planner
    is an event that points at the draft: it is told as the revision itself, and one that the
    planner has answered since, which a restored cursor replays, is told as nothing. A baseline
    agent is told its clock with the updates."""
    rest = [e for e in events if e["kind"] != updates.REVISION]
    parts = [updates.message(rest, setup.cfg.agent_id)] if rest else []
    if len(rest) < len(events):
        draft = await asyncio.to_thread(core.revising, setup)
        if draft:
            parts.insert(0, _revision_prompt(setup, draft))
    if parts:
        parts.append(await asyncio.to_thread(core.baseline_clock, setup))
    return "\n\n".join(part for part in parts if part)


def _first_prompt(setup: core.RunSetup, resume_id: str | None, initial_prompt: str | None) -> str:
    """What opens a connection: a handoff, the start, or for a resumed session the revision that
    brought a planner back, else that it goes on, with a baseline agent's clock. A new machine is
    said in each."""
    draft = core.revising(setup) if resume_id and not initial_prompt else None
    if draft:
        return _revision_prompt(setup, draft)
    prompt = "\n\n".join(part for part in (initial_prompt or ("Continue where you left off." if resume_id else "Begin."),
                                             core.baseline_clock(setup)) if part)
    return _new_machine_note(setup.cfg) + "\n\n" + prompt if _on_new_machine else prompt


_on_new_machine = False


def _settled_on_this_machine() -> None:
    global _on_new_machine
    _on_new_machine = False


def _restore_session(setup: core.RunSetup) -> None:
    """On a machine that holds no session of its own, takes the one the platform keeps."""
    global _on_new_machine
    if _sessions is None or _load_resume_id(setup) or _load_handoff(setup):
        return
    if _sessions.restore():
        _on_new_machine = True
        live = os.path.join(setup.statedir, _LIVE_SESSION_FILE)
        if not _load_resume_id(setup) and os.path.exists(live):
            # The machine was lost before the turn in flight ended: that session is the one to
            # resume, and a handoff beside it was already given to it.
            with open(live) as f:
                _save_resume_id(setup, f.read().strip())
        log.info("resuming session %s on a new machine", _load_resume_id(setup) or "from its handoff")


async def _interrupt_on_stop(client, setup: core.RunSetup, stopped) -> None:
    """Interrupts the turn in flight as soon as stopped() is true. The turn of an agent whose own
    action ended its run is its last and is interrupted only if it goes on past _SHUTDOWN_TURN_S."""
    while not stopped():
        await asyncio.sleep(1)
    if core.stop_cause(setup) is None:
        log.info("stop requested after the agent ended its own run: its turn has %ss to end by itself", _SHUTDOWN_TURN_S)
        await asyncio.sleep(_SHUTDOWN_TURN_S)
    try:
        await client.interrupt()
    except Exception as e:
        log.warning("could not interrupt the turn (%s): it runs to its own end", e)


async def _last_turn(client, setup: core.RunSetup, watch_env: dict[str, str], events: list[dict]) -> None:
    """The shutdown turn, opened with events and whatever else finished since the last delivery.
    An agent whose own action ended its run gets none."""
    prompt = _shutdown_prompt(setup)
    if prompt is None:
        log.info("stopping: the agent ended its own run, no last turn")
        return
    log.info("stopping: one last turn to file what is owed")
    agent = setup.cfg.agent_id
    events = [*events, *await updates.drain(watch_env)]
    for line in activity.event_lines(events, agent):
        print(line, flush=True)
    await client.query(prompt + ("\n\n" + updates.message(events, agent) if events else ""))
    try:
        async with asyncio.timeout(_SHUTDOWN_TURN_S):
            async for message in client.receive_response():
                _log_message(message, agent)
    except TimeoutError:
        log.info("last turn cut off after %ss", _SHUTDOWN_TURN_S)


def _in_call(message, in_call: bool) -> bool:
    """Whether a model call is in flight once message has arrived."""
    if isinstance(message, SystemMessage) and message.subtype == "status":
        return in_call or message.data.get("status") == "requesting"
    if isinstance(message, StreamEvent):
        kind = message.event.get("type")
        if kind == "message_start" and not message.parent_tool_use_id:
            return True
        if kind == "message_stop":
            return False
    return in_call and not isinstance(message, ResultMessage)


async def _interrupt_stalled_call(client) -> list:
    """Interrupts the call in flight and returns what the CLI sent until it ended the turn. The
    timeout closed the response iterator it cut, and the CLI's messages wait for a new one."""
    log.warning("a model call sent nothing for %ss: interrupting it and asking again once", _CALL_STALL_S)
    await client.interrupt()
    sent = []
    try:
        async with asyncio.timeout(_CALL_STALL_S):
            async for message in client.receive_response():
                if isinstance(message, ResultMessage):
                    break
                sent.append(message)
    except TimeoutError:
        raise TurnStalled(f"the interrupted model call did not end in {_CALL_STALL_S}s") from None
    await client.query(_STALL_PROMPT)
    return sent


async def _receive_turn(client):
    """client.receive_response(), failing with TurnStalled when the CLI goes silent, or when a
    model call stalls a second time."""
    messages = client.receive_response().__aiter__()
    _progress.doing(progress.THINKING)
    _progress.awaiting()
    in_call = retried = False
    try:
        while True:
            limit = _CALL_STALL_S if in_call else _TURN_SILENCE_TIMEOUT_S
            try:
                async with asyncio.timeout(limit):
                    message = await anext(messages)
            except StopAsyncIteration:
                return
            except TimeoutError:
                if not in_call or retried:
                    raise TurnStalled(f"no message from the CLI for {limit}s") from None
                in_call = retried = True
                for message in await _interrupt_stalled_call(client):
                    yield message
                messages = client.receive_response().__aiter__()
                continue
            in_call = _in_call(message, in_call)
            retried = retried and in_call
            yield message
            # A stream event is part of the model call in flight, which is still awaited: how long
            # it took is said when it answers.
            if not isinstance(message, StreamEvent):
                _progress.awaiting()
    finally:
        _progress.resting()
        _progress.doing(progress.WAITING)


# How hard the model thinks before each step. A baseline agent measures a control it is told how to
# run, against a deadline: at high it thought for 107 s (9155 tokens) before running `date` and
# for 135 s (10529 tokens) before a `cat`, 7 min of 40 in single calls. The other roles design
# and judge experiments and keep high.
def _effort(cfg) -> str:
    return "medium" if config.measures_baseline(cfg) else "high"


def _context_tokens(usage: dict) -> int:
    """Context carried by one model call, from its usage block."""
    return sum(int(usage.get(key, 0) or 0) for key in (
        "input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens",
    ))


# What the CLI answers a turn with when the model provider refuses the credential itself. A rate
# limit, an overloaded provider or a network failure is none of these and is waited out.
_CREDENTIAL_REFUSALS = (
    "not logged in", "please run /login", "invalid api key", "invalid x-api-key", "authentication_error",
    "oauth token has expired", "oauth token has been revoked", "invalid bearer token",
)


def credential_refused(result: str | None) -> bool:
    text = (result or "").lower()
    return any(marker in text for marker in _CREDENTIAL_REFUSALS)


async def _run_one_session(setup: core.RunSetup, model: str, resume_id: str | None,
                            stop_flag: list,
                            initial_prompt: str | None = None,
                            on_good_turn=lambda: None,
                            ) -> tuple[float | None, bool]:
    """Runs one ClaudeSDKClient connection until it ends (real stop, error, or rate limit).

    Returns (rate-limit reset time, rotated). Rotated means a handoff was written and the next
    session starts fresh from it. The session id and the handoff live on disk, see _restart_state.
    """
    cfg = setup.cfg
    watch_env = updates.watch_env(cfg.api_url, cfg.token, setup.statedir)
    options = ClaudeAgentOptions(
        system_prompt=setup.system_prompt + _FILE_TOOLS_NOTE,
        model=model,
        effort=_effort(cfg),
        # The stream events of a model call say what it writes while it writes, see activity.Composing.
        include_partial_messages=True,
        permission_mode="bypassPermissions",
        # No max_turns cap: this session is meant to run for the platform experiment's whole
        # lifetime (hours to days), not a bounded number of turns. The only real stop conditions
        # are a signal and the end of its run on the platform, see core.stop_reason.
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
        # The 2 min default backgrounds long commands, and agents then poll their output files.
        # Without background tasks Bash has no run_in_background to wait in. The briefings say
        # every command starts in the working directory: the CLI keeps a `cd` otherwise, and an
        # agent that repeated one failed.
        env={"BASH_DEFAULT_TIMEOUT_MS": "600000", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
             "CLAUDE_BASH_MAINTAIN_PROJECT_WORKING_DIR": "1", **core.git_identity(cfg)},
        hooks={"PreToolUse": [HookMatcher(matcher="Bash", hooks=[_wait_hook])],
               "PostToolUse": [HookMatcher(hooks=[_updates_hook(setup, watch_env)]), _UNDELIVERED_MATCHER],
               "PostToolUseFailure": [_UNDELIVERED_MATCHER]},
    )

    rate_limited_resets_at: float | None = None

    def stopped() -> bool:
        return stop_flag[0]

    async def over(events: list[dict]) -> bool:
        """Whether the run loop stops here, after the last turn when there is a platform to file
        with: a run it ended and a machine told to stop get one, a refused token and a platform
        out of reach for the limit get none. The one place a last turn is given."""
        lost = core.run_over(setup)
        if not lost and not stopped():
            return False
        log.info("stopping: %s", lost or "received signal")
        if not core.lost_platform(lost):
            await _last_turn(client, setup, watch_env, events)
            await _store_session(setup)
        return True

    connecting = time.monotonic()
    async with ClaudeSDKClient(options=options) as client:
        log.info("model CLI connected in %.1fs", time.monotonic() - connecting)
        await client.query(_first_prompt(setup, resume_id, initial_prompt))
        while True:
            got_result = False
            turn_is_error = False
            context_tokens = 0
            interrupter = asyncio.ensure_future(_interrupt_on_stop(client, setup, stopped))
            try:
                async for message in _receive_turn(client):
                    _seen_session(setup, message)
                    _log_message(message, setup.cfg.agent_id)
                    if isinstance(message, AssistantMessage) and message.usage and not message.parent_tool_use_id:
                        context_tokens = _context_tokens(message.usage)
                    if isinstance(message, RateLimitEvent) and message.rate_limit_info.status == "rejected":
                        rate_limited_resets_at = message.rate_limit_info.resets_at
                    if isinstance(message, ResultMessage):
                        got_result = True
                        turn_is_error = message.is_error
                        if message.is_error and credential_refused(message.result):
                            # Backing off cannot fix a credential the provider refuses. Exiting
                            # is what tells whoever started this agent, and with what words.
                            raise SystemExit(f"agent: the model provider refused this agent's credential: {(message.result or '').strip()[:300]}")
                        resume_id = message.session_id
                        _save_resume_id(setup, resume_id)
                        if not message.is_error:
                            on_good_turn()
                            _settled_on_this_machine()
                            if initial_prompt:
                                _remove_file(_handoff_file(setup))
            finally:
                interrupter.cancel()
            if got_result:
                await _store_session(setup)

            if got_result and not rate_limited_resets_at and await over([]):
                return None, False

            if rate_limited_resets_at:
                log.info("stopping session: rate-limited, resets_at=%s", rate_limited_resets_at)
                return rate_limited_resets_at, False

            if not got_result:
                # The stream ended without a ResultMessage — typically the CLI process itself
                # died mid-turn (e.g. an authentication or usage-limit error the SDK couldn't
                # turn into a clean ResultMessage). Treat as a transient error to retry, not a
                # reason to stop the whole agent: a quota outage must not end the run, it must
                # pause it.
                log.warning("stream ended without a ResultMessage — treating as transient")
                return None, False

            if turn_is_error:
                # is_error=True with subtype="success" is the CLI's signal for an underlying
                # failure it couldn't otherwise surface (auth not set up, a bad API response,
                # ...) — e.g. "Not logged in · Please run /login". The turn still "completes"
                # instantly with no tool use, so falling through to the update wait would treat a
                # broken session as an idle agent. Route it through the same backoff as a
                # transient error instead.
                log.warning("turn completed with an underlying error (is_error) — treating as transient")
                return None, False

            if context_tokens >= _CONTEXT_COMPACTION_THRESHOLD_TOKENS:
                log.info("context reached ~%d tokens — creating handoff before fresh session", context_tokens)
                await client.query(_COMPACTION_PROMPT)
                handoff_result = None
                async for message in _receive_turn(client):
                    _log_message(message, setup.cfg.agent_id)
                    if isinstance(message, ResultMessage):
                        handoff_result = message
                if not handoff_result or handoff_result.is_error:
                    log.warning("context handoff failed — preserving old session for retry")
                    return None, False
                handoff = (handoff_result.result or "").strip()
                if not handoff:
                    log.warning("context handoff was empty — preserving old session for retry")
                    return None, False
                # Persist the bridge before removing the old id. If the worker dies before the
                # fresh session returns its first ResultMessage, startup retries this handoff;
                # only that successful result removes the file.
                with open(_handoff_file(setup), "w") as f:
                    f.write(handoff)
                _remove_file(_session_file(setup))
                _forget_live_session(setup)
                await _store_session(setup)
                return None, True

            # The agent ended its turn: wait for the next batch of updates about its own jobs and
            # hypotheses and hand it over on the same connection.
            while True:
                events = await updates.next_batch(watch_env, stopped, updates.IDLE_NUDGE_S)
                told = "" if stopped() else await _told(setup, events)
                # A batch that tells nothing held only a revision answered since: the wait goes on.
                if told or not events or stopped():
                    break
            if not told and await over(events):
                return None, False
            if told:
                for line in activity.event_lines(events, cfg.agent_id):
                    print(line, flush=True)
                await client.query(told)
                continue
            log.info("no platform updates for %ds, nudging", updates.IDLE_NUDGE_S)
            await client.query(updates.IDLE_PROMPT)


def _restart_state(setup: core.RunSetup) -> tuple[str | None, str | None]:
    """The session to resume and the prompt that opens the next connection, as persisted.

    A session id beside a handoff means the replacement session already produced a result, so it
    is resumed and the bridge is not injected twice.
    """
    resume_id = _load_resume_id(setup)
    handoff = _load_handoff(setup)
    if resume_id or not handoff:
        return resume_id, None
    return None, "Continue this experiment from the following handoff:\n\n" + handoff


async def _run(setup: core.RunSetup, model: str) -> None:
    stop_flag = core.install_signal_handler()
    beat = asyncio.ensure_future(_progress.run())
    storing = asyncio.ensure_future(_keep_session_stored(setup))
    try:
        await core.run_until_lost(setup, _run_until_stopped(setup, model, stop_flag), stop_flag)
    finally:
        beat.cancel()
        storing.cancel()


async def _run_until_stopped(setup: core.RunSetup, model: str, stop_flag: list) -> None:
    backoff_s = _RECONNECT_BACKOFF_INITIAL_S

    def reset_backoff() -> None:
        nonlocal backoff_s
        backoff_s = _RECONNECT_BACKOFF_INITIAL_S

    # Outer loop: keeps the agent alive across quota exhaustion, transient errors, and dropped
    # connections for the whole platform-experiment lifetime — a "no budget right now" condition
    # must pause and resume, never permanently end the run, since running out of quota is an
    # everyday occurrence on a multi-day competition, not an exceptional one.
    while True:
        reason = core.stop_reason(setup, stop_flag)
        if reason:
            log.info("stopping: %s", reason)
            return

        resume_id, initial_prompt = _restart_state(setup)
        try:
            resets_at, rotated = await _run_one_session(
                setup, model, resume_id, stop_flag, initial_prompt, reset_backoff,
            )
        except Exception as e:
            if not _transport_error(e):
                raise
            log.warning("session lost its connection to the CLI (%s): reconnecting after backoff", e)
            resets_at, rotated = None, False
        # The CLI has exited here and written all of its transcript. The store at the end of a
        # turn can come before the CLI wrote the turn's last reply, and after a planner's
        # answer or an agent's finish no turn follows to store it.
        await _store_session(setup)
        if rotated or core.stop_reason(setup, stop_flag):
            continue

        if resets_at:
            sleep_s = max(1.0, min(resets_at - time.time(), _MAX_SINGLE_SLEEP_S))
            log.info("quota/rate limit hit — sleeping %.0fs before resuming", sleep_s)
            await asyncio.sleep(sleep_s)
            reset_backoff()
        else:
            log.info("reconnecting in %ss", backoff_s)
            await asyncio.sleep(backoff_s)
            backoff_s = min(backoff_s * 2, _RECONNECT_BACKOFF_MAX_S)


def main() -> None:
    global _sessions
    started = time.monotonic()
    setup = core.prepare()
    _sessions = session.Sessions(setup.client, setup.cfg, setup.statedir, _model_home())
    _restore_session(setup)
    model = setup.cfg.model
    # With the line the CLI connects with, this says where a slow start went: what precedes
    # both is the machine, its image and the interpreter.
    log.info(
        "llm_agent %s starting (claude): pe=%s model=%s, prepared in %.1fs",
        setup.cfg.agent_id, setup.cfg.platform_experiment_id, model, time.monotonic() - started,
    )
    asyncio.run(_run(setup, model))
    log.info("llm_agent %s stopped", setup.cfg.agent_id)


if __name__ == "__main__":
    main()
