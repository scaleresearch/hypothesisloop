"""True concurrency (not "back-to-back sequential") races: N guaranteed jobs are fired at the
scheduler/API at the same instant, racing for one shared boundary whose sum of requests exceeds
what it actually has. Unlike tests/e2e/test_capacity_safety.py (which submits sequentially and
checks quota debit isn't doubled), this scenario exercises the actual concurrent-write path --
multiple submitJob calls landing inside the same or adjacent scheduler ticks / PostgreSQL
transactions -- which is exactly where a reservation-write race would show up as over-admission or
over-commit.

Ported 1:1 from tests/scenarios/concurrent-admission-race.sh, split into its two independent
races:

  1. `test_concurrent_accelerator_requests_never_exceed_node_capacity` -- three half-node-capacity
     jobs fired at once, exactly two of which can fit. However the scheduler interleaves the
     concurrent submitJob calls, it must never admit more total accelerator count than the node
     physically has, and exactly one job must lose the race and stay QUEUED (the class of bug
     fixed in loop_preempt.go per findings.md, but only ever exercised there by sequential
     back-to-back submission -- this is the first test that fires true concurrent requests).
     Cluster-exclusive: it deliberately saturates a shared accelerator pool.

  2. `test_concurrent_same_agent_requests_race_one_quota_boundary` -- two same-agent submissions
     that each fit alone but not both against one PostgreSQL quota boundary (phase-1 guaranteed
     allocation 0.4 AccH, each request 0.24 AccH). The correct outcome is exactly one committed
     desired row and one 4xx; zero winners would expose the old provisional-row visibility race,
     two would expose over-admission. This half needs no real accelerator capacity -- the raced
     jobs are never expected to run, only to be admitted or rejected at the quota-commit boundary
     -- so it stays in the `parallel` lane with its own run-scoped agent/PE.

PROVEN against the live stack: 3x solo green (see final verification run in the port's report).
"""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

import pytest

from conftest import make_agent
from support.cluster import L40, H100, job_resource_absent, node_allocatable_gpu_total
from support.wait import eventually

ADMITTED_STATUSES = ("SUBMITTED", "ADMITTED", "RUNNING")
TERMINAL_STATUSES = ("COMPLETED", "FAILED", "EVICTED", "REJECTED")


