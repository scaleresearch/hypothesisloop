"""Shared metric-posting helper for job processes (harness.py, train.py, or any hand-written
job script that wants the same behavior).

The job process's only integration point with the platform is pushing its own metrics --
logs and debug comments are collected and relayed by the supervising runtime/experimentator
agent instead, never self-reported by the job (the runtime already reads pod stdout live and
the experimentator agent already observes and comments on job status from the outside; a job
duplicating either of those paths is a stale, harder-to-trust second copy of state the platform
already has). So this module intentionally does exactly one thing: post_metric().

Usage:
    from metrics import post_metric
    post_metric(fraction_complete, value, "metric_name")   # call at a steady cadence, not just
                                                             # once at the end -- a silent job is
                                                             # evicted as presumed-stuck.
"""

import json
import os
import sys
import time
import urllib.request

EXP_ID = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
API_URL = os.environ.get("HYPOTHESISLOOP_API_URL", "http://localhost:8081")


def post_metric(fraction: float, value: float, metric_name: str, attempts: int = 1) -> None:
    """POSTs one metric reading. Never raises -- a reporting-endpoint hiccup must not take down
    the timed work it's reporting on.

    `attempts > 1` is for the *final* reading of a run: "never raise on a reporting failure" must
    not quietly become "lose the result". A mid-run progress reading is disposable (the next one
    is seconds away), but the run's final score has no successor, so it gets a bounded retry.
    Even if every attempt fails, the full result record is still printed to stdout by the caller,
    which the runtime collects -- the score is recoverable from the job's logs, not gone.
    """
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    for attempt in range(attempts):
        req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=5)
            return
        except Exception as e:
            print(f"  [warn] metric POST failed ({attempt + 1}/{attempts}): {e}", file=sys.stderr)
            if attempt + 1 < attempts:
                time.sleep(2 * (attempt + 1))
