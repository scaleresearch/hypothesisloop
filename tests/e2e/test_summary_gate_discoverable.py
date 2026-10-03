"""The summary gate must be answerable, not just enforceable. Ported 1:1 from
tests/scenarios/summary-gate-discoverable.sh.

A completed job of an agent with no write-up filed holds the jobs that agent submits after it
ended, and no other: a job queued before then is admitted as if nothing were owed, since its
agent could not have known. Submission itself is accepted and the job waits in the queue.
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

import json

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def test_summary_gate_matches_needs_summary_listing(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "summary-gate")
    # One accelerator at a time, so the second job of the batch is still queued when the first ends.
    pe_id = experiment("summary-gate", [(agent, None, "guaranteed")], budget=5.0, max_concurrent_accelerators=1)

    assert api.needs_summary(agent, pe_id) == [], "needs_summary must be empty before anything finished"

    job = api.submit_job(pe_id, agent, hours=0.01)
    batched = api.submit_job(pe_id, agent, hours=0.01)
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

    # Submitted before that job ended, the batch's other job is not held by it.
    eventually(
        f"{batched}, queued before {job} ended, to be admitted while its summary is owed",
        lambda: api.experiment(batched),
        accept=lambda e: e["status"] != "QUEUED",
        reject=lambda e: e.get("not_admitted_reason") == "summary_gate",
        deadline=deadline,
    )
    assert api.needs_summary(agent, pe_id) == [job], "the batch's second job ran after the first's summary was filed"
    api.cancel_job(batched)

    held_code, held = api.submit_job_expect(pe_id, agent, hours=0.01)
    assert held_code < 400, f"a further submission answered {held_code}; it must be accepted and held in the queue"
    gated = eventually(
        f"{held} to be held by the summary gate",
        lambda: api.experiment(held),
        accept=lambda e: e["status"] == "QUEUED" and e.get("not_admitted_reason") == "summary_gate",
        deadline=deadline,
    )
    sentence = gated["not_admitted_message"]
    assert job in sentence and f"hl job summary {job} --text T" in sentence, f"the gate does not name the job owed and the command: {sentence!r}"
    # The gate is announced at once, with no wait for the long-wait notice.
    announced = eventually(
        f"job.held for {held}",
        lambda: [
            json.loads(line)
            for line in api.hl_expect("watch", "--experiment", held, "--kinds", "job.held", "--since", "1", "--no-wait", "--timeout", "5", timeout=20).stdout.splitlines()
            if line.startswith("{")
        ],
        accept=lambda events: bool(events),
        deadline=deadline,
    )
    assert [(e["kind"], e["subject"], e["value"], e["detail"]) for e in announced] == [("job.held", held, "summary_gate", sentence)], announced
    assert announced[0]["occurred_at"], f"job.held says no time: {announced[0]}"

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
