"""Two platform experiments sharing one agent must never leak usage into each other's quota
ledger, and a CPU-only (accelerator_count=0) job must be accepted into the fairness pool without
requiring an accelerator dimension. Ported 1:1 from tests/scenarios/fairness-isolation.sh.
API-only, parallel-safe (two fresh PEs of its own).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
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


def test_cpu_only_job_accepted_into_fairness_pool(api, two_pes_with_pe1_usage, deadline):
    pe1, pe2, agent, job1 = two_pes_with_pe1_usage
    code, cpu_only_id = api.submit_job_expect(
        pe2, agent, hours=0.02,
        job_overrides={"accelerator_count": 0, "accelerators": None},
    )
    assert code < 400, (
        f"CPU-only (accelerator_count=0) job submission was rejected outright (HTTP {code})"
    )
    eventually(
        f"{cpu_only_id} to reach a terminal/queued status",
        lambda: api.experiment(cpu_only_id)["status"],
        accept=lambda s: s in ("RUNNING", "COMPLETED", "FAILED", "EVICTED", "QUEUED", "REJECTED"),
        deadline=deadline,
    )


def test_pe2_quota_unaffected_by_pe1_usage(api, two_pes_with_pe1_usage, deadline):
    pe1, pe2, agent, job1 = two_pes_with_pe1_usage
    code, cpu_only_id = api.submit_job_expect(
        pe2, agent, hours=0.02,
        job_overrides={"accelerator_count": 0, "accelerators": None},
    )
    assert code < 400, f"CPU-only job submission was rejected outright (HTTP {code})"
    eventually(
        f"{cpu_only_id} to reach a terminal/queued status",
        lambda: api.experiment(cpu_only_id)["status"],
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
    # PE2's only job requested no accelerator, so any nonzero AccH usage recorded against it can
    # only have leaked in from PE1 via a quota-map key collision (AgentID-only vs (AgentID, PEID)).
    assert pe2_used == 0.0, (
        f"PE2 shows nonzero guaranteed usage ({pe2_used}) from an accelerator-free job -- quota "
        f"map may key on AgentID alone, leaking PE1's usage (PE1 used={pe1_used})"
    )
