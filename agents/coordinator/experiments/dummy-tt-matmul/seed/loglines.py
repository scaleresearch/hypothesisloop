"""Shared rolling-log-tail helper for job processes (harness.py, train.py, or any hand-written
job script that wants the same behavior).

Push-only, by design: the job process is the only thing that ever talks to the control plane
about its own logs (same direction as metric posts) -- the control plane never reaches into a
cluster to read a pod's stdout. This module keeps a small in-process ring buffer of the job's
own last N print()'d lines and POSTs the whole buffer to
`{REGISTRY_URL}/registry/experiments/{id}/logs` on demand, replacing (not appending to) whatever
was stored before. Once the job stops calling push(), whatever was last pushed is what stays
visible -- there is no accumulating archive.

Usage:
    from loglines import LogTail
    log_tail = LogTail()          # tees stdout+stderr into a ring buffer, size from
                                   # LOG_TAIL_LINES env var (default 100)
    ...                            # normal print()s throughout the script are captured
    log_tail.push()                # call this at the same cadence you call post_metric()
"""

import os
import io
import sys
import json
import urllib.request
from collections import deque

DEFAULT_LOG_TAIL_LINES = 100


class _TeeStream(io.TextIOBase):
    """Wraps an underlying stream, writing through to it while also feeding a ring buffer."""

    def __init__(self, underlying, buf: deque):
        self._underlying = underlying
        self._buf = buf
        self._partial = ""

    def write(self, s: str) -> int:
        self._underlying.write(s)
        self._partial += s
        while "\n" in self._partial:
            line, self._partial = self._partial.split("\n", 1)
            self._buf.append(line)
        return len(s)

    def flush(self) -> None:
        self._underlying.flush()


class LogTail:
    """Tees stdout and stderr into a bounded ring buffer and pushes it to the registry.

    max_lines defaults to the LOG_TAIL_LINES env var (configurable on the agent/job side, per
    the experiment's design) so a job can keep more or fewer lines without any code change.
    """

    def __init__(self, *, max_lines: int | None = None, experiment_id: str | None = None,
                 registry_url: str | None = None, tee_stdio: bool = True):
        self.max_lines = max_lines or int(os.environ.get("LOG_TAIL_LINES", DEFAULT_LOG_TAIL_LINES))
        self.experiment_id = experiment_id or os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
        self.registry_url = registry_url or os.environ.get("HYPOTHESISLOOP_REGISTRY_URL", "")
        self._buf: deque = deque(maxlen=self.max_lines)
        if tee_stdio:
            sys.stdout = _TeeStream(sys.stdout, self._buf)
            sys.stderr = _TeeStream(sys.stderr, self._buf)

    def push(self) -> None:
        """POSTs the current buffer, replacing whatever was stored before. Never raises --
        a log-tail push failing must never take down the job itself. No registry_url (running
        outside the platform, e.g. a local smoke test) means push is a silent no-op."""
        if not self.registry_url:
            return
        url = f"{self.registry_url}/registry/experiments/{self.experiment_id}/logs"
        payload = json.dumps({"lines": list(self._buf)}).encode()
        req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=5)
        except Exception as e:
            print(f"  [warn] log tail POST failed: {e}", file=sys.__stderr__)
