"""Full multi-agent smoke flow. Ported 1:1 from tests/scenarios/stages-and-settlement.sh: every
submitted job reaches a terminal state, the platform experiment advances past its first stage
boundary once enough budget burns, every terminal job gets a durably settled quota, and the
dashboard-facing metrics endpoint returns data for it. API-only, parallel-safe.

No job here completes: each outruns its agent's share of the first stage and is evicted for
quota_exhaustion. An agent is ranked on its completed jobs alone, so the boundary finds a field of
five with nothing to compare, and it must advance with nobody cut rather than wait for values no
job is left to report.

Needs quota_tier="guaranteed" at signup (an agent-kind participant otherwise defaults to
burst_only, so a guaranteed-tier job submission 402s with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

# Five agents is minSurvivorsForCut, so the default ladder's 75% first cut would apply to a field
# that could be ranked. Each holds 0.02 x 40% / 5 = 0.0016 AccH in the first stage, 23s of the
# default accelerator at its rate of 0.25. A job reserves 10s of it and runs far past the 23s.
AGENTS = 5
BUDGET = 0.02
JOB_HOURS = 0.0028
RUN_SECONDS = 120


def test_stage_boundary_advances_and_terminal_jobs_settle(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"stage-settle-{i}") for i in range(AGENTS)]
    pe_id = experiment(
        "stages-settlement", [(a, None, "guaranteed") for a in agents], budget=BUDGET
    )

    job_overrides = {"env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}, "acceptable_accelerator_types": None}
    jobs = [api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides=job_overrides) for a in agents]

    ended = eventually(
        "all jobs to reach a terminal state",
        lambda: [api.experiment(j) for j in jobs],
        accept=lambda exps: all(e["status"] in ("COMPLETED", "FAILED", "EVICTED", "REJECTED") for e in exps),
        deadline=deadline,
    )
    endings = [(e["status"], e.get("eviction_reason")) for e in ended]
    assert endings == [("EVICTED", "quota_exhaustion")] * AGENTS, (
        f"jobs ended as {endings}, want every one evicted for quota_exhaustion -- the boundary below "
        "must be met by a field with no completed job"
    )

    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )
    assert sorted(stages["active_agents"]) == sorted(agents), (
        f"cut {stages['cut_agents']} from a field in which nobody completed a job"
    )

    eventually(
        "all terminal jobs to be durably settled",
        lambda: [api.experiment(j).get("quota_settled_at") for j in jobs],
        accept=lambda settled: all(s for s in settled),
        deadline=deadline,
    )

    for job in jobs:
        exp = api.experiment(job)
        assert exp.get("quota_settled_at"), f"{job}: quota_settled_at not set after grace period"
        assert len(api.metrics(job)) >= 1, f"{job}: ran and reported 0 metric points via registry-service"
