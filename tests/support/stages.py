"""A six-agent ladder brought to its first boundary with a field that can be ranked.

An agent is ranked at a boundary on its completed jobs alone, so the boundary is reached in two
waves: every agent completes one short job, and only then does a second job each outrun what is
left of the agent's share of the first stage. Those are evicted for quota_exhaustion, and the
field having spent the whole stage is what crosses the boundary.
"""
from __future__ import annotations

from support.api import API
from support.wait import Deadline, eventually

STAGES = [
    {"length_pct": 20, "evict_pct": 50},
    {"length_pct": 30, "evict_pct": 25},
    {"length_pct": 50, "evict_pct": 0},
]
ROSTER_SIZE = 6
REPORT_INTERVAL_SECONDS = 5
# Each of the six agents holds 0.48 x 20% / 6 = 0.016 AccH in the first stage: 57s of the default
# job's one core at its rate of 1.0.
BUDGET = 0.48
# The workload reports four times at the least, so 15s of run at this report interval. With its
# start-up that is well inside the 25s reserved and far from the agent's 57s.
RANKED_JOB_HOURS = 0.007
RANKED_RUN_SECONDS = 20
# Reserves 14s, which fits beside a ranked job that took up to 43s, and runs past the 57s whatever
# the ranked job left of them.
BURN_JOB_HOURS = 0.004
BURN_RUN_SECONDS = 120


def _job(seconds: int, baseline: str | None) -> dict:
    env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(seconds)}
    if baseline is not None:
        env["HYPOTHESISLOOP_BASELINE"] = baseline
    return {"env": env}


def cross_first_boundary_ranked(
    api: API, pe_id: str, agents: list[str], deadline: Deadline, baselines: dict[str, str] | None = None
) -> tuple[dict, dict[str, str]]:
    """Returns the stages document once the first boundary is crossed, and each agent's second
    job. baselines names the HYPOTHESISLOOP_BASELINE an agent's ranked job reports around."""
    assert len(agents) == ROSTER_SIZE, f"the budget is sized for {ROSTER_SIZE} agents, got {len(agents)}"
    baselines = baselines or {}
    # An agent of role baseline registers no hypothesis and its jobs name none.
    tests = {a: "" if api.signup_role(pe_id, a)[1] == "baseline" else None for a in agents}
    ranked = {a: api.submit_job(pe_id, a, hours=RANKED_JOB_HOURS, hyp_id=tests[a], job_overrides=_job(RANKED_RUN_SECONDS, baselines.get(a))) for a in agents}
    for job in ranked.values():
        eventually(
            f"{job} to complete, so its agent can be ranked",
            lambda job=job: api.experiment(job),
            accept=lambda e: e["status"] == "COMPLETED",
            reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
            deadline=deadline,
        )
    assert api.stages(pe_id)["current_stage"] == 1, "the boundary was crossed before the second wave was submitted"

    burn = {a: api.submit_job(pe_id, a, hours=BURN_JOB_HOURS, hyp_id=tests[a], job_overrides=_job(BURN_RUN_SECONDS, None)) for a in agents}
    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )
    return stages, burn
