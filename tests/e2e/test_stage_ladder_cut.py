"""A real stage cut, end to end, ported 1:1 from tests/scenarios/stage-ladder-cut.sh: a 3-stage
ladder over a roster large enough to clear the guardrail floor crosses its first boundary, cuts the
configured share of the field, stops the cut agents' jobs, blocks their resubmissions with 422, and
moves their unspent budget to the survivors.

Deliberately asserts the mechanism, not which agent loses. Who ranks where depends on the values
each agent's completed job happened to report -- the ranking arithmetic itself (worst-first order,
tie groups, no-data-last, the guardrails) is pinned exactly by controller/stages_rank_test.go
instead. API-only, parallel-safe.

The platform experiment also declares a second ranking metric no workload ever reports. An agent
is cut only when it ranks below the line on every ranking metric that produced usable data, so a
metric nobody still standing reports must count as no data: counted as healthy it ranks all
survivors as one tie group, which is kept whole, and the ladder advances through every boundary
cutting nobody. That is the shape a mistyped or retired metric key takes in a real run.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.api import ENDED
from support.stages import BUDGET, BURN_JOB_HOURS, REPORT_INTERVAL_SECONDS, ROSTER_SIZE, STAGES, cross_first_boundary_ranked
from support.wait import eventually

pytestmark = pytest.mark.parallel

METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "never_reported_score", "direction": "maximize"},
]


def test_stage_boundary_cuts_field_and_reallocates_budget(api, experiment, run_id, deadline):
    # Six agents: above minSurvivorsForCut (5), and 50% of 6 is 3 cut / 3 kept, which clears
    # minSurvivorsAfterCut (2) without being clamped.
    agents = [make_agent(api, run_id, f"ladder-{i}") for i in range(ROSTER_SIZE)]
    pe_id = experiment(
        "stage-ladder-cut", agents, budget=BUDGET, report_interval_seconds=REPORT_INTERVAL_SECONDS,
        metrics=METRICS, stages=STAGES,
    )

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

    stages, jobs = cross_first_boundary_ranked(api, pe_id, agents, deadline)

    cut_agents = stages["cut_agents"]
    # The agent that measured the baseline is never cut and is no part of the ranked field.
    baseline = api.baseline_of[pe_id].agent
    assert baseline in stages["active_agents"], stages
    active_agents = [a for a in stages["active_agents"] if a != baseline]
    cut_n, active_n = len(cut_agents), len(active_agents)
    assert active_n + cut_n == len(agents), f"active({active_n})+cut({cut_n}) != {len(agents)} agents"
    # 50% of 6 survivors. Fewer is legitimate only if a tie group straddled the line; more is a bug.
    assert cut_n <= 3, f"cut {cut_n} agents, more than floor(50% x 6) = 3"
    assert active_n >= 2, f"only {active_n} survivor(s) left -- the survivor floor was violated"
    # Every agent completed a job that reported val_accuracy, so the field can be ranked.
    assert cut_n >= 1, "boundary cut nobody: a ranking metric no survivor reports vetoed the whole cut"

    # Progress is monotonic and the published next boundary is the one that actually follows.
    assert stages["next_boundary_progress"] == 0.5, (
        f"next_boundary_progress={stages['next_boundary_progress']}, want 0.5 after advancing to stage 2"
    )

    # The endpoint must never leak standings -- an agent may see that it is cut, not how close it is.
    leaked = [k for k in ("rank", "ranks", "standings", "scores", "metric_values") if k in stages]
    assert not leaked, f"stages endpoint leaked ranking fields: {leaked}"

    bad_stage = sum(1 for c in cut_agents if c["stage_index"] != 1)
    assert bad_stage == 0, f"{bad_stage} cut record(s) attributed to the wrong stage"

    # A cut is terminal: further submissions are rejected platform-side, not merely discouraged.
    victim = cut_agents[0]["agent_id"]
    code, _ = api.submit_job_expect(pe_id, victim, hours=BURN_JOB_HOURS)
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
    # had a running job at the boundary is not fixed -- an agent whose second job had already
    # spent its share was evicted for quota_exhaustion and is cut without anything to stop -- so
    # find one that really was evicted by the cut rather than assuming the first cut agent was.
    cut_jobs = {c["agent_id"]: jobs[c["agent_id"]] for c in cut_agents}
    ended = eventually(
        "every cut agent's job to end",
        lambda: {a: api.experiment(j) for a, j in cut_jobs.items()},
        accept=lambda exps: all(e["status"] in ENDED for e in exps.values()),
        deadline=deadline,
    )
    found = next(
        ((a, cut_jobs[a]) for a, e in ended.items()
         if e["status"] == "EVICTED" and (e.get("eviction_reason") or "").startswith("stage_cut")),
        None,
    )
    if found is None:
        return  # every cut agent's job had already ended before the boundary -- nothing observable

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
