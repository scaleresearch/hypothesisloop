"""Reading the turns of a connection: the watchdog on a turn the CLI went silent in or a model call
that stalled, the answer to a prompt, and the turns the CLI starts by itself."""
from __future__ import annotations

import asyncio
import logging

from claude_agent_sdk import CLIConnectionError, ProcessError, ResultMessage, StreamEvent, SystemMessage, UserMessage

from hypothesisloop_agent import progress
from hypothesisloop_agent.claude import stream
from hypothesisloop_agent.claude.store import Store

log = logging.getLogger("llm_agent")

# Longest a model call may go without a message once it streams, from its message_start to its
# last stream event. A healthy call sends one at least every 1.5 s, thinking included (an empty
# thinking_delta per ~50 tokens, measured on the bundled CLI); one call sat 6 min on 2790 tokens.
# A stalled call is interrupted and asked for again once. message_start comes with the first
# token, after the prefill of the whole context, so before it stream.TURN_SILENCE_TIMEOUT_S bounds
# the call: resumed 650-710K sessions with a cold cache were silent past 120 s there, every time.
_CALL_STALL_S = 120
_STALL_PROMPT = f"Your last model call sent nothing for {_CALL_STALL_S} s and was interrupted. Go on from where it stopped."


class TurnStalled(Exception):
    pass


def transport_error(error: Exception) -> bool:
    """Whether error is a lost connection to the CLI, which a new session repairs. The SDK
    raises a CLI that died mid-stream, and a control request it never answered, as Exception
    itself, so that exact class counts and no subclass of it does."""
    return isinstance(error, (CLIConnectionError, ProcessError, TurnStalled)) or type(error) is Exception


def _in_call(message, in_call: bool) -> bool:
    """Whether a model call streams once message has arrived."""
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


async def receive(client):
    """client.receive_response(), failing with TurnStalled when the CLI goes silent, or when a
    model call stalls a second time."""
    messages = client.receive_response().__aiter__()
    stream.progress.doing(progress.THINKING)
    stream.progress.awaiting()
    in_call = retried = False
    try:
        while True:
            limit = _CALL_STALL_S if in_call else stream.TURN_SILENCE_TIMEOUT_S
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
                stream.progress.awaiting()
    finally:
        stream.progress.resting()
        stream.progress.doing(progress.WAITING)


async def answer(client, agent: str, prompt: str, background: dict[str, dict]) -> ResultMessage | None:
    """The result of the turn that took up prompt, None for a stream that ended first. A subagent
    that ends as prompt is sent starts a turn of its own that can come first: it is read and passed."""
    while True:
        took = False
        result = None
        async for message in receive(client):
            stream.log_message(message, agent)
            stream.note_background(message, background)
            took = took or (isinstance(message, UserMessage) and message.content == prompt)
            if isinstance(message, ResultMessage):
                result = message
        if took or result is None:
            return result


async def own_turn(client, store: Store, agent: str, background: dict[str, dict]) -> None:
    """Returns once the CLI starts a turn by itself, or its stream ends. It starts one when a
    background task ends, and on a resume it answers the opening with a turn of its own, without
    a model call, for the subagents that stopped with the last process before the opening's turn.
    What the subagents do meanwhile is logged."""
    async for message in client.receive_messages():
        store.seen(message)
        stream.log_message(message, agent)
        stream.note_background(message, background)
        if isinstance(message, SystemMessage) and message.subtype == "init":
            return
