"""The end of a connection on a stop: the turn in flight interrupted, and the last turn that files
what is owed."""
from __future__ import annotations

import asyncio
import logging

from hypothesisloop_agent import config, core, updates
from hypothesisloop_agent.claude import activity, prompts, stream
from hypothesisloop_agent.claude.running import Running

log = logging.getLogger("llm_agent")

# On a stop a turn in flight is interrupted and the agent, working or idle, gets one turn of this
# length to write down what it already knows, inside the wind-down its token and machine outlive
# the end of its run by (domain.AgentWindDown, 120 s), with a run check of core._RUN_CHECK_INTERVAL_S
# to spare. Jobs still in flight are left running and without a summary: nothing of theirs can be
# written yet. An agent whose own action ended its run owes nothing: its turn in flight gets this
# long to end by itself and no turn follows.
_SHUTDOWN_TURN_S = 60
# A coordinator's last turn writes the run's final summary, which takes reading the standings and
# the graph first: it is given most of domain.CoordinatorWindDown (300 s).
_COORDINATOR_SHUTDOWN_TURN_S = 240
_LAST_TURN = (
    "This is your last turn, about a minute long. Start nothing new and "
    "leave unfinished jobs running: they cannot be summarised yet. File what your finished jobs "
    "showed as their summaries{verdicts}, then end your turn. "
    "A refusal with reason `experiment_closed` ends it too: your run is over, so retry nothing."
)
_SHUTDOWN_PROMPT = _LAST_TURN.format(verdicts=" and set the verdicts your evidence already supports")
# A baseline agent registers no hypothesis, so it has no verdict to set.
_BASELINE_SHUTDOWN_PROMPT = _LAST_TURN.format(verdicts="")


_COORDINATOR_SHUTDOWN_PROMPT = (
    f"This is your last turn, {_COORDINATOR_SHUTDOWN_TURN_S // 60} minutes long. Start nothing new. "
    "Rewrite the summary as the record of the ended run (`hl platform-experiments summary --file FILE`), "
    "in the past tense, with nothing in it still under way, and where it says why the run ended, say "
    "what is stated above. The platform adds its own record of the close after it. Then end your turn."
)
# A planner has no job and no hypothesis, and what it owes is its proposal, which it proposes only
# finished. The session outlives the machine and resumes with a changed goal; a file does not.
_PLANNER_SHUTDOWN_PROMPT = (
    "This is your last turn, about a minute long. Start nothing new and propose nothing you have not finished. "
    "End your turn with where your proposal stands and what is left to do on it: a file you wrote on this "
    "machine is gone with it. A refusal with reason `experiment_closed` means the draft is closed, so retry nothing."
)


def turn_s(setup: core.RunSetup) -> int:
    return _COORDINATOR_SHUTDOWN_TURN_S if config.coordinates(setup.cfg) else _SHUTDOWN_TURN_S


async def _prompt(setup: core.RunSetup) -> str | None:
    """The last turn's instruction, opened with why the machine stops as the platform has it now.
    None for an agent whose own action ended its run: it gets no last turn."""
    cause = await core.stop_cause(setup)
    if cause is None:
        return None
    if config.coordinates(setup.cfg):
        return f"{cause} {_COORDINATOR_SHUTDOWN_PROMPT}"
    if config.plans(setup.cfg):
        return f"{cause} {_PLANNER_SHUTDOWN_PROMPT}"
    return f"{cause} {_BASELINE_SHUTDOWN_PROMPT if config.measures_baseline(setup.cfg) else _SHUTDOWN_PROMPT}"


async def interrupt_on_stop(client, setup: core.RunSetup, stopped) -> None:
    """Interrupts the turn in flight as soon as stopped() is true. The turn of an agent whose own
    action ended its run is its last and is interrupted only if it goes on past turn_s."""
    while not stopped():
        await asyncio.sleep(1)
    if await core.stop_cause(setup) is None:
        limit = turn_s(setup)
        log.info("stop requested after the agent ended its own run: its turn has %ss to end by itself", limit)
        await asyncio.sleep(limit)
    try:
        await client.interrupt()
    except Exception as e:
        log.warning("could not interrupt the turn (%s): it runs to its own end", e)


async def last_turn(client, setup: core.RunSetup, running: Running, watch_env: dict[str, str], handing: asyncio.Lock) -> None:
    """The shutdown turn, opened with whatever finished since the last delivery. An agent whose
    own action ended its run gets none."""
    prompt = await _prompt(setup)
    if prompt is None:
        log.info("stopping: the agent ended its own run, no last turn")
        return
    log.info("stopping: one last turn to file what is owed")
    agent = setup.cfg.agent_id
    async with handing:
        env = updates.taking(watch_env, "closing")
        events = await updates.drain(env)
        for line in activity.event_lines(events, agent):
            print(line, flush=True)
        events = running.not_own(events)
        await client.query(await prompts.turn_start(setup, watch_env, prompt + ("\n\n" + updates.message(events, agent) if events else "")))
        updates.delivered(env)
    limit = turn_s(setup)
    try:
        async with asyncio.timeout(limit):
            async for message in client.receive_response():
                stream.log_message(message, agent)
    except TimeoutError:
        log.info("last turn cut off after %ss", limit)
