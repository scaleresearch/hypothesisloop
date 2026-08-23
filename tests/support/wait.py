"""One deadline per test, nothing else (tests/improve.md #2.1).

Every wait inside a test is `eventually(...)` against that test's shared `deadline` fixture — no
per-wait tries/sleep budget. A failing test spends up to the deadline to fail with the most useful
message it can produce; the passing path is unchanged since eventually() returns the moment its
probe is satisfied.
"""
from __future__ import annotations

import time
from dataclasses import dataclass
from typing import Callable, TypeVar

T = TypeVar("T")


class DeadlineExceeded(AssertionError):
    pass


@dataclass
class Deadline:
    """A monotonic budget shared by every wait inside one test."""

    ends_at: float

    @classmethod
    def in_seconds(cls, seconds: float) -> "Deadline":
        return cls(ends_at=time.monotonic() + seconds)

    def remaining(self) -> float:
        return max(0.0, self.ends_at - time.monotonic())

    def expired(self) -> bool:
        return time.monotonic() >= self.ends_at


def eventually(
    description: str,
    probe: Callable[[], T],
    *,
    deadline: Deadline,
    accept: Callable[[T], bool] | None = None,
    reject: Callable[[T], bool] | None = None,
    interval: float = 0.25,
) -> T:
    """Poll `probe` until `accept` is true (or, with no `accept`, until it returns without
    raising), failing immediately if `reject` is true of an observed value, and otherwise failing
    once `deadline` is exhausted.

    Named for the log line it produces on failure: it names the condition being waited for and the
    last observed value/error, so a timeout reads as "X never happened, last saw Y" rather than a
    bare assertion failure deep inside a helper.
    """
    last_value: T | None = None
    last_error: Exception | None = None
    first = True
    while first or not deadline.expired():
        first = False
        try:
            last_value = probe()
            last_error = None
        except Exception as exc:  # noqa: BLE001 - surfaced in the failure message, not swallowed
            last_error = exc
            last_value = None
        else:
            ok = accept(last_value) if accept is not None else True
            if ok:
                return last_value
            if reject is not None and reject(last_value):
                raise DeadlineExceeded(
                    f"{description}: reached a terminal condition that is not the one waited for "
                    f"(last observed: {last_value!r})"
                )
        if deadline.expired():
            break
        time.sleep(interval)

    if last_error is not None:
        raise DeadlineExceeded(f"{description}: timed out, last probe raised: {last_error!r}") from last_error
    raise DeadlineExceeded(f"{description}: timed out, last observed: {last_value!r}")


def assert_stable(
    description: str,
    probe: Callable[[], T],
    *,
    ok: Callable[[T], bool],
    duration: float,
    interval: float = 1.0,
) -> None:
    """Polls `probe` every `interval` seconds for `duration` seconds (a fixed sleep is correct
    here -- tests/improve.md's "waiting without flakiness": elapsed time *is* the behavior under
    test), asserting `ok` holds on every observation. Fails immediately, naming the first bad
    observation, rather than only checking the final one."""
    end = time.monotonic() + duration
    while True:
        value = probe()
        assert ok(value), f"{description}: unstable, observed {value!r}"
        if time.monotonic() >= end:
            return
        time.sleep(interval)
