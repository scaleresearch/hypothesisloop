"""GET /platform-experiments/{id}/results -- what a finished run amounts to. Ported 1:1 from
tests/scenarios/results-standings.sh. The ranking arithmetic itself is pinned by Go unit tests;
this covers what only a live run exercises:
  1. standings come back ranked, one entry per agent that reported, on the declared metric.
  2. every standing carries the experiment that produced it and that job's code_ref.
  3. a constraint metric gates eligibility: an agent that never reports a declared constraint is
     excluded from standings entirely rather than ranked on the metric it did report.

API-only, parallel-safe (own PE, its own agents).

PROVEN against the live stack: 3x solo green. Needed quota_tier="guaranteed" at signup (an
agent-kind participant otherwise defaults to burst_only, so a guaranteed-tier job submission 402s
with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.01
RUN_SECONDS = 20


def _run_env():
    return {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}


def test_unreported_constraint_excludes_every_agent(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"results-a-{i}") for i in range(2)]
    metrics = [
        {"key": "val_accuracy", "direction": "maximize", "role": "ranking"},
        {"key": "never_emitted_budget", "direction": "minimize", "role": "constraint", "bound": 1.0},
    ]
    pe_id = experiment(
        "results-standings-constrained",
        [(a, None, "guaranteed") for a in agents],
        budget=10.0,
        report_interval_seconds=5,
        metrics=metrics,
    )

    jobs = [api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": _run_env()}) for a in agents]
    for job in jobs:
        final = eventually(
            f"{job} to complete",
            lambda j=job: api.experiment(j),
            accept=lambda e: e["status"] == "COMPLETED",
            reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
            deadline=deadline,
        )
        assert final["status"] == "COMPLETED"
        api.file_finding(job)

    results = api.results(pe_id)
    metric_names = {m["metric"] for m in results["metrics"]}
    assert "val_accuracy" in metric_names
    assert "never_emitted_budget" not in metric_names, "a constraint metric must never be presented as its own ranking"

    ranking = next(m for m in results["metrics"] if m["metric"] == "val_accuracy")
    assert len(ranking["standings"]) == 0, "agents that never reported the declared constraint must be excluded"


def test_standings_are_ranked_traceable_and_exact(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"results-b-{i}") for i in range(2)]
    pe_id = experiment(
        "results-standings-plain", [(a, None, "guaranteed") for a in agents], budget=10.0, report_interval_seconds=5,
        metrics=[{"key": "val_accuracy", "direction": "maximize"}],
    )

    jobs = [api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": _run_env()}) for a in agents]
    for job in jobs:
        final = eventually(
            f"{job} to complete",
            lambda j=job: api.experiment(j),
            accept=lambda e: e["status"] == "COMPLETED",
            reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
            deadline=deadline,
        )
        assert final["status"] == "COMPLETED"
        api.file_finding(job)

    results = api.results(pe_id)
    ranked = next(m for m in results["metrics"] if m["metric"] == "val_accuracy")
    standings = ranked["standings"]
    assert standings, "no agent was ranked on val_accuracy even with no constraint declared"

    ranks = [s["rank"] for s in standings]
    assert ranks == list(range(1, len(standings) + 1))
    assert all(standings[i]["best"] >= standings[i + 1]["best"] for i in range(len(standings) - 1))

    assert sorted(s["agent_id"] for s in standings) == sorted(agents)

    job_ids = set(jobs)
    traceable = sum(1 for s in standings if s.get("experiment_id") in job_ids and s.get("code_ref"))
    assert traceable == len(standings), "every standing must name one of this run's own jobs and its code_ref"
