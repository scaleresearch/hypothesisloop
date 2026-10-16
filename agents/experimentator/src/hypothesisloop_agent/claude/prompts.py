"""What the agent is told: the prompt that opens a connection, the updates of a batch, and the
context every prompt that starts a turn carries."""
from __future__ import annotations

import asyncio
import os
import time

from hypothesisloop_agent import config, core, proposal, seed, updates
from hypothesisloop_agent.claude import stream
from hypothesisloop_agent.claude.running import Running
from hypothesisloop_agent.claude.store import Store

# Said first to the session that follows a wait for the limit, until a turn there has ended well.
_LIMIT_WAITED = ("The model provider refused your calls from {since} UTC, its usage limit reached, and you were "
                 "paused until it reset. Jobs on the platform ran on meanwhile; any subagent of yours stopped. "
                 "Look at the state of your jobs and hypotheses with hl before you go on.")


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


def _revision_prompt(setup: core.RunSetup, store: Store, draft: dict) -> str:
    """What a planner is told of the revision it has to answer, with the proposal the draft holds
    put back as a file. A draft it declined holds none."""
    prompt = _REVISION_PROMPT.format(revision=draft["revisions"][-1]["message"]) + (_REVISION_NEW_MACHINE if store.on_new_machine else "")
    if not draft.get("proposal"):
        return prompt
    path = os.path.join(os.path.abspath(setup.statedir), _PROPOSAL_FILE)
    with open(path, "w") as f:
        f.write(proposal.render(draft["proposal"]))
    return prompt + _REVISION_PROPOSAL.format(path=path)


async def turn_start(setup: core.RunSetup, watch_env: dict[str, str], prompt: str) -> str:
    """prompt as it starts a turn: with what stands around it, read now, at its end. The one
    place a turn's context is put together: a baseline agent's clock, and the situation for every
    role in a run, which a planner has none of yet. A tool call mid-turn is handed its updates
    alone."""
    clock = await core.baseline_clock(setup)
    situation = "" if config.plans(setup.cfg) else await updates.situation(watch_env)
    return "\n\n".join(part for part in (prompt, clock, situation) if part)


async def told(setup: core.RunSetup, store: Store, running: Running, events: list[dict]) -> str:
    """What the agent is told of a batch of updates, "" for nothing. A revision asked of a planner
    is an event that points at the draft: it is told as the revision itself, and one that the
    planner has answered since, which a restored cursor replays, is told as nothing. That its own
    instructions changed is told to no one: the harness applies them at the turn boundary."""
    events = running.not_own(events)
    rest = [e for e in events if e["kind"] != updates.REVISION]
    parts = [updates.message(rest, setup.cfg.agent_id)] if rest else []
    if len(rest) < len(events):
        draft = await core.revising(setup)
        if draft:
            parts.insert(0, _revision_prompt(setup, store, draft))
    return "\n\n".join(parts)


async def opening(setup: core.RunSetup, store: Store, resume_id: str | None, initial_prompt: str | None) -> str:
    """What opens a connection: for a resumed session the revision that brought a planner back,
    else a handoff, that it goes on, or for a session that starts fresh what its role reads first:
    a planner's opening reads, a baseline agent's goal with the planner's map of the repository.
    A wait for the model provider's limit and a new machine are said before it."""
    if resume_id and not initial_prompt and (draft := await core.revising(setup)):
        return _revision_prompt(setup, store, draft)
    if initial_prompt:
        body = initial_prompt
    elif resume_id:
        body = "Continue where you left off."
    elif config.measures_baseline(setup.cfg):
        body = seed.baseline(await core.repo_map(setup))
    elif config.plans(setup.cfg):
        body = "\n\n".join(part for part in ("Begin.", await asyncio.to_thread(seed.planner, setup.workdir)) if part)
    else:
        body = "Begin."
    since = stream.limit_since
    waited = _LIMIT_WAITED.format(since=time.strftime("%Y-%m-%d %H:%M", time.gmtime(since))) if since else ""
    return "\n\n".join(part for part in (_new_machine_note(setup.cfg) if store.on_new_machine else "", waited, body) if part)
