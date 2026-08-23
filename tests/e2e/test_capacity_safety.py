"""Two capacity-safety properties that must hold under contention, ported 1:1 from
tests/scenarios/capacity-safety.sh:
  1. Two guaranteed jobs submitted back-to-back to the same accelerator type across two scheduler
     ticks must never together be over-admitted beyond that type's real capacity, and each current
     PostgreSQL job row must contribute exactly one desired-quota reservation (no double-counting
     across reconcile ticks).
  2. When preemption must free MORE capacity than any single victim holds, either a sufficient set
     of victims is preempted (full requested footprint preserved, no silent down-sizing) or the
     requester is left QUEUED -- never partially/incorrectly admitted.
API-only, parallel-safe (uses its own accelerator type/agents, distinct from
preemption-requeue.sh's).
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCELERATOR_TYPE, TEST_ACCH_RATE, make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def test_desired_usage_stable_across_scheduler_ticks(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"cap-{i}") for i in "ab"]
    pe_id = experiment("capacity-safety", agents, budget=50.0)

    job_hours = 0.003
    expected_reservation = round(job_hours * TEST_ACCH_RATE, 6)
    qa_before = api.quota_field(pe_id, agents[0], "used_guaranteed_acch")
    qb_before = api.quota_field(pe_id, agents[1], "used_guaranteed_acch")

    job_a = api.submit_job(pe_id, agents[0], hours=job_hours, job_overrides={"accelerator_type": TEST_ACCELERATOR_TYPE})
    job_b = api.submit_job(pe_id, agents[1], hours=job_hours, job_overrides={"accelerator_type": TEST_ACCELERATOR_TYPE})

    for j in (job_a, job_b):
        eventually(
            f"{j} to settle",
            lambda j=j: api.experiment(j)["status"],
            accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED", "QUEUED"),
            deadline=deadline,
        )

    # Desired usage is derived from each current PostgreSQL job row. Reconsidering a job on later
    # scheduler ticks must not create another reservation or any metrics-side copy.
    qa_after = api.quota_field(pe_id, agents[0], "used_guaranteed_acch")
    qb_after = api.quota_field(pe_id, agents[1], "used_guaranteed_acch")
    assert round(qa_after - qa_before, 6) == pytest.approx(expected_reservation, abs=1e-6)
    assert round(qb_after - qb_before, 6) == pytest.approx(expected_reservation, abs=1e-6)

    for j in (job_a, job_b):
        final = eventually(
            f"{j} to reach a terminal state",
            lambda j=j: api.experiment(j),
            accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
            deadline=deadline,
        )
        if final["status"] == "COMPLETED":
            api.file_finding(j)



# Needs the *entire* L40 node's capacity free (8x) to prove a sufficient victim set gets
# preempted -- every other parallel test that happens to touch TEST_ACCELERATOR_TYPE (the shared
# default, L40) is real contention for that, not noise around it, so this one test can't actually
# share the default lane's concurrency the way the rest of the module does.
@pytest.mark.exclusive("l40")
def test_preemption_plans_sufficient_victim_set(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"cap-big-{i}") for i in "abc"]
    pe_id = experiment("capacity-safety-preempt", agents, budget=50.0)

    burst_a = api.submit_job(
        pe_id, agents[0], hours=0.017, tier="burst",
        job_overrides={"accelerator_type": TEST_ACCELERATOR_TYPE, "accelerator_count": 4},
    )
    burst_b = api.submit_job(
        pe_id, agents[1], hours=0.017, tier="burst",
        job_overrides={"accelerator_type": TEST_ACCELERATOR_TYPE, "accelerator_count": 4},
    )
    for bj in (burst_a, burst_b):
        status = eventually(
            f"{bj} burst victim to run",
            lambda bj=bj: api.experiment(bj)["status"],
            accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
            deadline=deadline,
        )
        assert status == "RUNNING", f"burst victim setup failed for {bj} (status={status}); victim-set assertion would be invalid"

    # The whole type's pool (8x L40 on the dev cluster), not a dynamically-observed busy count:
    # under the full suite's shared capacity, a count derived from a point-in-time global busy
    # read is racy (other tests' jobs inflate it, producing a request larger than the type's
    # entire pool -- unsatisfiable by any amount of preemption). Matches the original bash
    # scenario's fixed BIG_ACCELERATOR_COUNT=8 exactly.
    big_accelerator_count = 8
    job_big = api.submit_job(
        pe_id, agents[2], hours=0.017,
        job_overrides={"accelerator_type": TEST_ACCELERATOR_TYPE, "accelerator_count": big_accelerator_count},
    )
    # QUEUED excluded from the accept target: every fresh submission starts QUEUED -- including it
    # here would return instantly instead of giving admission/preemption the full deadline to
    # actually happen.
    status = eventually(
        f"{job_big} (requesting {big_accelerator_count}x {TEST_ACCELERATOR_TYPE}, larger than any single "
        "burst victim's share) to settle after preemption",
        lambda: api.experiment(job_big)["status"],
        accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert status in ("RUNNING", "COMPLETED"), (
        f"guaranteed job did not run after two sufficient burst victims became available for "
        f"preemption (status={status})"
    )
    admitted = api.experiment(job_big)
    assert admitted.get("accelerator_count") == big_accelerator_count, (
        f"admitted but accelerator_count={admitted.get('accelerator_count')} != requested "
        f"{big_accelerator_count} -- partial admission after preemption"
    )
