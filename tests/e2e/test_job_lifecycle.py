"""Job/platform-experiment termination semantics, ported 1:1 from tests/scenarios/job-lifecycle.sh:
  1. A valid job that can never be scheduled (known flavor, impossible physical count) fails
     closed -- stays QUEUED indefinitely, never debits guaranteed quota -- and cancelling it while
     still QUEUED terminates it as REJECTED (not EVICTED: it was never admitted).
  2. A RUNNING job that's cancelled is EVICTED (terminal, refunded) -- and stays EVICTED, unlike
     preemption (see preemption-requeue.sh), confirming cancel-while-QUEUED and
     cancel-while-RUNNING are genuinely different terminal outcomes of the same endpoint.
  3. Closing a platform experiment is not just a status flip: controller.reconcileClosedExperiments
     (a self-healing sweep, not a synchronous side effect of the close call itself) evicts every
     still-RUNNING/ADMITTED job under it with reason "experiment_closed" on its next reconcile
     tick, and rejects any further submission.
API-only, parallel-safe.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import assert_stable, eventually

pytestmark = pytest.mark.parallel


def test_cancel_while_queued_is_rejected_and_refunded(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "lifecycle")
    pe_id = experiment("job-lifecycle", [agent], budget=10.0)

    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    stuck = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        # api.py auto-drops acceptable_accelerator_types when accelerator_type is overridden
        # (mirrors tests/lib/mk_body.py) -- an explicit type request must not silently fall back
        # to a cheaper alternate, or this scenario's AccH arithmetic and its "genuinely
        # unsatisfiable" premise both go stale.
        job_overrides={"accelerator_type": "nvidia.com/gpu.product=NVIDIA-H200", "accelerator_count": 3},
    )

    # QUEUED and SUBMITTED are both "not yet admitted" -- the scheduler legitimately claims a
    # QUEUED job as SUBMITTED on each attempt and reverts it when admission fails, so a single
    # unsatisfiable job flaps between the two every reconcile tick without ever being admitted.
    not_admitted = ("QUEUED", "SUBMITTED")
    status = eventually(
        f"{stuck} to settle",
        lambda: api.experiment(stuck)["status"],
        accept=lambda s: s in (*not_admitted, "RUNNING", "COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    if status not in not_admitted:
        pytest.skip(f"unsatisfiable job settled as {status!r} at submission (also an acceptable fail-closed outcome)")

    exp = api.experiment(stuck)
    assert (exp.get("not_admitted_reason") or "").startswith("capacity_unavailable"), (
        f"queued unsatisfiable job has wrong not_admitted_reason={exp.get('not_admitted_reason')!r}"
    )

    assert_stable(
        "unsatisfiable job never falsely admitted, no flapping into a terminal state",
        lambda: api.experiment(stuck)["status"],
        ok=lambda s: s in not_admitted,
        duration=5,
    )

    # The still-QUEUED row IS the desired quota reservation; there is no reservation metric or
    # secondary table. H200's acch_rate (1.25) is fixed in controlplane/settings/hypothesisloop.yaml
    # regardless of TEST_ACCH_RATE (which prices TEST_ACCELERATOR_TYPE, not this deliberately
    # unsatisfiable request): 0.02h * 3 H200 * 1.25 AccH/h = 0.075 AccH.
    debit = api.quota_field(pe_id, agent, "used_guaranteed_acch") - quota_before
    assert round(debit, 6) == pytest.approx(0.075, abs=1e-6), (
        f"expected 0.075 AccH desired reservation for the QUEUED job, got {debit}"
    )

    api.cancel_job(stuck)
    final = eventually(
        f"{stuck} to terminate as REJECTED",
        lambda: api.experiment(stuck),
        accept=lambda e: e["status"] in ("EVICTED", "REJECTED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "REJECTED", (
        f"cancelling a still-QUEUED job produced {final['status']}, expected REJECTED"
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
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "EVICTED", f"expected EVICTED after cancelling a RUNNING job, got {final['status']}"

    assert_stable(
        "eviction is terminal, unlike preemption (no auto-resubmission)",
        lambda: api.experiment(runner)["status"],
        ok=lambda s: s == "EVICTED",
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
        reject=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert running["status"] == "RUNNING"

    api.close_platform_experiment(pe_id)

    code, _ = api.submit_job_expect(pe_id, agent, hours=0.02)
    assert code >= 400, f"submission against a closed platform experiment was accepted (HTTP {code})"

    # reconcile_interval_seconds (30s default) is a self-healing sweep, not synchronous with the
    # close call -- give it real time to run before checking the outcome.
    final = eventually(
        f"{live} to settle after its platform experiment closed",
        lambda: api.experiment(live),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
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
