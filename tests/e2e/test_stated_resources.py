"""Every job states its cpu, memory and storage as quantities, and a job holds what it states:
  1. one left out, or "max", is 422 required, naming
     the field in problems, in a group as at the top of the job.
  2. a stated cpu above the job's per-accelerator share of every eligible node is never admitted
     (exceeds_accelerator_share).

Cluster-exclusive: sizing the oversized job requires knowing no other job is competing for the
same node's accelerators, or its "impossible" number might not be.
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCELERATOR_TYPE, make_agent
from support.cluster import single_eligible_node_cpu_fair_share
from support.wait import eventually

pytestmark = pytest.mark.exclusive("cluster")

GROUP = {"name": "trainer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi", "accelerator_count": 1}


def _guaranteed_agent(api, run_id, name):
    agent = make_agent(api, run_id, name)
    pe_id = api.create_platform_experiment(f"{name}-{run_id}", 20.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.declare_baseline_and_wait_running(pe_id)
    return pe_id, agent


@pytest.mark.parametrize("field", ["cpu", "memory", "storage"])
def test_a_quantity_left_out_or_stated_as_max_is_refused(api, run_id, field):
    pe_id, agent = _guaranteed_agent(api, run_id, f"stated-{field}")
    stated = api.submission_body_for_id(api._job_id(), pe_id, agent)
    whole = {k: v for k, v in stated["job"].items() if k not in ("cpu", "memory", "storage", "accelerator_count")}
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
    api.close_platform_experiment(pe_id)


def test_a_stated_cpu_above_the_accelerator_share_is_never_admitted(api, run_id, deadline):
    share = single_eligible_node_cpu_fair_share(TEST_ACCELERATOR_TYPE)
    if share is None:
        pytest.skip(
            f"needs exactly one live node carrying {TEST_ACCELERATOR_TYPE} "
            "so the expected fair share is unambiguous regardless of scheduler placement"
        )
    node, _, share_milli = share
    if share_milli < 1:
        pytest.skip(f"node {node} computes a 0m fair share")

    pe_id, agent = _guaranteed_agent(api, run_id, "over-share")
    impossible_cpu = f"{share_milli * 2}m"
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=0.02,
        job_overrides={"cpu": impossible_cpu, "accelerator_count": 1, "acceptable_accelerator_types": []},
    )
    if code < 400:
        # A queued row reads awaiting_admission until the admission loop has looked at it.
        stuck = eventually(
            f"{job_id} to settle",
            lambda: api.experiment(job_id),
            accept=lambda e: e["status"] in ("RUNNING", "COMPLETED")
            or e.get("not_admitted_reason") == "exceeds_accelerator_share",
            deadline=deadline,
        )
        assert stuck["status"] not in ("RUNNING", "COMPLETED"), (
            f"stated cpu ({impossible_cpu}) beyond every eligible node's fair share "
            f"({share_milli}m) was admitted (status={stuck['status']})"
        )
        assert stuck.get("not_admitted_reason") == "exceeds_accelerator_share", stuck
        assert stuck.get("not_admitted_message"), f"the reason came with no sentence: {stuck}"
        api.cancel_job(job_id)

    api.close_platform_experiment(pe_id)
