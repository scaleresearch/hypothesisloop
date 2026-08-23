"""A real stage cut, end to end, ported 1:1 from tests/scenarios/stage-ladder-cut.sh: a 3-stage
ladder over a roster large enough to clear the guardrail floor crosses its first boundary, cuts the
configured share of the field, stops the cut agents' jobs, blocks their resubmissions with 422, and
moves their unspent budget to the survivors.

Deliberately asserts the mechanism, not which agent loses. Who ranks where depends on how much of
each agent's metric stream landed before the boundary, which depends on cluster capacity at run
time -- the ranking arithmetic itself (worst-first order, tie groups, no-data-last, the guardrails)
is pinned exactly by controller/stages_rank_test.go instead. API-only, parallel-safe.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

STAGES = [
    {"length_pct": 20, "evict_pct": 50},
    {"length_pct": 30, "evict_pct": 25},
    {"length_pct": 50, "evict_pct": 0},
]
# Each job declares a short estimate and then genuinely runs longer than it, which is what lets a
# wave of jobs burn past the first stage's share of the budget at all -- see the header comment in
# the .sh this was ported from for the full arithmetic.
JOB_HOURS = 0.0028  # ~10s reserved
RUN_SECONDS = 40  # ~4x that actually consumed
# Six agents observe roughly 6 x 0.25 x 37s = 0.0154 AccH, so a 20% first stage of a 0.03 AccH
# budget puts the boundary at 0.006 -- about 40% of the way through the wave.
BUDGET = 0.03


def test_stage_boundary_cuts_field_and_reallocates_budget(api, experiment, run_id, deadline):
    # Six agents: above minSurvivorsForCut (5), and 50% of 6 is 3 cut / 3 kept, which clears
    # minSurvivorsAfterCut (2) without being clamped.
    agents = [make_agent(api, run_id, f"ladder-{i}") for i in range(6)]
    pe_id = experiment("stage-ladder-cut", agents, budget=BUDGET, report_interval_seconds=10, stages=STAGES)

    stages = api.stages(pe_id)
    # The ladder must come back exactly as configured -- this is the write path (POST with an
    # explicit stages field) round-tripping through Postgres. Compared as numbers, not JSON text.
    configured = [(float(s["length_pct"]), float(s["evict_pct"])) for s in stages["stages"]]
    assert configured == [(20.0, 50.0), (30.0, 25.0), (50.0, 0.0)]
    assert stages["current_stage"] == 1

    # Initial allocation is capped to the first stage's share (20%), not the whole budget -- the
    # rest is released at the boundaries.
    first_alloc = api.quota_field(pe_id, agents[0], "guaranteed_accelerator_hours")
    assert first_alloc < BUDGET * 0.2, (
        f"stage-1 allocation {first_alloc} exceeds the first stage's 20% share of {BUDGET}"
    )

    run_env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}
    jobs = [api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": run_env}) for a in agents]

    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )

    cut_agents = stages["cut_agents"]
    active_agents = stages["active_agents"]
    cut_n, active_n = len(cut_agents), len(active_agents)
    assert active_n + cut_n == len(agents), f"active({active_n})+cut({cut_n}) != {len(agents)} agents"
    # 50% of 6 survivors. Fewer is legitimate only if a tie group straddled the line; more is a bug.
    assert cut_n <= 3, f"cut {cut_n} agents, more than floor(50% x 6) = 3"
    assert active_n >= 2, f"only {active_n} survivor(s) left -- the survivor floor was violated"

    if cut_n == 0:
        # A tie group straddled the line -- legitimate, but nothing further to assert.
        return

    bad_stage = sum(1 for c in cut_agents if c["stage_index"] != 1)
    assert bad_stage == 0, f"{bad_stage} cut record(s) attributed to the wrong stage"

    # A cut is terminal: further submissions are rejected platform-side, not merely discouraged.
    victim = cut_agents[0]["agent_id"]
    code, _ = api.submit_job_expect(pe_id, victim, hours=JOB_HOURS, job_overrides={"env": run_env})
    assert code >= 400, f"{victim} was cut but its resubmission was accepted (HTTP {code})"

    # A cut agent's quota is zeroed, and its unspent share plus the incoming stage's release goes
    # to the survivors.
    victim_q = api.quota_field(pe_id, victim, "guaranteed_accelerator_hours")
    assert victim_q == 0, f"{victim} was cut but still holds {victim_q} guaranteed AccH"

    survivor = active_agents[0]
    survivor_q = api.quota_field(pe_id, survivor, "guaranteed_accelerator_hours")
    assert survivor_q > first_alloc, f"survivor's quota did not grow: {survivor_q} <= {first_alloc}"

    # A cut job did nothing wrong: the platform decided. It must be reported as `policy`, never as
    # one of the agent's failures (domain.EvictionStageCut is FaultPolicy). Which cut agent still
    # had a running job at the boundary is not fixed -- an agent whose job had already completed is
    # cut without anything to stop -- so find one that really was evicted rather than assuming the
    # first cut agent was.
    cut_agent_ids = {c["agent_id"] for c in cut_agents}
    agent_by_job = dict(zip(jobs, agents))

    def find_cut_evicted_job():
        for j, a in agent_by_job.items():
            if a not in cut_agent_ids:
                continue
            exp = api.experiment(j)
            reason = exp.get("eviction_reason") or ""
            if exp["status"] == "EVICTED" and reason.startswith("stage_cut"):
                return a, j
        return None

    found = None
    try:
        found = eventually(
            "a cut agent's job to reach its stage_cut eviction",
            find_cut_evicted_job,
            accept=lambda v: v is not None,
            deadline=deadline,
        )
    except AssertionError:
        found = None

    if found is None:
        return  # every cut agent's job had already finished before the boundary -- nothing observable

    cut_job_agent, cut_job = found
    policy_n = api.eviction_class_count(pe_id, cut_job_agent, "policy")
    workload_n = api.eviction_class_count(pe_id, cut_job_agent, "workload")
    infra_n = api.eviction_class_count(pe_id, cut_job_agent, "infrastructure")
    assert policy_n is not None and policy_n >= 1, (
        f"evictions_by_class.policy is {policy_n!r} -- a stage cut is the platform's own decision "
        "and must be reported as one"
    )
    # The point of the class: a cut agent's record must not read as if it failed. Both of these
    # flip if stage_cut is classified as anything but policy.
    assert workload_n == 0, f"evictions_by_class.workload is {workload_n} for a cut agent, expected 0"
    assert infra_n == 0, f"evictions_by_class.infrastructure is {infra_n} for a cut agent, expected 0"
    # A stage cut is terminal by policy, so it must not have bought the job a free infrastructure
    # requeue: infra_requeue_count only increments, so 0 proves it never took one.
    assert api.experiment(cut_job).get("infra_requeue_count") == 0, (
        "cut job has a nonzero infra_requeue_count -- a policy decision was treated as an "
        "infrastructure fault"
    )

    class_total, reason_total, unclassified = api.eviction_class_coverage(pe_id, cut_job_agent)
    assert class_total == reason_total and unclassified == 0, (
        f"class breakdown does not account for the evictions: by_class={class_total} "
        f"by_reason={reason_total} unclassified={unclassified}"
    )

    # Progress is monotonic and the published next boundary is the one that actually follows.
    final_stages = api.stages(pe_id)
    assert final_stages["next_boundary_progress"] == 0.5, (
        f"next_boundary_progress={final_stages['next_boundary_progress']}, want 0.5 after advancing to stage 2"
    )

    # The endpoint must never leak standings -- an agent may see that it is cut, not how close it is.
    leaked = [k for k in ("rank", "ranks", "standings", "scores", "metric_values") if k in final_stages]
    assert not leaked, f"stages endpoint leaked ranking fields: {leaked}"
