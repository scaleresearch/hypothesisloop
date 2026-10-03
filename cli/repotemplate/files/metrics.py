"""How a job of this experiment reports a metric to the platform.

    from metrics import post_metric
    post_metric(fraction_complete, value, "metric_key")

Report every declared metric as the run goes and once more at the end: a job that reports
nothing is taken for stuck and evicted. Metrics are all a job sends. Its logs are read from
its output. Outside a job this module imports and post_metric raises: only a job reports.
"""

import json
import os
import sys
import urllib.request


def _of_the_job(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"post_metric: ${name} is not set, so this process is no job of the platform and "
                           "has nowhere to report: the platform sets it in a job, and hl job submit runs one")
    return value


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    """Posts one reading. In a job it never raises: a failed report must not end the run it reports on."""
    url = f"{_of_the_job('HYPOTHESISLOOP_API_URL')}/experiments/{_of_the_job('HYPOTHESISLOOP_EXPERIMENT_ID')}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", "Authorization": f"Bearer {_of_the_job('HYPOTHESISLOOP_TOKEN')}"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:
        reason = e.read().decode(errors="replace")[:300] if hasattr(e, "read") else ""
        # A refused token stays refused, so that is said once.
        if getattr(e, "code", None) in (401, 403):
            if not getattr(post_metric, "refused", False):
                post_metric.refused = True
                print(f"  [error] metric POST refused, this run's metrics are not being recorded: {e} {reason}", file=sys.stderr, flush=True)
            return
        print(f"  [warn] metric POST failed: {e} {reason}", file=sys.stderr, flush=True)
