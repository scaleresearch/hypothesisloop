"""The agent's session as the harness keeps it: which session to resume or the handoff that opens
the next one, in the state directory, and the whole of it with the platform, so that it outlives
the machine."""
from __future__ import annotations

import asyncio
import logging
import os
import time
from pathlib import Path

from hypothesisloop_agent import core, instructions, session, updates
from hypothesisloop_agent.claude import stream, transcripts

log = logging.getLogger("llm_agent")

_SESSION_ID_FILE = ".claude_session_id"
_HANDOFF_FILE = ".claude_compaction_handoff"
# The session a turn is in flight in, known from its first message on. It travels with the
# snapshot, so a machine lost in the middle of a turn, the first one included, is resumed there.
_LIVE_SESSION_FILE = ".claude_live_session_id"
# How often a session is handed to the platform while a turn is in flight. A turn can last as
# long as a job does, and nothing unchanged is sent.
_STORE_INTERVAL_S = 30
# How long stores fail in a row before the agent's activity line says so: a platform that is
# away for a moment is no news.
_NOT_STORED_SAID_S = 90


def _read(path: str) -> str | None:
    try:
        with open(path) as f:
            return f.read().strip() or None
    except FileNotFoundError:
        return None


def _write(path: str, text: str) -> None:
    with open(path, "w") as f:
        f.write(text)


def _remove(path: str) -> None:
    Path(path).unlink(missing_ok=True)


class Store:
    def __init__(self, setup: core.RunSetup):
        self._statedir = setup.statedir
        self._sessions = session.Sessions(setup.client, setup.cfg, setup.statedir, transcripts.model_home())
        self._live: str | None = None
        self._not_stored_since: float | None = None
        self._lock = asyncio.Lock()
        # Sessions rotated away, whose transcripts are deleted once the CLI has exited.
        self._retired: set[str] = set()
        # Said first to a session that was restored on a machine it did not run on, until a turn
        # there has ended well. The conversation remembers files this machine never had.
        self.on_new_machine = False

    def _path(self, name: str) -> str:
        return os.path.join(self._statedir, name)

    def resume_id(self) -> str | None:
        return _read(self._path(_SESSION_ID_FILE))

    def save_resume_id(self, session_id: str) -> None:
        _write(self._path(_SESSION_ID_FILE), session_id)

    def handoff(self) -> str | None:
        return _read(self._path(_HANDOFF_FILE))

    def handed_over(self) -> None:
        """The handoff opened a session that has ended a turn well: it is given no more."""
        _remove(self._path(_HANDOFF_FILE))

    def seen(self, message) -> None:
        """Notes the session a message belongs to as the one in flight."""
        session_id = getattr(message, "session_id", None)
        if session_id is None and isinstance(getattr(message, "data", None), dict):
            session_id = message.data.get("session_id")
        if session_id and session_id != self._live:
            self._live = session_id
            _write(self._path(_LIVE_SESSION_FILE), session_id)

    def restart_state(self) -> tuple[str | None, str | None]:
        """The session to resume and the prompt that opens the next connection, as persisted.

        The session resumed is the newest one stored: the one in flight once its transcript holds
        what the turn did, else the one saved at the last result. An executor that lost its
        first turn began again from nothing, and the session in flight holds more than the saved one.
        A session id beside a handoff means the replacement session already produced a result, so it
        is resumed and the bridge is not injected twice.
        """
        in_flight = self._live if transcripts.files(self._live) else None
        resume_id = in_flight or self.resume_id()
        handoff = self.handoff()
        if resume_id or not handoff:
            return resume_id, None
        return None, "Continue this experiment from the following handoff:\n\n" + handoff

    async def restore(self) -> None:
        """On a machine that holds no session of its own, takes the one the platform keeps."""
        if self.resume_id() or self.handoff():
            return
        if not await self._sessions.restore():
            return
        self.on_new_machine = True
        live = _read(self._path(_LIVE_SESSION_FILE))
        if not self.resume_id() and live:
            # The machine was lost before the turn in flight ended: that session is the one to
            # resume, and a handoff beside it was already given to it.
            self.save_resume_id(live)
        log.info("resuming session %s on a new machine", self.resume_id() or "from its handoff")

    def settled(self) -> None:
        self.on_new_machine = False

    async def save(self) -> None:
        """Hands the session as it stands to the platform, unless it already holds this one."""
        statedir = Path(self._statedir)
        files = [_SESSION_ID_FILE, _HANDOFF_FILE, _LIVE_SESSION_FILE, instructions.FILE]
        files += [str(path.relative_to(statedir)) for path in (statedir / updates.STATE_DIR).rglob("*") if path.is_file()]
        names = {name for session_id in (self.resume_id(), self._live) for name in transcripts.files(session_id)}
        async with self._lock:
            try:
                await self._sessions.save(files, sorted(names))
            except session.NotStored as why:
                self._not_stored(str(why))
            else:
                self._not_stored("")

    def _not_stored(self, why: str) -> None:
        """Notes a store that failed for why, "" for one that did not. The agent works on and the
        next interval stores again. A failure that persists is said in the agent's activity line
        until a store succeeds: a replacement machine would resume from an older session, silently."""
        if not why:
            self._not_stored_since = None
            stream.progress.trouble("")
            return
        if self._not_stored_since is None:
            self._not_stored_since = time.monotonic()
        lasted = time.monotonic() - self._not_stored_since
        persists = lasted >= _NOT_STORED_SAID_S
        log.log(logging.ERROR if persists else logging.WARNING,
                "session not stored for %.0fs: %s. A replacement machine would resume from the last one stored. Storing again in %ds",
                lasted, why, _STORE_INTERVAL_S)
        if persists:
            stream.progress.trouble(f"session not stored: {why}")

    async def keep_saved(self) -> None:
        while True:
            await asyncio.sleep(_STORE_INTERVAL_S)
            if self._live or self._not_stored_since is not None:
                await self.save()

    async def rotate(self, handoff: str, old_session_id: str | None) -> None:
        """Makes handoff open the next session, which starts fresh. The handoff is persisted before
        the old id is removed: if the worker dies before the fresh session returns its first
        ResultMessage, startup retries this handoff, and only that successful result removes it."""
        _write(self._path(_HANDOFF_FILE), handoff)
        _remove(self._path(_SESSION_ID_FILE))
        self._live = None
        _remove(self._path(_LIVE_SESSION_FILE))
        if old_session_id:
            self._retired.add(old_session_id)
        await self.save()

    def delete_retired(self) -> None:
        """Deletes the transcripts of the sessions rotated away, once the CLI has exited."""
        for session_id in list(self._retired):
            transcripts.delete(session_id)
            self._retired.discard(session_id)
