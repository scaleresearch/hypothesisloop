"""How a job of this experiment reports a metric to the platform.

    from metrics import post_metric
    post_metric(fraction_complete, value, "metric_key")

Report every declared metric as the run goes and once more at the end: a job that reports
nothing is taken for stuck and evicted, and its final report on a metric is its result. Metrics are all a job sends. Its logs are read from
its output. Outside a job this module imports and post_metric raises: only a job reports.

A report the platform could not take for now (no answer, 429, 5xx) is kept in a file and sent
before the next one, and once more, with a few retries, when the process exits.
"""

import atexit
import fcntl
import json
import os
import sys
import tempfile
import time
import urllib.request

_SPOOL_MAX = 10_000
_EXIT_PAUSES_S = (1, 2, 4, 8, 16)


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    """Posts one reading. In a job it never raises: a failed report must not end the run it reports on."""
    job = {name: os.environ.get(name) for name in ("HYPOTHESISLOOP_API_URL", "HYPOTHESISLOOP_EXPERIMENT_ID", "HYPOTHESISLOOP_TOKEN")}
    if not all(job.values()):
        unset = ", ".join("$" + name for name, value in job.items() if not value)
        raise RuntimeError(f"post_metric: {unset} not set, so this process is no job of the platform and has "
                           "nowhere to report: the platform sets them in a job, and hl job submit runs one")
    url = f"{job['HYPOTHESISLOOP_API_URL']}/experiments/{job['HYPOTHESISLOOP_EXPERIMENT_ID']}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value})
    try:
        _deliver(url, job["HYPOTHESISLOOP_TOKEN"], _spool_path(job["HYPOTHESISLOOP_EXPERIMENT_ID"]), [payload])
    except OSError as e:
        print(f"  [warn] metric POST failed and could not be kept to send again: {e}", file=sys.stderr, flush=True)


def _spool_path(experiment_id: str) -> str:
    return os.path.join(tempfile.gettempdir(), f"hypothesisloop-metrics-{experiment_id}.jsonl")


def _deliver(url: str, token: str, spool: str, new: list) -> int:
    """Sends the kept reports and then new, in order, and keeps what could not be sent. Returns
    how many are kept."""
    with open(spool, "a+") as f:
        fcntl.flock(f, fcntl.LOCK_EX)
        f.seek(0)
        pending = [line for line in f.read().splitlines() if line] + new
        sent = 0
        while sent < len(pending) and _send(url, token, pending[sent]):
            sent += 1
        kept = pending[sent:][-_SPOOL_MAX:]
        f.seek(0)
        f.truncate()
        f.writelines(line + "\n" for line in kept)
    if kept and not getattr(_deliver, "at_exit", False):
        _deliver.at_exit = True
        atexit.register(_flush_at_exit, url, token, spool)
    return len(kept)


def _flush_at_exit(url: str, token: str, spool: str) -> None:
    kept = 0
    for pause in _EXIT_PAUSES_S:
        try:
            kept = _deliver(url, token, spool, [])
        except OSError:
            return
        if not kept:
            return
        time.sleep(pause)
    print(f"  [error] {kept} metric reports could not be sent before exit, kept in {spool}", file=sys.stderr, flush=True)


def _send(url: str, token: str, payload: str) -> bool:
    """False for a report to send again: one the platform did not answer or answered with 429 or
    a 5xx. Any other refusal is said and the report dropped, since sending it again changes nothing."""
    req = urllib.request.Request(url, data=payload.encode(), headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"})
    try:
        urllib.request.urlopen(req, timeout=5)
        return True
    except Exception as e:
        reason = e.read().decode(errors="replace")[:300] if hasattr(e, "read") else ""
        code = getattr(e, "code", None)
        # A refused token stays refused, so that is said once.
        if code in (401, 403):
            if not getattr(post_metric, "refused", False):
                post_metric.refused = True
                print(f"  [error] metric POST refused, this run's metrics are not being recorded: {e} {reason}", file=sys.stderr, flush=True)
            return True
        again = code is None or code == 429 or code >= 500
        print(f"  [warn] metric POST failed{', kept to send again' if again else ''}: {e} {reason}", file=sys.stderr, flush=True)
        return not again
