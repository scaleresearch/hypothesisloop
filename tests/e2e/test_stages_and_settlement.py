"""Full multi-agent smoke flow. Ported 1:1 from tests/scenarios/stages-and-settlement.sh: every
submitted job reaches a terminal state, the platform experiment advances past its first stage
boundary once enough budget burns, every terminal job gets a durably settled quota, and the
dashboard-facing metrics endpoint returns data for it. API-only, parallel-safe.

PROVEN against the live stack: 3x solo green. Needed quota_tier="guaranteed" at signup (an
agent-kind participant otherwise defaults to burst_only, so a guaranteed-tier job submission 402s
with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

# Jobs declare a short estimate and genuinely run longer, which is what carries the wave past the
# first stage boundary: observed consumption is measured from the observations themselves, so a
# job that runs exactly its estimate never quite reaches the line it is measured against.
JOB_HOURS = 0.0028
RUN_SECONDS = 40


def test_stage_boundary_advances_and_terminal_jobs_settle(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"stage-settle-{i}") for i in range(3)]
    # Three jobs observe roughly 3 x 0.25 x 37s = 0.0077 AccH; the default ladder's 40% first stage
    # of a 0.012 AccH budget puts the boundary at 0.0048, about 60% through the wave.
    pe_id = experiment(
        "stages-settlement", [(a, None, "guaranteed") for a in agents], budget=0.012
    )

    run_env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}
    jobs = [api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": run_env}) for a in agents]

    eventually(
        "all jobs to reach a terminal state",
        lambda: [api.experiment(j)["status"] for j in jobs],
        accept=lambda statuses: all(s in ("COMPLETED", "FAILED", "EVICTED", "REJECTED") for s in statuses),
        deadline=deadline,
    )

    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )
    active_n = len(stages["active_agents"])
    cut_n = len(stages["cut_agents"])
    assert active_n + cut_n == len(agents)
    # 3 agents is below the guardrail floor, so the boundary must advance without cutting anyone.
    assert cut_n == 0, f"cut {cut_n} agent(s) despite only {len(agents)} survivors (guardrail floor)"

    eventually(
        "all terminal jobs to be durably settled",
        lambda: [api.experiment(j).get("quota_settled_at") for j in jobs],
        accept=lambda settled: all(s for s in settled),
        deadline=deadline,
    )

    for job in jobs:
        exp = api.experiment(job)
        assert exp.get("quota_settled_at"), f"{job}: quota_settled_at not set after grace period"
        metrics = api.metrics(job)
        if exp["status"] == "COMPLETED":
            assert len(metrics) >= 1, f"{job}: COMPLETED but reported 0 metric points via registry-service"
