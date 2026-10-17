"""Two platform experiments sharing one agent never leak usage into each other's quota ledger,
and a job that holds no priced unit is refused."""
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


def test_a_job_that_holds_no_priced_unit_is_refused(api, two_pes_with_pe1_usage):
    _, pe2, agent, _ = two_pes_with_pe1_usage
    for overrides in ({"cpu": "500m"}, {"cpu": "500m", "accelerator_count": 0, "accelerator_type": "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"}):
        code, _ = api.submit_job_expect(pe2, agent, hours=0.02, job_overrides=overrides)
        assert code == 422, f"a job of {overrides} holds no accelerator and no whole core and was answered {code}, want 422"


def test_pe2_quota_unaffected_by_pe1_usage(api, two_pes_with_pe1_usage, deadline):
    pe1, pe2, agent, job1 = two_pes_with_pe1_usage
    eventually(
        f"{job1} to reach a terminal/queued status",
        lambda: api.experiment(job1)["status"],
        accept=lambda s: s in ("COMPLETED", "FAILED", "EVICTED", "QUEUED"),
        deadline=deadline,
    )
    pe1_used = api.quota_field(pe1, agent, "used_guaranteed_acch")
    pe2_used = api.quota_field(pe2, agent, "used_guaranteed_acch")
    # PE2 has no job of its own in this test, so any usage in its ledger leaked in from PE1 via a
    # quota-map key collision (AgentID-only vs (AgentID, PEID)).
    assert pe2_used <= 1e-9, (
        f"PE2 shows guaranteed usage ({pe2_used}) with no job of its own -- quota map may key on "
        f"AgentID alone, leaking PE1's usage (PE1 used={pe1_used})"
    )
