"""One experiment id, one experiment -- even when the same id arrives N times at the same instant.
Ported 1:1 from tests/scenarios/duplicate-submission-race.sh.

Submission decides inside its admission transaction whether an id is new, already QUEUED, or
taken. Two outcomes are both correct and must be told apart:
  - the SAME agent re-submitting its own queued job is an idempotent retry: every POST returns 2xx
    and exactly one row exists.
  - a DIFFERENT agent naming that id is a collision, refused 409 without touching the owner's job.

What must never happen either way is a 5xx: that was the original defect, where concurrent
submissions all passed a pre-transaction check and the loser hit the primary key.

API-only, no accelerator required (the jobs need never be admitted), parallel-safe: own agent(s)
and platform experiment, own run-scoped id.
"""
from __future__ import annotations

import uuid
from concurrent.futures import ThreadPoolExecutor

import pytest

from conftest import make_agent

pytestmark = pytest.mark.parallel

N = 4


def test_concurrent_identical_submissions_are_idempotent_and_second_agent_is_refused(api, experiment, run_id):
    agent = make_agent(api, run_id, "dup-owner")
    other_agent = make_agent(api, run_id, "dup-other")
    pe_id = experiment("dup-race", [agent, other_agent], budget=5.0)

    job_id = f"job-dup-{uuid.uuid4().hex[:8]}-{run_id}"
    # Built once and reused: the race under test is N POSTs of one identical request, not N racing
    # body constructions (each of which would register its own hypothesis first).
    body = api.submission_body_for_id(job_id, pe_id, agent)

    with ThreadPoolExecutor(max_workers=N) as pool:
        responses = list(pool.map(lambda _: api.post_experiment_body(body), range(N)))
    codes = [r.status_code for r in responses]

    accepted = sum(1 for c in codes if 200 <= c < 300)
    # No 5xx, and no 409 either: every one of these is the owning agent retrying its own submission.
    assert accepted == N, f"only {accepted} of {N} concurrent retries succeeded, codes={codes}"

    exp = api.experiment(job_id)
    assert exp["status"] in ("QUEUED", "SUBMITTED", "RUNNING"), exp
    # The id is the primary key, so a second row is impossible by construction; this checks the
    # winner is a complete row and not a half-written one from a rolled-back transaction.
    assert exp.get("estimated_cost_acch") not in (None, ""), exp

    # -- a second agent cannot claim, or disturb, a job id it does not own --
    # The id check is scoped to the submitter, so a different agent naming the same id is a
    # collision, not a legal re-submission of work it does not own.
    body_b = api.submission_body_for_id(job_id, pe_id, other_agent)
    resp_b = api.post_experiment_body(body_b)
    assert resp_b.status_code == 409, resp_b.text

    assert api.experiment(job_id)["agent_id"] == agent
