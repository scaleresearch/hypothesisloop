"""Pacing the model allowance of the account an agent runs on, which its other agents share.

The provider counts use in windows that reset at a stated time. Three agents on one subscription
spent its five-hour window in about three hours and were refused calls for the rest of it. An
agent whose window is used further than its elapsed share, by more than MARGIN, is ahead of the
pace that lasts the window: it starts fewer subagents, a coordinator reviews less often, and each
wake says so.
"""
from __future__ import annotations

import datetime
import time
from dataclasses import dataclass

from hypothesisloop_agent import config

# The windows the CLI names, by length.
WINDOW_S = {"five_hour": 5 * 3600, "seven_day": 7 * 86400, "seven_day_opus": 7 * 86400,
            "seven_day_sonnet": 7 * 86400, "seven_day_overage_included": 7 * 86400}
# Below this share used no window is paced: early in a window a little use is far ahead of its
# share of time.
FLOOR = config.tuning("HL_PACING_FLOOR", 0.3)
MARGIN = config.tuning("HL_PACING_MARGIN", 0.1)
# The most subagents an agent runs at once while ahead, and a coordinator's review interval.
SUBAGENTS = config.tuning("HL_PACING_SUBAGENTS", 2)
REVIEW_S = config.tuning("HL_PACING_REVIEW_S", 3600)

NOTE = ("Model allowance: the model account you share with the run's other agents has used {used:.0%} of its "
        "{window} window with {gone:.0%} of the window gone, ahead of the pace that lasts until it resets at "
        "{resets} UTC. Calls past the allowance are refused until then. Until it is back on pace, at most "
        "{subagents} subagents run at once: let those at work finish, give each a whole idea, and end a turn "
        "that has nothing to act on.")
COORDINATOR_NOTE = (" Your regular review comes every {minutes} minutes until then, and a researcher with "
                    "fewer subagents than its room allows is pacing, not idle.")


@dataclass(frozen=True)
class Reading:
    window: str
    utilization: float
    resets_at: float

    def gone(self, now: float) -> float:
        """The share of the window that has passed at now."""
        return min(1.0, max(0.0, 1 - (self.resets_at - now) / WINDOW_S[self.window]))

    def ahead(self, now: float) -> float:
        return self.utilization - self.gone(now)


class Allowance:
    """The latest reading of each window of the account."""

    def __init__(self, clock=time.time):
        self._clock = clock
        self._readings: dict[str, Reading] = {}

    def read(self, window: str | None, utilization: float | None, resets_at: float | None) -> None:
        if window in WINDOW_S and utilization is not None and resets_at:
            self._readings[window] = Reading(window, float(utilization), float(resets_at))

    def read_usage(self, usage: dict) -> None:
        """Reads the answer to the CLI's get_usage: utilization in percent, resets_at in ISO 8601."""
        for window, limit in ((usage or {}).get("rate_limits") or {}).items():
            if not isinstance(limit, dict) or limit.get("utilization") is None or not limit.get("resets_at"):
                continue
            try:
                resets_at = datetime.datetime.fromisoformat(str(limit["resets_at"])).timestamp()
                self.read(window, float(limit["utilization"]) / 100, resets_at)
            except (TypeError, ValueError):
                continue

    def furthest(self) -> Reading | None:
        """The live reading furthest ahead of its pace, None for none."""
        now = self._clock()
        live = [r for r in self._readings.values() if r.resets_at > now]
        return max(live, key=lambda r: r.ahead(now), default=None)

    def pacing(self) -> Reading | None:
        """The reading that runs ahead of its pace, None while every window is on pace."""
        reading = self.furthest()
        if reading and reading.utilization >= FLOOR and reading.ahead(self._clock()) > MARGIN:
            return reading
        return None

    def note(self, coordinator: bool) -> str:
        """What a wake says of the allowance while it is paced, "" while it is not."""
        reading = self.pacing()
        if reading is None:
            return ""
        said = NOTE.format(used=reading.utilization, window=reading.window, gone=reading.gone(self._clock()),
                           resets=time.strftime("%Y-%m-%d %H:%M", time.gmtime(reading.resets_at)), subagents=SUBAGENTS)
        return said + (COORDINATOR_NOTE.format(minutes=REVIEW_S // 60) if coordinator else "")

    def cap(self) -> tuple[int, str] | None:
        """The subagents that run at once while paced, and why, None while not."""
        reading = self.pacing()
        if reading is None:
            return None
        return SUBAGENTS, (f" while the model allowance runs ahead of its pace ({reading.utilization:.0%} of its "
                           f"{reading.window} window used, {reading.gone(self._clock()):.0%} of it gone)")
