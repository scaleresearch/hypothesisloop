"""The summary gate must be answerable, not just enforceable. Ported 1:1 from
tests/scenarios/summary-gate-discoverable.sh.

An agent's queued jobs are not admitted while it still has a finished job in the platform
experiment with no write-up filed. Submission itself is accepted: a job that completed during the
agent's turn is unknown to it until the turn ends, so a refusal would hit every submit of a batch.
A rule an agent cannot inspect is a dead end -- so the same set the gate reads is listable:
GET /experiments?needs_summary=true. This checks the two halves agree -- the job that holds the
queue is exactly the job the filter returns, and clearing it clears both.

API-only, one accelerator, parallel-safe: own agent and platform experiment.

PROVEN against the live stack: 3x solo green. The job-submitting test needed
quota_tier="guaranteed" at signup (an agent-kind participant otherwise defaults to burst_only, see
the other exclusive-marker ports' convention for the same fix); the cluster-reconcile 422 the
docstring previously blamed is no longer reproducible against this stack.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def test_summary_gate_matches_needs_summary_listing(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "summary-gate")
    pe_id = experiment("summary-gate", [(agent, None, "guaranteed")], budget=5.0)

    assert api.needs_summary(agent, pe_id) == [], "needs_summary must be empty before anything finished"

    job = api.submit_job(pe_id, agent, hours=0.01)
    final = eventually(
        f"{job} to finish",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"job ended as {final['status']!r}, expected COMPLETED"

    eventually(
        "the completed job to appear in needs_summary",
        lambda: api.needs_summary(agent, pe_id),
        accept=lambda ids: job in ids,
        deadline=deadline,
    )

    held_code, held = api.submit_job_expect(pe_id, agent, hours=0.01)
    assert held_code < 400, f"a further submission answered {held_code}; it must be accepted and held in the queue"
    eventually(
        f"{held} to be held by the summary gate",
        lambda: api.experiment(held),
        accept=lambda e: e["status"] == "QUEUED" and e.get("not_admitted_reason") == "summary_gate",
        deadline=deadline,
    )

    api.file_finding(job)
    eventually(
        "the job to leave needs_summary",
        lambda: api.needs_summary(agent, pe_id),
        accept=lambda ids: job not in ids,
        deadline=deadline,
    )

    eventually(
        f"{held} to be admitted once nothing is owed",
        lambda: api.experiment(held),
        accept=lambda e: e["status"] != "QUEUED",
        deadline=deadline,
    )
