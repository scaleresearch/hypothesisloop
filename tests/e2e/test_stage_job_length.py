"""A stage's max_job_hours, end to end: an over-long submission is rejected before it costs
anything, and a job that understates its duration to get in is evicted job_too_long once its
observed runtime passes the cap. Ported 1:1 from tests/scenarios/stage-job-length.sh.

The second half is the one that matters: estimated_duration_hours is a claim, and the cap is only
real if the control plane measures what actually ran.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = [pytest.mark.parallel, pytest.mark.accelerator]

# One stage, no cuts -- this scenario is about the length cap alone, so nothing else can stop a
# job. 30s cap; the budget is far larger than anything below can burn, so quota exhaustion never
# races the cap for the eviction.
CAP_HOURS = 0.0084
STAGES = [{"length_pct": 100, "evict_pct": 0, "max_job_hours": CAP_HOURS}]


@pytest.fixture
def joblen_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "agent-joblen")
    pe_id = experiment(
        "stage-joblen", [agent], budget=5, max_agents=2, report_interval_seconds=5, stages=STAGES,
    )
    return pe_id, agent


def test_max_job_hours_published_on_stages_endpoint(api, joblen_pe):
    pe_id, agent = joblen_pe
    # The cap must round-trip and be published to agents -- they can only plan around a limit they
    # can read.
    published = api.stages(pe_id)["stages"][0]["max_job_hours"]
    assert published == CAP_HOURS, (
        f"stages endpoint reports max_job_hours={published}, want {CAP_HOURS}"
    )


def test_overlong_estimate_rejected_at_submit_gate(api, joblen_pe):
    pe_id, agent = joblen_pe
    # Submit gate: an honest over-long estimate is rejected outright, before any quota is debited.
    code, _job_id = api.submit_job_expect(pe_id, agent, hours=1.0)
    assert code >= 400, (
        f"a 1.0h job was accepted (HTTP {code}) despite the {CAP_HOURS}h cap"
    )


def test_underestimated_job_evicted_job_too_long_and_settles_usage(api, joblen_pe, deadline):
    pe_id, agent = joblen_pe
    # Runtime enforcement: estimate under the cap, then run well past it. This is the job the
    # submit gate cannot catch.
    OVERRUN_SECONDS = 150
    job_id = api.submit_job(
        pe_id, agent, hours=0.005,
        job_overrides={"env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(OVERRUN_SECONDS)}},
    )

    running = eventually(
        f"{job_id} to run",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] == "RUNNING",
        deadline=deadline,
    )
    assert running["status"] == "RUNNING", f"job never reached RUNNING (status={running['status']})"

    # The cap is measured against observed elapsed hours, so eviction lands a reconcile tick or
    # two after the cap itself -- never before it, which is the half that would be a bug.
    final = eventually(
        f"{job_id} to settle",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "EVICTED" and final.get("eviction_reason") == "job_too_long", (
        f"job ended {final['status']}/{final.get('eviction_reason', 'n/a')}, want EVICTED/job_too_long"
    )

    # Eviction settles like any other terminal path -- the agent is billed for what genuinely
    # ran, not for the estimate it submitted. quota_settled_at (Postgres) is written only after
    # the observed-cost write to the metrics DB (GreptimeDB) succeeds, so it never lies about
    # order -- but used_guaranteed_acch itself is populated by a live query against that same
    # separate, eventually-consistent store, which under concurrent load can still lag behind the
    # Postgres flag by a beat. Waiting on quota_settled_at alone isn't enough -- wait on the
    # actual field being asserted on.
    eventually(
        f"{job_id}'s quota to settle",
        lambda: api.experiment(job_id).get("quota_settled_at"),
        accept=bool,
        deadline=deadline,
    )
    used = eventually(
        f"{job_id}'s billed usage to be observable via used_guaranteed_acch",
        lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
        accept=lambda v: v > 0,
        deadline=deadline,
    )
    assert used > 0, f"evicted job settled to {used} AccH -- nothing was billed for a job that ran"
