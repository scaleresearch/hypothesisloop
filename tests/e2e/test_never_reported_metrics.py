"""A job that runs but never emits a metric its platform experiment declared, ported 1:1 from
tests/scenarios/never-reported-metrics.sh. It cannot be ranked, cut, or compared against anything
-- there is nothing to judge it by -- while it holds an accelerator and bills for it, so the
controller evicts it and names the actual fault: its reporting path, not a hung trainer.

The distinction this test exists to protect: "no samples recently" and "never reported at all" are
different jobs with different fixes. A healthy job that reports normally must survive the same
window that condemns the mute one, so both run side by side here.

API-only and parallel-safe: two short jobs on their own platform experiment. Marked `slow`: the
silence window is a real platform setting (max(min_silence_window_seconds, silence_multiplier x
report_interval) = 300s floor by default), not something this test may shrink -- shrinking it would
stop testing the deployed grace period.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import assert_stable, eventually

pytestmark = [pytest.mark.parallel, pytest.mark.slow]

# Mirrors tests/lib/common.sh::silence_window_seconds and
# controlplane/settings/hypothesisloop.yaml's silence_multiplier(3.0)/min_silence_window_seconds(300):
# the configured floor dominates any short report_interval, so a job is only called mute after two
# full windows.
REPORT_INTERVAL = 5
SILENCE_MULTIPLIER = 3.0
MIN_SILENCE_WINDOW_SECONDS = 300
SILENCE_WINDOW_SECONDS = max(MIN_SILENCE_WINDOW_SECONDS, SILENCE_MULTIPLIER * REPORT_INTERVAL)
GRACE_SECONDS = int(SILENCE_WINDOW_SECONDS * 2)


@pytest.mark.timeout(1500)
def test_mute_job_evicted_while_reporting_job_survives(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "mute")
    pe_id = experiment("never-reported-metrics", [agent], budget=10.0, report_interval_seconds=REPORT_INTERVAL)

    # Comfortably longer than GRACE_SECONDS so the job is still running when the verdict lands: if
    # it exited first, its disappearance -- not the mute check -- would explain the terminal status.
    mute_seconds = GRACE_SECONDS + 150
    mute_hours = round(mute_seconds / 3600.0, 6)
    mute_job = api.submit_job(
        pe_id, agent, hours=mute_hours,
        job_overrides={"command": ["/bin/sh", "-c"], "args": [f"sleep {mute_seconds}"], "max_retries": 0},
    )

    running = eventually(
        f"{mute_job} to run (it is alive, just silent)",
        lambda: api.experiment(mute_job),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert running["status"] == "RUNNING"

    # -- a live job that never reports a declared metric is evicted --
    # The verdict cannot land before the grace window, so waiting only that long would time out on
    # a perfectly healthy platform; give it the grace plus a reconcile margin, still short of the
    # job's own runtime so a pass here cannot be the job simply finishing.
    final = eventually(
        f"{mute_job} to be evicted for never reporting (grace={GRACE_SECONDS}s)",
        lambda: api.experiment(mute_job),
        accept=lambda e: e["status"] in ("EVICTED", "FAILED", "COMPLETED"),
        deadline=deadline,
    )
    assert final["status"] == "EVICTED", (
        f"mute job ended as {final['status']!r}, expected EVICTED for never_reported_metrics -- if "
        "COMPLETED, it ran its full duration emitting nothing and was never evicted"
    )
    # The code is the first token; a reason may carry a ": detail" suffix (see EvictionReason.WithDetail).
    reason = final.get("eviction_reason") or ""
    assert reason.startswith("never_reported_metrics"), (
        f"mute job evicted for {reason!r}, expected never_reported_metrics"
    )

    # -- a job that does report survives the same window --
    # The control: same platform experiment, same declared metric, same silence window -- the only
    # difference is that this one reports. If this is evicted too, the check is not measuring
    # reporting, it is just killing jobs.
    healthy_seconds = GRACE_SECONDS + 60
    healthy_hours = round(healthy_seconds / 3600.0, 6)
    healthy_job = api.submit_job(
        pe_id, agent, hours=healthy_hours,
        job_overrides={"env": {
            "HYPOTHESISLOOP_DURATION_SECONDS": str(healthy_seconds),
            "HYPOTHESISLOOP_REPORT_INTERVAL_SECONDS": str(REPORT_INTERVAL),
        }},
    )
    healthy_running = eventually(
        f"{healthy_job} (the reporting control) to run",
        lambda: api.experiment(healthy_job),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert healthy_running["status"] == "RUNNING"

    # Hold it across more than the grace that condemned the mute job. Polling the whole time rather
    # than sleeping once catches an eviction that happens and is then superseded.
    assert_stable(
        "reporting job survives the window that evicted the mute one",
        lambda: api.experiment(healthy_job)["status"],
        ok=lambda s: s in ("RUNNING", "COMPLETED"),
        duration=GRACE_SECONDS + 30,
        interval=5,
    )
    assert not api.experiment(healthy_job).get("eviction_reason"), (
        f"reporting job was evicted for "
        f"{api.experiment(healthy_job).get('eviction_reason')!r} despite reporting normally"
    )

    api.cancel_job(healthy_job)
