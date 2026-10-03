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


def post_metric(fraction: float, value: float, metric_name: str) -> None:
    """Posts one reading. In a job it never raises: a failed report must not end the run it reports on."""
    job = {name: os.environ.get(name) for name in ("HYPOTHESISLOOP_API_URL", "HYPOTHESISLOOP_EXPERIMENT_ID", "HYPOTHESISLOOP_TOKEN")}
    if not all(job.values()):
        unset = ", ".join("$" + name for name, value in job.items() if not value)
        raise RuntimeError(f"post_metric: {unset} not set, so this process is no job of the platform and has "
                           "nowhere to report: the platform sets them in a job, and hl job submit runs one")
    url = f"{job['HYPOTHESISLOOP_API_URL']}/experiments/{job['HYPOTHESISLOOP_EXPERIMENT_ID']}/metrics"
    payload = json.dumps({"metric_name": metric_name, "fraction_complete": fraction, "metric_value": value}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json", "Authorization": f"Bearer {job['HYPOTHESISLOOP_TOKEN']}"})
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
