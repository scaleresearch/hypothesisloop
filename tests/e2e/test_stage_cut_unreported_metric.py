"""A ranking metric that no surviving agent reports must not veto the boundary. Ported 1:1 from
tests/scenarios/stage-cut-unreported-metric.sh.

An agent is cut only when it ranks below the line on *every* ranking metric that produced usable
data. If "usable data" were judged from any agent's samples rather than from the current
survivors', a metric nobody still standing reports would look healthy while ranking all survivors
as one tie group -- which the whole-tie-group guardrail then refuses to cut. The result is a ladder
that advances through every boundary and never cuts anyone, silently. That is exactly the shape a
mistyped or retired metric key takes in a real platform experiment.

PROVEN against the live stack: 3x solo green. Needed quota_tier="guaranteed" at signup (an
agent-kind participant otherwise defaults to burst_only, so a guaranteed-tier job submission 402s
with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.0028
RUN_SECONDS = 40


def test_unreported_ranking_metric_does_not_veto_the_cut(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"decoy-{i}") for i in range(6)]
    stages = [
        {"length_pct": 20, "evict_pct": 50},
        {"length_pct": 30, "evict_pct": 25},
        {"length_pct": 50, "evict_pct": 0},
    ]
    # val_accuracy is what the generic workload actually emits; never_reported_score is a second
    # *ranking* metric no workload ever posts a sample for -- the decoy.
    metrics = [
        {"key": "val_accuracy", "direction": "maximize"},
        {"key": "never_reported_score", "direction": "maximize"},
    ]
    pe_id = experiment(
        "stage-decoy",
        [(a, None, "guaranteed") for a in agents],
        budget=0.03,
        report_interval_seconds=10,
        metrics=metrics,
        stages=stages,
    )

    for a in agents:
        api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}})

    st = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda s: s["current_stage"] >= 2,
        deadline=deadline,
    )
    cut_n = len(st["cut_agents"])
    active_n = len(st["active_agents"])

    # The assertion this scenario exists for: reaching the boundary and cutting nobody is
    # precisely the silent failure being pinned here.
    assert cut_n >= 1, "boundary cut nobody: a ranking metric no survivor reports vetoed the whole cut"
    assert cut_n <= 3, f"cut {cut_n} agents, more than floor(50% x 6) = 3"
    assert active_n >= 2, "survivor floor was violated"
    assert active_n + cut_n == len(agents)
