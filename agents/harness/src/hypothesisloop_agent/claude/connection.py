"""One ClaudeSDKClient connection: its turns, and between them the wait for the next platform
updates, handed over on the same connection."""
from __future__ import annotations

import asyncio
import logging
import math
import time

from claude_agent_sdk import AssistantMessage, ClaudeSDKClient, ResultMessage

from hypothesisloop_agent import api_client, config, core, updates
from hypothesisloop_agent.claude import activity, cli, hooks, prompts, rotation, shutdown, stream, turns
from hypothesisloop_agent.claude.results import context_overflowed, context_tokens, credential_refused, request_refused
from hypothesisloop_agent.claude.running import PROMPT_REPLACED, Running
from hypothesisloop_agent.claude.store import Store

log = logging.getLogger("llm_agent")


async def run(setup: core.RunSetup, store: Store, running: Running, resume_id: str | None, stop_flag: list,
              initial_prompt: str | None, on_good_turn) -> bool:
    """Runs one ClaudeSDKClient connection until it ends (real stop, error, or rate limit).

    Returns whether the next connection starts at once: a handoff was written and the next session
    starts fresh from it, or the session is resumed with a new system prompt. A rate limit is read
    by the run loop from stream.refused_until. The session id and the handoff live on disk, see
    Store.restart_state.
    """
    cfg = setup.cfg
    agent = cfg.agent_id
    try:
        prompt_replaced = await running.connect(resume_id)
    except api_client.APIError as e:
        log.warning("the instructions were not read (%s): connecting again after the backoff", e)
        return False
    watch_env = updates.watch_env(cfg, setup.statedir)
    handing = asyncio.Lock()
    background: dict[str, dict] = {}
    subagents = hooks.Subagents(background)
    options = cli.options(setup, running.current, resume_id, hooks.matchers(setup, store, running, watch_env, handing, subagents))

    def stopped() -> bool:
        return stop_flag[0]

    async def over() -> bool:
        """Whether the run loop stops here, after the last turn when there is a platform to file
        with: a run it ended and a machine told to stop get one, a refused token gets none. The
        one place a last turn is given."""
        lost = await core.run_over(setup)
        if not lost and not stopped():
            return False
        log.info("stopping: %s", lost or "received signal")
        if not core.lost_platform(lost):
            await shutdown.last_turn(client, setup, running, watch_env, handing)
            await store.save()
        return True

    opening = await prompts.opening(setup, store, resume_id, initial_prompt)
    opening = await prompts.turn_start(setup, watch_env, PROMPT_REPLACED + "\n\n" + opening if prompt_replaced else opening)
    connecting = time.monotonic()
    told_nothing = False
    stream.progress.subagents(0)
    async with ClaudeSDKClient(options=options) as client:
        log.info("model CLI connected in %.1fs", time.monotonic() - connecting)
        await client.query(opening)
        while True:
            result: ResultMessage | None = None
            context = 0
            interrupter = asyncio.ensure_future(shutdown.interrupt_on_stop(client, setup, stopped))
            try:
                async for message in turns.receive(client):
                    store.seen(message)
                    stream.log_message(message, agent)
                    stream.note_background(message, background)
                    if isinstance(message, AssistantMessage) and message.usage and not message.parent_tool_use_id:
                        context = context_tokens(message.usage)
                    if isinstance(message, ResultMessage):
                        result = message
                        if message.is_error and credential_refused(message.result):
                            # Backing off cannot fix a credential the provider refuses. Exiting
                            # is what tells whoever started this agent, and with what words.
                            raise SystemExit(f"agent: the model provider refused this agent's credential: {(message.result or '').strip()[:300]}")
                        if request_refused(message):
                            raise SystemExit(f"agent: the model provider refused this agent's requests "
                                             f"({message.api_error_status}): {(message.result or '').strip()[:300]}")
                        resume_id = message.session_id
                        store.save_resume_id(resume_id)
                        if not message.is_error:
                            on_good_turn()
                            store.settled()
                            stream.limit_lifted()
                            if initial_prompt:
                                store.handed_over()
            finally:
                interrupter.cancel()
            if result:
                await store.save()

            # A refused agent can take no turn, the last one included: the run loop waits.
            if refused_until := stream.refused_until():
                log.info("stopping session: the model provider refuses calls until %s", refused_until)
                return False

            if result is None:
                # The stream ended without a ResultMessage — typically the CLI process itself
                # died mid-turn (e.g. an authentication or usage-limit error the SDK couldn't
                # turn into a clean ResultMessage). Treat as a transient error to retry, not a
                # reason to stop the whole agent: a quota outage must not end the run, it must
                # pause it.
                log.warning("stream ended without a ResultMessage — treating as transient")
                return False

            if await over():
                return False

            if result.is_error and context_overflowed(result):
                # The session no longer fits and cannot write its own handoff: the next one starts
                # fresh from what the agent last wrote, after the usual reconnect backoff.
                log.warning("the session overflowed its context: starting a fresh one from its last messages")
                await rotation.overflowed(store, resume_id)
                return False

            if result.is_error:
                # is_error=True with subtype="success" is the CLI's signal for an underlying
                # failure it couldn't otherwise surface (auth not set up, a bad API response,
                # ...) — e.g. "Not logged in · Please run /login". The turn still "completes"
                # instantly with no tool use, so falling through to the update wait would treat a
                # broken session as an idle agent. Route it through the same backoff as a
                # transient error instead.
                log.warning("turn completed with an underlying error (is_error) — treating as transient")
                return False

            if await running.apply(client, store, background, resume_id):
                return True

            rotate_at, hard_at = rotation.limits(result, running.current.model_id)
            if rotation.due(context, resume_id, rotate_at, hard_at, background):
                return await rotation.hand_off(client, store, agent, rotation.COMPACTION_PROMPT, background, resume_id)

            # The agent ended its turn. With no job of its own submitted, queued or running on the
            # platform, no background task the CLI lists and no subagent whose start it has not
            # listed yet, its wait waits for nothing: it is told so at every such end but the one
            # of the turn that answered the statement and while the platform takes no job of it yet, after which it waits and the platform's
            # recurring agent.idle is its timer. A read that failed says nothing.
            answered, told_nothing = told_nothing, False
            if (config.submits_jobs(cfg) and not answered and not background and not subagents.live()
                    and await updates.jobs_in_flight(watch_env) == 0 and not await core.takes_no_job_yet(setup)):
                log.info("nothing in flight at the end of the turn, saying so")
                hooks.forget_job_reads()
                await client.query(await prompts.turn_start(setup, watch_env, updates.NOTHING_IN_FLIGHT))
                told_nothing = True
                continue
            # An agent waits for an update with no limit, a coordinator until its review.
            wait_s = updates.REVIEW_S if config.coordinates(cfg) else math.inf
            # Wait for the next batch of updates about its own jobs and hypotheses and hand it
            # over on the same connection. A turn the CLI starts by itself ends the wait and is
            # read first: one starts when a subagent ends, and on a resume to say which stopped.
            # The batch the wait took is then read again, as its cursor moves only once handed over.
            while True:
                own = asyncio.ensure_future(turns.own_turn(client, store, agent, background))
                events, told, taken = await _next_told(setup, store, running, watch_env,
                                                       lambda: stopped() or own.done(), wait_s, handing)
                came = own.done()
                own.cancel()
                await asyncio.gather(own, return_exceptions=True)
                if not events or told or came or stopped() or not running.told:
                    break
                # A batch that said only that its own instructions changed: between turns is a
                # turn boundary, where they apply, and the wait goes on.
                async with handing:
                    updates.delivered(taken)
                if await running.apply(client, store, background, resume_id):
                    return True
            if came:
                hooks.forget_job_reads()
                continue
            # A batch that comes with the end of the run, the close among its events, opens the
            # last turn rather than a turn of its own that the stop then cuts short.
            if await over():
                return False
            if told:
                prompt = await prompts.turn_start(setup, watch_env, told)
                async with handing:
                    # A drain since the batch was taken belongs to a turn under way, which is
                    # read next, and handed the batch over or reads it again.
                    if updates.superseded(taken):
                        continue
                    for line in activity.event_lines(events, agent):
                        print(line, flush=True)
                    hooks.forget_job_reads()
                    await client.query(prompt)
                    updates.delivered(taken)
                continue
            log.info("nothing woke the session for %ds: its review", wait_s)
            hooks.forget_job_reads()
            standing = await updates.standing(watch_env)
            await client.query(await prompts.turn_start(setup, watch_env, updates.review_prompt(round(wait_s / 60), standing)))


async def _next_told(setup: core.RunSetup, store: Store, running: Running, watch_env: dict[str, str], ended,
                     idle_s: float, handing: asyncio.Lock) -> tuple[list[dict], str, dict[str, str]]:
    """The next batch of updates, what the agent is told of it, "" once ended(), and the env that
    took it, for updates.delivered. A batch that tells nothing held a changed goal answered since,
    which is passed and the wait goes on, or said that the agent's own instructions changed,
    which the session applies."""
    while True:
        taken = updates.taking(watch_env, "waiting")
        events = await updates.next_batch(taken, ended, idle_s)
        told = await prompts.told(setup, store, running, events) if events and not ended() else ""
        if told or not events or ended() or running.told:
            return events, told, taken
        async with handing:
            updates.delivered(taken)
