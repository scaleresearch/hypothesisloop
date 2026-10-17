from __future__ import annotations

import asyncio
import time

from hypothesisloop_agent import platform

# The line the platform reads this agent's health from. The launcher that runs the agent looks
# for the newest one in its output and hands the marker after the word to the platform, which
# ends a machine whose marker stops changing (agents.progress_limit_seconds in its config).
PREFIX = platform.PROGRESS_PREFIX
INTERVAL_S = platform.PROGRESS_INTERVAL_S
# The line that says what the agent is doing now. The launcher hands the newest to the platform
# with the marker, and the platform shows it beside the agent: the latest sentence and nothing
# before it. It is printed with every beat, so the newest is never far back in the output, and
# when it changes, at most every ACTIVITY_INTERVAL_S.
ACTIVITY_PREFIX = platform.ACTIVITY_PREFIX
ACTIVITY_INTERVAL_S = 3
MAX_ACTIVITY_LENGTH = platform.MAX_ACTIVITY_LENGTH
# The line that carries what the harness observes of its agent, printed with every beat: how many
# subagents are live, whether a turn is in flight (working) or the agent waits for an update, since
# when (Unix seconds), and the model provider's rate limit as the CLI last said it, once there is a
# reading, and the digest of the instructions it runs. The launcher hands the newest to the platform, which keeps it in the run's metrics
# (launcherapi.parseHarnessState reads it).
STATE_PREFIX = platform.STATE_PREFIX
# Characters of the instructions digest the state line carries: the launcher keeps 200 characters
# of it, and 12 hex characters are 48 bits, which the platform's metrics hold exactly.
DIGEST_LENGTH = 12

THINKING = "thinking"
WAITING = "waiting for platform updates"


def state_line(subagents: int, working: bool, since: float, digest: str, version: int, model: dict[str, object]) -> str:
    """The state line: subagents, turn and since, the instructions_digest and the version of the
    agent type they are once the harness runs instructions, then the model_ fields of model that
    are known."""
    fields = {"subagents": subagents, "turn": "working" if working else "waiting", "since": int(since),
              **({"instructions_digest": digest[:DIGEST_LENGTH], "version": version} if digest else {}),
              **{f"model_{k}": v for k, v in model.items() if v is not None}}
    return " ".join([STATE_PREFIX] + [f"{k}={v}" for k, v in fields.items()])


def sentence(text: str) -> str:
    """text as one printable line of at most MAX_ACTIVITY_LENGTH characters."""
    text = " ".join("".join(c if c.isprintable() else " " for c in text).split())
    return text if len(text) <= MAX_ACTIVITY_LENGTH else text[:MAX_ACTIVITY_LENGTH - 3] + "..."


def announce(text: str) -> None:
    """Says text as what the agent is doing, at once: the next beat says the agent's activity again."""
    print(f"{ACTIVITY_PREFIX} {sentence(text)}", flush=True)


class Progress:
    """Prints a new marker every INTERVAL_S for as long as the harness is doing what it should,
    and beside it what the agent is doing.

    The beat runs on the harness's own event loop, so a frozen process or a blocked loop prints
    nothing. While a turn is in flight it also stops once the model session has been silent for
    longer than quiet_limit_s: the harness's own watchdog should have ended that turn by then,
    and a harness whose watchdog did not is hung. Waiting between turns is no silence.
    """

    def __init__(self, quiet_limit_s: float, clock=time.monotonic, emit=None, wall=time.time):
        self._quiet_limit_s = quiet_limit_s
        self._clock = clock
        self._wall = wall
        self._emit = emit or (lambda line: print(line, flush=True))
        self._awaiting_since: float | None = None
        self._count = 0
        self._activity = ""
        self._trouble = ""
        self._said = ""
        self._said_at: float | None = None
        self._subagents = 0
        self._turn_since = wall()
        self._digest = ""
        self._version = 0
        self._model: dict[str, object] = {}

    def awaiting(self) -> None:
        """A turn is in flight and the next message from the model session is awaited from now."""
        if self._awaiting_since is None:
            self._turn_since = self._wall()
        self._awaiting_since = self._clock()

    def resting(self) -> None:
        """No turn is in flight."""
        if self._awaiting_since is not None:
            self._turn_since = self._wall()
        self._awaiting_since = None

    def waited(self) -> float:
        """How long the turn in flight has awaited its next message, 0 with none in flight."""
        return 0.0 if self._awaiting_since is None else self._clock() - self._awaiting_since

    def healthy(self) -> bool:
        return self._awaiting_since is None or self._clock() - self._awaiting_since <= self._quiet_limit_s

    def doing(self, text: str) -> None:
        """What the agent is doing from now on, in a few words that hold nothing secret: said at
        once when the last sentence is ACTIVITY_INTERVAL_S old, and with the next beat otherwise."""
        self._activity = sentence(text)
        if self._sentence() != self._said and (self._said_at is None or self._clock() - self._said_at >= ACTIVITY_INTERVAL_S):
            self._say()

    def trouble(self, text: str) -> None:
        """What is wrong with the harness itself while the agent works on, "" for nothing: said
        ahead of what the agent is doing, from the next sentence on, by the same rule on what it
        may hold."""
        self._trouble = text

    def subagents(self, live: int) -> None:
        """How many subagents of the agent are live from now on."""
        self._subagents = live

    def instructions(self, digest: str, version: int) -> None:
        """The digest of the instructions the agent runs from now on, and the version of its agent
        type they are."""
        self._digest, self._version = digest, version

    def model(self, status: str, kind: str | None, utilization: float | None, resets_at: int | None) -> None:
        """The model provider's rate limit as the CLI said it last."""
        self._model = {"status": status, "type": kind, "utilization": utilization, "resets_at": resets_at}

    def _sentence(self) -> str:
        return sentence(f"{self._trouble}. {self._activity}") if self._trouble else self._activity

    def _say(self) -> None:
        said = self._sentence()
        if said:
            self._emit(f"{ACTIVITY_PREFIX} {said}")
            self._said, self._said_at = said, self._clock()

    def beat(self) -> None:
        if self.healthy():
            self._count += 1
            self._emit(f"{PREFIX} {self._count}")
            self._say()
            self._emit(state_line(self._subagents, self._awaiting_since is not None, self._turn_since, self._digest, self._version, self._model))

    async def run(self) -> None:
        while True:
            self.beat()
            await asyncio.sleep(INTERVAL_S)
