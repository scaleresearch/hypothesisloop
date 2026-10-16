"""Rotating a session into a fresh one that its handoff opens: when it is due, and the handoff."""
from __future__ import annotations

import logging

from claude_agent_sdk import ResultMessage

from hypothesisloop_agent import config, session
from hypothesisloop_agent.claude import cli, stream, transcripts, turns
from hypothesisloop_agent.claude.store import Store

log = logging.getLogger("llm_agent")

# Left at the model's window the CLI's auto-compaction never fired (a production transcript reached
# 650K cache-read tokens with zero compact boundaries), and the SDK has no compact method, so a
# session is checkpointed into a fresh, non-resumed one at a fixed threshold. A lead with no
# subagent live rotates with the platform's handoff before the CLI compacts it; with one live, the
# CLI's compaction keeps the subagents a rotation would stop. The last assistant message's usage
# measures the context of the latest model call; ResultMessage usage sums every call in the turn
# and overstates it many times over.
# Below a smaller model window, the session rotates at this part of the one the CLI reports.
_CONTEXT_WINDOW_SHARE = 0.75
_CONTEXT_COMPACTION_THRESHOLD_TOKENS = (min(400_000, int(cli.AUTO_COMPACT_WINDOW_TOKENS * _CONTEXT_WINDOW_SHARE))
                                        if cli.AUTO_COMPACT_WINDOW_TOKENS else 400_000)
# A transcript this large rotates too, so that the session still fits what the platform stores.
_TRANSCRIPT_ROTATION_BYTES = session.MAX_BYTES * 3 // 4
# A rotation ends the CLI and with it every subagent at work, which a lead then starts again. While
# one is live the session goes on past the points above, up to the ceiling or this part of the
# window, or of what the platform stores.
_HARD_SHARE = 0.9
_TRANSCRIPT_HARD_BYTES = int(session.MAX_BYTES * _HARD_SHARE)
# Every model call reads the session's whole context again. Researchers whose subagents were live
# at nearly every turn end carried 0.7-0.85M tokens into each call, about 50M cache-read tokens an
# hour each, and spent the account's five-hour allowance in three.
_CONTEXT_CEILING_TOKENS = config.tuning("HL_CONTEXT_CEILING_TOKENS", 500_000)
# Ends the handoff of a session rotated with subagents at work.
_STOPPED_SUBAGENTS = (
    "These subagents were at work when that session ended and stopped with it before they completed:\n"
    "{tasks}\nStart each again with Agent from where its hypothesis, branch and jobs stand: its jobs ran on.")
_HANDOFF_PROMPT = (
    "We must rotate to a fresh session because {why}. "
    "Produce a concise but complete handoff for the next instance of yourself: current platform "
    "experiment state, hypotheses and evidence, actions/results so far, important files or jobs, "
    "decisions and rejected paths, any message from the coordinator or the owner not yet acted on, "
    "word for word, each subagent still at work, which stops with this session, with its hypothesis, "
    "worktree, branch, jobs and how far it got, and the exact next useful steps. Do not do more research or "
    "use tools; output only the handoff."
)
COMPACTION_PROMPT = _HANDOFF_PROMPT.format(why="this session has reached its context budget")
# A session keeps the model it started with, so new instructions that change it start a fresh session.
INSTRUCTIONS_ROTATION = _HANDOFF_PROMPT.format(
    why="the instructions you run with changed, and the next session runs with the new ones")


def limits(message: ResultMessage, model: str) -> tuple[int, int]:
    """The context at which the session rotates with no subagent live, the fixed threshold or less
    for a model whose window, as the CLI reports it in the turn's result, is smaller, and the
    context at which it rotates whatever is live, the ceiling or less for such a window."""
    windows = {name: int(usage.get("contextWindow") or 0) for name, usage in (message.model_usage or {}).items()
               if isinstance(usage, dict)}
    window = next((w for name, w in windows.items() if name.startswith(model) and w), max(windows.values(), default=0))
    soft, hard = _CONTEXT_COMPACTION_THRESHOLD_TOKENS, _CONTEXT_CEILING_TOKENS
    if window:
        soft, hard = min(soft, int(window * _CONTEXT_WINDOW_SHARE)), min(hard, int(window * _HARD_SHARE))
    return min(soft, hard), hard


def due(context_tokens: int, session_id: str | None, rotate_at: int, hard_at: int, background: dict[str, dict]) -> bool:
    """Whether the session rotates at the end of this turn. With a background task live it goes on
    to the hard limits."""
    transcript_bytes = transcripts.size(session_id)
    if context_tokens < rotate_at and transcript_bytes < _TRANSCRIPT_ROTATION_BYTES:
        return False
    if background and context_tokens < hard_at and transcript_bytes < _TRANSCRIPT_HARD_BYTES:
        log.info("rotation deferred: %d subagents live (context ~%d tokens, transcript %d bytes)",
                 len(background), context_tokens, transcript_bytes)
        return False
    log.info("context reached ~%d tokens, transcript %d bytes: creating handoff before fresh session",
             context_tokens, transcript_bytes)
    return True


def _stopped_subagents(background: dict[str, dict]) -> str:
    """What the next session is told of the subagents a rotation stops, "" for none."""
    tasks = [f"- {task.get('description') or 'a subagent'}" for task in background.values()
             if stream.is_subagent(task)]
    return "\n\n" + _STOPPED_SUBAGENTS.format(tasks="\n".join(tasks)) if tasks else ""


async def hand_off(client, store: Store, agent: str, prompt: str, background: dict[str, dict], resume_id: str | None) -> bool:
    """Asks the lead for its handoff with prompt and makes it open the next session, which starts
    fresh, with the subagents that stop with this one named. Whether it did: a handoff that failed
    or came back empty keeps this session, for another try."""
    await client.query(prompt)
    handoff_result = await turns.answer(client, agent, prompt, background)
    if not handoff_result or handoff_result.is_error:
        log.warning("handoff failed — preserving old session for retry")
        return False
    handoff = (handoff_result.result or "").strip()
    if not handoff:
        log.warning("handoff was empty — preserving old session for retry")
        return False
    await store.rotate(handoff + _stopped_subagents(background), resume_id)
    return True


# How much of what the agent last wrote opens the session that replaces one that overflowed.
_OVERFLOW_HANDOFF_CHARS = 20_000
_OVERFLOW_HANDOFF = (
    "Your last session ran out of context before it could write a handoff. These are the last "
    "things you wrote in it, oldest first:\n\n{tail}\n\n"
    "Look at the state of your jobs and hypotheses with hl before you go on.")


async def overflowed(store: Store, session_id: str | None) -> None:
    """Rotates a session that overflowed into a fresh one, which the agent's own last words in its
    transcript open: it can write no handoff."""
    tail, size = [], 0
    for text in reversed(transcripts.lead_texts(session_id)):
        if size + len(text) > _OVERFLOW_HANDOFF_CHARS:
            if not tail:
                tail.append(text.strip()[-_OVERFLOW_HANDOFF_CHARS:])
            break
        tail.insert(0, text.strip())
        size += len(text)
    await store.rotate(_OVERFLOW_HANDOFF.format(tail="\n\n".join(tail) or "(nothing)"), session_id)
