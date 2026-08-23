"""Live running-cost correction (controlplane/services/quota/running_cost.go), ported 1:1 from
tests/scenarios/running-cost-live.sh: a RUNNING job's contribution to used_guaranteed_acch must
track its actual observed-elapsed cost, not stay pinned at the static admission-time estimate for
the job's whole lifetime. Right after admission the estimate is debited in full (budget is never
under-reserved before a job has reported anything); once the job starts reporting metrics,
correctRunningCosts should replace that full estimate with the (much smaller) actual
observed-so-far cost, then grow it back up as the job keeps running -- never staying flat at the
original estimate throughout. API-only, parallel-safe (its own fresh PE/agent).
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def test_running_cost_corrects_below_estimate_then_grows(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "livecost")
    # report_interval_seconds=5: frequent enough samples to see the correction move within the
    # test's own short runtime.
    pe_id = experiment("running-cost-live", [agent], budget=50.0, report_interval_seconds=5)

    hours = 0.05
    duration_seconds = round(hours * 3600)
    early_delay = max(1, round(duration_seconds * 0.15))
    late_delay = max(1, round(duration_seconds * 0.65))

    job = api.submit_job(pe_id, agent, hours=hours)
    running = eventually(
        f"{job} to run",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    estimated = running["estimated_cost_acch"]

    # Sample early (job has barely run) and late (job has run for a while, but still well inside
    # its own duration). correctRunningCosts only ever replaces a RUNNING job's estimate once it
    # has observed at least one sample from that job -- the 15%-in early sample gives it a couple
    # of report intervals first. Both samples assert the job is still RUNNING -- if it already
    # finished, the sample says nothing about live correction of a RUNNING job.
    time.sleep(early_delay)
    assert api.experiment(job)["status"] == "RUNNING", (
        f"{job} is no longer RUNNING at the early sample point (t+{early_delay}s of ~{duration_seconds}s) "
        "-- duration too short for this test's sample delays"
    )
    early = api.quota_field(pe_id, agent, "used_guaranteed_acch")

    time.sleep(late_delay - early_delay)
    assert api.experiment(job)["status"] == "RUNNING", (
        f"{job} is no longer RUNNING at the late sample point (t+{late_delay}s of ~{duration_seconds}s) "
        "-- duration too short for this test's sample delays"
    )
    late = api.quota_field(pe_id, agent, "used_guaranteed_acch")

    assert late > early, (
        f"used_guaranteed_acch did not grow while {job} kept running ({early} -> {late}) -- "
        "live running-cost correction may not be applying"
    )
    assert early < estimated, (
        f"early used_guaranteed_acch ({early}) is not below the full estimate ({estimated}) -- job "
        "may still be debited at its static estimate instead of observed cost"
    )

    api.cancel_job(job)
