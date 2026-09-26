from __future__ import annotations

import asyncio
import time

# The line the platform reads this agent's health from. The launcher that runs the agent looks
# for the newest one in its output and hands the marker after the word to the platform, which
# ends a machine whose marker stops changing (agents.progress_limit_seconds in its config).
PREFIX = "hl-progress"
# domain.AgentProgressIntervalSeconds on the platform's side.
INTERVAL_S = 15


class Progress:
    """Prints a new marker every INTERVAL_S for as long as the harness is doing what it should.

    The beat runs on the harness's own event loop, so a frozen process or a blocked loop prints
    nothing. While a turn is in flight it also stops once the model session has been silent for
    longer than quiet_limit_s: the harness's own watchdog should have ended that turn by then,
    and a harness whose watchdog did not is hung. Waiting between turns is no silence.
    """

    def __init__(self, quiet_limit_s: float, clock=time.monotonic, emit=None):
        self._quiet_limit_s = quiet_limit_s
        self._clock = clock
        self._emit = emit or (lambda line: print(line, flush=True))
        self._awaiting_since: float | None = None
        self._count = 0

    def awaiting(self) -> None:
        """A turn is in flight and the next message from the model session is awaited from now."""
        self._awaiting_since = self._clock()

    def resting(self) -> None:
        """No turn is in flight."""
        self._awaiting_since = None

    def healthy(self) -> bool:
        return self._awaiting_since is None or self._clock() - self._awaiting_since <= self._quiet_limit_s

    def beat(self) -> None:
        if self.healthy():
            self._count += 1
            self._emit(f"{PREFIX} {self._count}")

    async def run(self) -> None:
        while True:
            self.beat()
            await asyncio.sleep(INTERVAL_S)
