"""Two platform experiments sharing one agent must never leak usage into each other's quota
ledger, and a job on a CPU class (cores, no accelerator) must be accepted into the fairness pool
and charged for its cores alone. Ported 1:1 from tests/scenarios/fairness-isolation.sh.
API-only, parallel-safe (two fresh PEs of its own).
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCH_RATE, TEST_CPU_ACCH_RATE, cpu_job, make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


@pytest.fixture
def two_pes_with_pe1_usage(api, experiment, run_id, deadline):
    """One agent signed up to two fresh PEs, with nonzero usage already established in PE1 --
    so a PE1/PE2 quota-key collision would be visible as contamination in PE2's ledger."""
    agent = make_agent(api, run_id, "agent-fair")
    pe1 = experiment("fairness-1", [agent], budget=1.0)
    pe2 = experiment("fairness-2", [agent], budget=1.0)

    job1 = api.submit_job(pe1, agent, hours=0.02)
    eventually(
        f"{job1} to reach a terminal/queued status",
        lambda: api.experiment(job1)["status"],
        accept=lambda s: s in ("COMPLETED", "FAILED", "EVICTED", "QUEUED", "RUNNING"),
        deadline=deadline,
    )
    return pe1, pe2, agent, job1


def test_a_cpu_class_job_is_accepted_into_the_fairness_pool(api, two_pes_with_pe1_usage, deadline):
    pe1, pe2, agent, job1 = two_pes_with_pe1_usage
    code, cpu_id = api.submit_job_expect(pe2, agent, hours=0.02, job_overrides=cpu_job())
    assert code < 400, f"a job for one core of the CPU class was rejected outright (HTTP {code})"
    eventually(
        f"{cpu_id} to reach a terminal/queued status",
        lambda: api.experiment(cpu_id)["status"],
        accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED", "QUEUED", "REJECTED"),
        deadline=deadline,
    )


def test_a_job_that_holds_no_priced_unit_is_refused(api, two_pes_with_pe1_usage):
    _, pe2, agent, _ = two_pes_with_pe1_usage
    for overrides in ({"accelerator_count": 0}, {"accelerator_count": 0, "accelerator_type": None, "acceptable_accelerator_types": None}):
        code, _ = api.submit_job_expect(pe2, agent, hours=0.02, job_overrides=overrides)
        assert code == 400, f"a job with no accelerator and no cores of a CPU class was answered {code}, want 400"


def test_pe2_quota_unaffected_by_pe1_usage(api, two_pes_with_pe1_usage, deadline):
    pe1, pe2, agent, job1 = two_pes_with_pe1_usage
    code, cpu_id = api.submit_job_expect(pe2, agent, hours=0.02, job_overrides=cpu_job())
    assert code < 400, f"a job for one core of the CPU class was rejected outright (HTTP {code})"
    eventually(
        f"{cpu_id} to reach a terminal/queued status",
        lambda: api.experiment(cpu_id)["status"],
        accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED", "QUEUED", "REJECTED"),
        deadline=deadline,
    )

    eventually(
        f"{job1} to reach a terminal/queued status",
        lambda: api.experiment(job1)["status"],
        accept=lambda s: s in ("COMPLETED", "FAILED", "EVICTED", "QUEUED"),
        deadline=deadline,
    )
    pe1_used = api.quota_field(pe1, agent, "used_guaranteed_acch")
    pe2_used = api.quota_field(pe2, agent, "used_guaranteed_acch")
    # PE2's only job held one core for at most 0.02 hours, a twenty-fifth of what PE1's
    # accelerator job costs. Usage above that can only have leaked in from PE1 via a quota-map key
    # collision (AgentID-only vs (AgentID, PEID)).
    own_cost = TEST_CPU_ACCH_RATE * 0.02
    assert own_cost < TEST_ACCH_RATE * 0.02 / 10
    assert pe2_used <= own_cost + 1e-9, (
        f"PE2 shows guaranteed usage ({pe2_used}) above the {own_cost} its one-core job can cost -- quota "
        f"map may key on AgentID alone, leaking PE1's usage (PE1 used={pe1_used})"
    )
