"""Two submissions of one agent fired at the same instant, each fitting its quota alone and not
both: exactly one is committed and the other refused for its quota. Zero winners would be a
provisional-row visibility race, two an over-admission."""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

import pytest

from conftest import make_agent

pytestmark = pytest.mark.parallel


def test_concurrent_same_agent_requests_race_one_quota_boundary(api, run_id):
    agent = make_agent(api, run_id, "agent-quota-race")
    pe_id = api.create_platform_experiment(f"concurrent-quota-race-{run_id}", 1.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.declare_baseline_and_wait_running(pe_id)

    # A run of one researcher has one stage, so the agent holds its whole share from the start.
    # Each job reserves 60% of it on its one core.
    allocation = api.quota_field(pe_id, agent, "guaranteed_accelerator_hours")
    hours = round(allocation * 0.6, 4)

    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(lambda _: api.submit_job_expect(pe_id, agent, hours=hours), range(2)))

    accepted = [jid for code, jid in results if 200 <= code < 300]
    refused = [code for code, _ in results if not 200 <= code < 300]
    assert len(accepted) == 1 and refused == [402], f"the race was answered {results}, want one accepted and one 402 insufficient_quota"

    used = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    assert abs(used - hours) < 1e-6, f"the one committed job reserves {used} AccH, want {hours}"
    api.cancel_job(accepted[0])
    api.close_platform_experiment(pe_id)
