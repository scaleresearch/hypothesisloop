"""The summary gate must be answerable, not just enforceable. Ported 1:1 from
tests/scenarios/summary-gate-discoverable.sh.

An agent may not submit into a platform experiment while it still has a finished job there with no
write-up filed. That rule is enforced at admission, but a rule an agent cannot inspect is a dead
end -- so the same set the gate reads is listable: GET /experiments?needs_summary=true. This
checks the two halves agree -- the job that blocks submission is exactly the job the filter
returns, and clearing it clears both.

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

    blocked_code, _ = api.submit_job_expect(pe_id, agent, hours=0.01)
    assert blocked_code == 403, "a further submission must be refused while the write-up is owed"

    api.file_finding(job)
    eventually(
        "the job to leave needs_summary",
        lambda: api.needs_summary(agent, pe_id),
        accept=lambda ids: job not in ids,
        deadline=deadline,
    )

    after_code, _ = api.submit_job_expect(pe_id, agent, hours=0.01)
    assert after_code < 400, "submission must be accepted again once nothing is owed"
