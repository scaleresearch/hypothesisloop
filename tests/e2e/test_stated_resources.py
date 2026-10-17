"""Every job states its cpu, memory and storage as quantities: one left out, or "max", is 422
required, naming the field in problems, in a group as at the top of the job."""
from __future__ import annotations

import pytest

from conftest import make_agent

pytestmark = pytest.mark.parallel

GROUP = {"name": "trainer", "replicas": 1, "cpu": "1", "memory": "128Mi", "storage": "512Mi"}


@pytest.fixture(scope="module")
def stated_pe(api, run_id):
    agent = make_agent(api, run_id, "stated")
    pe_id = api.create_platform_experiment(f"stated-{run_id}", 20.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.declare_baseline_and_wait_running(pe_id)
    yield pe_id, agent
    api.close_platform_experiment(pe_id)


@pytest.mark.parametrize("field", ["cpu", "memory", "storage"])
def test_a_quantity_left_out_or_stated_as_max_is_refused(api, stated_pe, field):
    pe_id, agent = stated_pe
    stated = api.submission_body_for_id(api._job_id(), pe_id, agent)
    whole = {k: v for k, v in stated["job"].items() if k not in ("cpu", "memory", "storage")}
    for said, job in (
        (f"job.{field} is required", {k: v for k, v in stated["job"].items() if k != field}),
        (f'job.{field} is "max"', {**stated["job"], field: "max"}),
        (f'job.groups["trainer"].{field} is required', {**whole, "groups": [{k: v for k, v in GROUP.items() if k != field}]}),
        (f'job.groups["trainer"].{field} is "max"', {**whole, "groups": [{**GROUP, field: "max"}]}),
    ):
        r = api.post_experiment_body({**stated, "job": job})
        assert r.status_code == 422, f"{job}: answered {r.status_code}, want 422: {r.text}"
        refusal = r.json()
        assert refusal["reason"] == "required" and said in refusal["message"], f"{job}: {refusal}"
        assert [p["field"] for p in refusal["problems"]] == [said.split(" is ")[0]], f"{job}: {refusal}"