@pytest.mark.exclusive("l40")
def test_concurrent_accelerator_requests_never_exceed_node_capacity(api, run_id, deadline):
    capacity = node_allocatable_gpu_total(L40)
    if capacity < 2 or capacity % 2 != 0:
        pytest.skip(
            f"concurrent admission race fixture needs a positive even L40 capacity, observed {capacity}"
        )
    per_job = capacity // 2
    n_jobs = 3  # three half-capacity requests: exactly two fit and one must lose the race.
    expect_admitted = 2

    agents = [make_agent(api, run_id, f"agent-race-{i}") for i in range(n_jobs)]
    pe_id = api.create_platform_experiment(f"concurrent-race-{run_id}", 50.0, len(agents))
    # An autonomous agent defaults to the burst_only tier; this scenario submits tier="guaranteed"
    # jobs and needs the reserved quota that tier draws from, so signup explicitly overrides to
    # quota_tier="guaranteed" (see test_max_resource_sentinel.py for the same override).
    for a in agents:
        api.signup_ok(pe_id, a, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    def fire(agent: str) -> str:
        return api.submit_job(
            pe_id, agent, hours=0.05,
            job_overrides={"accelerator_type": L40, "accelerator_count": per_job},
        )

    with ThreadPoolExecutor(max_workers=n_jobs) as pool:
        jobs = list(pool.map(fire, agents))
    assert len(jobs) == n_jobs, f"only {len(jobs)} of {n_jobs} concurrent submissions were accepted at the HTTP layer"

    def capacity_admitted() -> int:
        return sum(1 for j in jobs if api.experiment(j)["status"] in ADMITTED_STATUSES)

    # Wait only for the capacity-sized winning set: the over-capacity loser is expected to remain
    # QUEUED, so requiring every job to leave QUEUED would be contradictory.
    eventually(
        "capacity-sized winning set to be admitted",
        capacity_admitted,
        accept=lambda n: n >= expect_admitted,
        deadline=deadline,
    )

    statuses = {j: api.experiment(j)["status"] for j in jobs}
    admitted_jobs = [j for j, s in statuses.items() if s in ADMITTED_STATUSES]
    queued_jobs = [j for j, s in statuses.items() if s == "QUEUED"]

    # The core race-safety invariant: however the scheduler interleaves concurrent submitJob
    # calls, it must never admit more total accelerator count than the node physically has.
    total_admitted_accelerators = len(admitted_jobs) * per_job
    assert total_admitted_accelerators <= capacity, (
        f"OVER-ADMISSION under concurrent submission: {len(admitted_jobs)} jobs "
        f"({total_admitted_accelerators} accelerators) admitted against only {capacity} accelerators "
        f"available -- reservation race. statuses={statuses}"
    )
    assert len(admitted_jobs) == expect_admitted, (
        f"expected exactly {expect_admitted} admitted, got {len(admitted_jobs)}. statuses={statuses}"
    )
    assert len(queued_jobs) >= 1, (
        f"no job was left QUEUED even though requests ({n_jobs}x{per_job}={n_jobs * per_job}) exceed "
        f"capacity ({capacity}) -- every job appears admitted, which is impossible if capacity "
        f"accounting is correct. statuses={statuses}"
    )

    for j in admitted_jobs:
        api.cancel_job(j)
    for j in admitted_jobs:
        final = eventually(
            f"{j} to stop after race assertions",
            lambda j=j: api.experiment(j),
            accept=lambda e: e["status"] in TERMINAL_STATUSES,
            deadline=deadline,
        )
        assert final["status"] in TERMINAL_STATUSES, f"{j} did not stop after cancellation (status={final['status']})"
        eventually(
            f"{j} Kubernetes Job to be removed after cancellation",
            lambda j=j: job_resource_absent(j),
            accept=lambda absent: absent,
            deadline=deadline,
        )

    api.close_platform_experiment(pe_id)


@pytest.mark.parallel
def test_concurrent_same_agent_requests_race_one_quota_boundary(api, run_id, deadline):
    agent = make_agent(api, run_id, "agent-quota-race")
    pe_id = api.create_platform_experiment(f"concurrent-quota-race-{run_id}", 1.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    # Phase-1 guaranteed allocation is 0.4 AccH. Each job reserves 0.24 AccH, so either job fits
    # alone but both cannot. The correct outcome is exactly one committed desired row and one 4xx;
    # zero winners exposes the old provisional-row visibility race, while two exposes
    # over-admission.
    def fire(_i: int) -> tuple[int, str]:
        return api.submit_job_expect(
            pe_id, agent, hours=0.06,
            job_overrides={"accelerator_type": H100, "accelerator_count": 4},
        )

    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(fire, range(2)))

    accepted = [(code, jid) for code, jid in results if 200 <= code < 300]
    rejected = [(code, jid) for code, jid in results if 400 <= code < 500]
    unexpected = [(code, jid) for code, jid in results if not (200 <= code < 300 or 400 <= code < 500)]
    assert not unexpected, f"same-agent quota race returned unexpected HTTP status(es): {unexpected}"
    assert len(accepted) == 1 and len(rejected) == 1, (
        f"same-agent quota race produced accepted={len(accepted)} rejected={len(rejected)}; expected 1/1 "
        f"(results={results})"
    )

    winner_job = accepted[0][1]
    used = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    assert round(used, 6) == 0.24, (
        f"the sole committed PostgreSQL row contributes {used} AccH desired usage; expected exactly 0.24"
    )

    api.cancel_job(winner_job)
    # Cancellation is async: cluster-agent still has to tear the pod/Job down. Waiting for that
    # before closing keeps this test's H100 footprint from lingering into whatever exclusive-lane
    # scenario runs next (e.g. node-and-daemonset-faults, which needs a real free H100 node to
    # reschedule onto) -- mirrors the wait already done above for the capacity-race jobs.
    eventually(
        f"{winner_job} Kubernetes Job to be removed after cancellation",
        lambda: job_resource_absent(winner_job),
        accept=lambda absent: absent,
        deadline=deadline,
    )
    api.close_platform_experiment(pe_id)
