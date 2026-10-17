"""Job/platform-experiment termination semantics:
  1. A valid job that no connected cluster can ever hold (more cores than any node has) is
     refused at submit with 422 exceeds_cluster_capacity, is not stored and debits nothing. A job
     that waits in the queue reserves its estimate, and cancelling it while still QUEUED ends it
     as CANCELLED with its reservation returned.
  2. A RUNNING job that's cancelled is CANCELLED too (terminal, refunded) -- and stays so, unlike
     preemption (see preemption-requeue.sh).
  3. Closing a platform experiment is not just a status flip: controller.reconcileClosedExperiments
     (a self-healing sweep, not a synchronous side effect of the close call itself) evicts every
     still-RUNNING job under it with reason "experiment_closed" on its next reconcile
     tick, and rejects any further submission.
API-only, parallel-safe.
"""
from __future__ import annotations

import pytest

from conftest import TEST_CPU_ACCH_RATE, cpu_job, make_agent
from support.api import ENDED
from support.wait import assert_stable, eventually

pytestmark = pytest.mark.parallel

# The most cores one job may ask for, more than a node of any cluster the suite runs on has.
UNAVAILABLE_CORES = 64


def test_a_job_no_cluster_can_hold_is_refused_at_submit_and_debits_nothing(api, experiment, run_id):
    agent = make_agent(api, run_id, "lifecycle-never")
    pe_id = experiment("job-lifecycle-never", [agent], budget=10.0)

    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    # No node has that many cores, so no wait admits this job.
    body = api.submission_body_for_id(api._job_id(), pe_id, agent)
    body["job"] |= cpu_job(UNAVAILABLE_CORES)
    refused = api.post_experiment_body(body)
    assert refused.status_code == 422, f"a job no cluster can hold: {refused.status_code} {refused.text}"
    assert refused.json()["reason"] == "exceeds_cluster_capacity" and "hl capacity" in refused.json()["message"], refused.text
    assert api.get(f"/experiments/{body['id']}").status_code == 404, "the refused job was stored"
    debit = api.quota_field(pe_id, agent, "used_guaranteed_acch") - quota_before
    assert abs(debit) < 1e-6, f"a job refused at submit debited {debit} AccH"


def test_cancel_while_queued_is_cancelled_and_refunded(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "lifecycle")
    # The experiment runs one unit at a time, so a job that asks for two cores fits the cluster,
    # is accepted, and is never admitted.
    pe_id = experiment("job-lifecycle", [agent], budget=10.0, max_concurrent_accelerators=1)

    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    stuck = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides=cpu_job(2),
    )

    # QUEUED and SUBMITTED are both "not yet admitted" -- the scheduler may claim a QUEUED job as
    # SUBMITTED on an attempt and revert it when admission fails.
    not_admitted = ("QUEUED", "SUBMITTED")
    exp = eventually(
        f"the admission loop's verdict on {stuck}",
        lambda: api.experiment(stuck),
        accept=lambda e: e.get("not_admitted_reason") not in (None, "awaiting_admission"),
        reject=lambda e: e["status"] not in not_admitted,
        deadline=deadline,
    )
    assert exp.get("not_admitted_reason") == "concurrency_cap", (
        f"the job over its experiment's concurrency has wrong not_admitted_reason={exp.get('not_admitted_reason')!r}"
    )

    assert_stable(
        "the job over the cap is never falsely admitted, no flapping into a terminal state",
        lambda: api.experiment(stuck)["status"],
        ok=lambda s: s in not_admitted,
        duration=5,
    )

    # The still-QUEUED row IS the desired quota reservation; there is no reservation metric or
    # secondary table: 0.02h * 2 cores * TEST_CPU_ACCH_RATE.
    want = 0.02 * 2 * TEST_CPU_ACCH_RATE
    debit = api.quota_field(pe_id, agent, "used_guaranteed_acch") - quota_before
    assert round(debit, 6) == pytest.approx(want, abs=1e-6), (
        f"expected {want} AccH desired reservation for the QUEUED job, got {debit}"
    )

    api.cancel_job(stuck)
    final = eventually(
        f"{stuck} to terminate as CANCELLED",
        lambda: api.experiment(stuck),
        accept=lambda e: e["status"] in ("CANCELLED", "EVICTED", "REJECTED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "CANCELLED", (
        f"cancelling a still-QUEUED job produced {final['status']}, expected CANCELLED"
    )

    debit_after = api.quota_field(pe_id, agent, "used_guaranteed_acch") - quota_before
    assert abs(debit_after) < 1e-6, (
        f"guaranteed quota still shows {debit_after} AccH debited after cancelling a never-run job -- refund incomplete"
    )


def test_cancel_while_running_is_evicted_and_terminal(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "lifecycle-run")
    pe_id = experiment("job-lifecycle-run", [agent], budget=10.0)

    runner = api.submit_job(pe_id, agent, hours=0.03)
    running = eventually(
        f"{runner} to run",
        lambda: api.experiment(runner),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert not running.get("not_admitted_reason"), (
        f"admitted job retained stale not_admitted_reason={running.get('not_admitted_reason')!r}"
    )

    api.cancel_job(runner)
    final = eventually(
        f"{runner} to terminate",
        lambda: api.experiment(runner),
        accept=lambda e: e["status"] in ENDED,
        deadline=deadline,
    )
    assert final["status"] == "CANCELLED", f"expected CANCELLED after cancelling a RUNNING job, got {final['status']}"

    assert_stable(
        "a cancel is terminal, unlike preemption (no auto-resubmission)",
        lambda: api.experiment(runner)["status"],
        ok=lambda s: s == "CANCELLED",
        duration=5,
    )


def test_closing_experiment_evicts_running_job_and_blocks_submission(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "lifecycle-close")
    pe_id = experiment("job-lifecycle-close", [agent], budget=10.0)

    live = api.submit_job(pe_id, agent, hours=0.03)
    running = eventually(
        f"{live} to run before close",
        lambda: api.experiment(live),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ENDED,
        deadline=deadline,
    )
    assert running["status"] == "RUNNING"

    api.close_platform_experiment(pe_id)

    # A closed platform experiment takes no new hypothesis either, so the job names the live one's.
    code, _ = api.submit_job_expect(pe_id, agent, hours=0.02, hyp_id=running["hypothesis_id"])
    assert code >= 400, f"submission against a closed platform experiment was accepted (HTTP {code})"

    # reconcile_interval_seconds (30s default) is a self-healing sweep, not synchronous with the
    # close call -- give it real time to run before checking the outcome.
    final = eventually(
        f"{live} to settle after its platform experiment closed",
        lambda: api.experiment(live),
        accept=lambda e: e["status"] in ENDED,
        deadline=deadline,
    )
    if final["status"] == "COMPLETED":
        return  # job finished on its own before the close-triggered sweep reached it -- also correct
    assert final["status"] == "EVICTED", (
        f"expected the still-RUNNING job to be evicted (or complete first) after its platform "
        f"experiment closed, got status={final['status']}"
    )
    assert final.get("eviction_reason") == "experiment_closed", (
        f"job was evicted after close but with an unexpected reason: {final.get('eviction_reason')}"
    )
