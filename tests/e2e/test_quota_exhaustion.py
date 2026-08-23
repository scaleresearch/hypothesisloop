"""Quota exhaustion -- the path that evicts RUNNING jobs and cancels queued ones when an agent's
budget is genuinely spent. Ported 1:1 from tests/scenarios/quota-exhaustion.sh. Two halves, and
the order matters:

  1. Reservations alone must NEVER evict anything. Queuing work worth the whole remaining quota is
     a claim about the future; only observed consumption can exhaust a budget. This once was not
     true -- admission legitimately lets reservations fill a quota to 100%, and the exhaustion
     check read the same reservation-inclusive figure, so an agent that queued enough work had its
     already-running jobs evicted (irreversibly, unrefunded) for budget nobody had spent.
  2. Observed overrun DOES evict. A job that outruns its own estimate keeps consuming real
     accelerator-hours; once those pass the budget the running job is evicted with
     quota_exhaustion and its queued siblings are cancelled with their reservations returned.

Overrun is produced honestly rather than by waiting out a large budget: the job is submitted with a
small estimated_duration_hours (what admission reserves against) while its workload is told to run
far longer (HYPOTHESISLOOP_DURATION_SECONDS), so observed cost climbs past the estimate on a
timescale a test can watch. API-only, parallel-safe (own PE/agent, one accelerator).
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCH_RATE, make_agent
from support.wait import assert_stable, eventually

pytestmark = pytest.mark.parallel

# The estimate is a fixed wall-clock duration, deliberately NOT scaled by the accelerator rate: it
# decides how long this test runs, and a pricier accelerator must not stretch it past the suite's
# per-test ceiling. The rate belongs in the budget instead, which is where cost lives.
JOB_HOURS = 0.01
ESTIMATE_SECONDS = round(JOB_HOURS * 3600)
JOB_COST = round(JOB_HOURS * TEST_ACCH_RATE, 6)
# Budget covers exactly TWO of these jobs. That is the whole design of half 1: both jobs fit at
# admission, so reservations alone reach 100% of the quota -- which is precisely what used to trip
# the exhaustion check and kill the running job. Anything less and the second job is refused at
# submission, no reservation is ever recorded, and half 1 asserts nothing at all.
BUDGET = round(JOB_COST * 2, 6)
# The job runs 3x its estimate. Observed consumption therefore passes the 2-estimate budget around
# the 2x mark, leaving a full estimate's worth of headroom for a reconcile tick to notice before
# the job would have ended on its own.
OVERRUN_SECONDS = ESTIMATE_SECONDS * 3


def test_reservation_alone_never_evicts_then_overrun_evicts(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "quota-exhaustion")
    # One stage taking the whole run, so the agent's guaranteed allocation is the entire budget
    # and no stage boundary can move it mid-scenario.
    pe_id = experiment(
        "quota-exhaustion",
        [agent],
        budget=BUDGET,
        report_interval_seconds=5,
        stages=[{"length_pct": 100, "evict_pct": 0}],
    )

    # Both jobs are pinned to the requested flavor: every assertion below is arithmetic on a
    # budget sized as exactly two of them, which only holds if both are actually priced at that
    # rate (an alternate acceptable type prices differently -- see pin_job_flavor's bash comment).
    pin = {"acceptable_accelerator_types": None}
    job = api.submit_job(
        pe_id,
        agent,
        hours=JOB_HOURS,
        job_overrides={**pin, "env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(OVERRUN_SECONDS)}},
    )

    running = eventually(
        f"{job} admitted and RUNNING against a budget covering two of its estimate",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED", "COMPLETED"),
        deadline=deadline,
    )
    assert running["status"] == "RUNNING"

    # -- half 1: a queued reservation must not evict the running job --
    # The budget covers two jobs, so this one is admitted as a reservation rather than refused.
    # Its estimate plus the running job's now account for 100% of the quota while the running job
    # has barely consumed anything -- the exact state that used to evict the RUNNING job.
    code, queued_job = api.submit_job_expect(pe_id, agent, hours=JOB_HOURS, job_overrides=pin)
    assert code < 400, f"second job was refused at submission (HTTP {code}) -- no reservation exists, the budget is mis-sized"

    # Two full reconcile intervals (5s each). Observed consumption after this long is still far
    # below the two-estimate budget, so any eviction here is the reservation-driven bug.
    assert_stable(
        "running job survives a sibling's reservation filling the quota",
        lambda: api.experiment(job)["status"],
        ok=lambda s: s == "RUNNING",
        duration=12,
    )

    # -- half 2: observed overrun evicts the running job --
    # This must happen once observed cost crosses the budget, not at any particular second.
    final = eventually(
        f"{job} to leave RUNNING once observed consumption passes its budget",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "EVICTED", (
        f"job ended as {final['status']!r} instead of EVICTED for quota_exhaustion "
        f"(ran ~{OVERRUN_SECONDS}s on a budget covering {ESTIMATE_SECONDS * 2}s of runtime)"
    )
    assert final.get("eviction_reason") == "quota_exhaustion", (
        f"job evicted for {final.get('eviction_reason')!r}, expected quota_exhaustion"
    )

    # No refund for a running job -- the budget really was spent. Settlement must still record
    # what it consumed, so the eviction is billed rather than silently free.
    settled = eventually(
        f"{job} to be durably settled",
        lambda: api.experiment(job),
        accept=lambda e: bool(e.get("quota_settled_at")),
        deadline=deadline,
    )
    assert settled.get("quota_settled_at")

    # A pre-run job consumed nothing, so exhaustion cancels it rather than billing it. If the
    # cluster had a free accelerator it may have started and been evicted alongside its sibling,
    # and if quick it may even have finished before the budget ran out -- all three are the
    # platform behaving correctly; only "still sitting there QUEUED" is not.
    q_final = eventually(
        f"{queued_job} to settle after its agent's budget was exhausted",
        lambda: api.experiment(queued_job),
        accept=lambda e: e["status"] in ("REJECTED", "EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    if q_final["status"] in ("REJECTED", "EVICTED"):
        # The reason matters as much as the status: an unrelated rejection would otherwise
        # satisfy "cancelled by the same exhaustion".
        assert q_final.get("eviction_reason") == "quota_exhaustion", (
            f"queued sibling is {q_final['status']} but for reason {q_final.get('eviction_reason')!r}, "
            "not quota_exhaustion"
        )
    elif q_final["status"] == "COMPLETED":
        pass  # already ran to completion before the budget ran out
    else:
        pytest.fail(
            f"queued sibling is {q_final['status']} (reason={q_final.get('eviction_reason')!r}) after its "
            "agent's budget was exhausted -- it should have been cancelled"
        )

    # The bound the platform actually promises: consumption may pass the budget by the running
    # work of a few reconcile ticks, never by an unbounded amount. Computed from the real
    # quantities (reconcile interval x accelerator count x rate), not a round multiple, so it
    # stays a real ceiling.
    guaranteed = api.quota_field(pe_id, agent, "guaranteed_accelerator_hours")
    used_final = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    allowance = round(guaranteed + (30.0 / 3600.0) * TEST_ACCH_RATE, 6)
    assert used_final <= allowance, (
        f"settled consumption {used_final} ran past the {allowance} AccH bound (budget + a few reconcile ticks)"
    )
