"""The one clock of a platform outage as this process sees it.

core.run_over, the read of whether the run goes on, is all that starts and ends it. hl exits 1
for a refused token, a refusal and a platform that is down alike, so a poll of hl that failed
only asks for that read now (suspect).
"""
from __future__ import annotations

import asyncio
import time

_since: float | None = None
_over: tuple[float, float] | None = None
_suspected = False


def unanswered() -> None:
    """Notes a read the platform left unanswered."""
    global _since
    if _since is None:
        _since = time.time()


def answered() -> None:
    global _since, _over
    if _since is not None:
        _since, _over = None, (_since, time.time())


def under_way() -> bool:
    return _since is not None


def _clock(at: float) -> str:
    return time.strftime("%H:%M:%S", time.gmtime(at))


def over() -> list[dict]:
    """The event that tells the agent an outage has ended, handed out once. One refused by a
    platform that could not be reached ended its turn and, with no job in flight, heard nothing
    until the idle nudge."""
    global _over
    ended, _over = _over, None
    if ended is None:
        return []
    return [{"kind": "platform.reachable", "value": f"again: it could not be reached from {_clock(ended[0])} to "
                                                     f"{_clock(ended[1])} UTC. Retry what was refused then."}]


def suspect() -> None:
    global _suspected
    _suspected = True


async def next_read(interval_s: float) -> None:
    """Returns when the platform is to be read again: after interval_s, or once a poll of hl
    failed."""
    global _suspected
    deadline = time.monotonic() + interval_s
    while not _suspected and time.monotonic() < deadline:
        await asyncio.sleep(min(1.0, interval_s))
    _suspected = False
