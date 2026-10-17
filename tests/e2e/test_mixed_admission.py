"""Admission on the CPU dimension: a job on an accelerator states its cpu beside it and both are
fit together, fractional cpu reaches the runtime as stated, a job on cores alone is billed core
hours, and a submission missing a required resource field is refused rather than defaulted.
"""
from __future__ import annotations

import pytest

from conftest import TEST_CPU_ACCH_RATE, TEST_CPU_TYPE, cpu_job, make_agent
from support.api import ENDED
from support.wait import eventually

pytestmark = pytest.mark.parallel

PROBE_JOB_HOURS = 0.003


@pytest.fixture
def mixed_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "mixed")
    pe_id = experiment("mixed-admission", [agent], budget=1.0)
    return pe_id, agent


def _finish_if_completed(api, job_id, deadline):
    """Waits for a job to end, and files its finding once it completed."""
    final = eventually(
        f"{job_id} to reach a terminal status",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ENDED,
        deadline=deadline,
    )
    if final["status"] == "COMPLETED":
        api.file_finding(job_id)
    return final


def test_impossible_cpu_request_is_not_admitted(api, mixed_pe, deadline, accelerator):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "999999", "accelerator_count": 1, "accelerator_type": accelerator.type},
    )
    if code >= 400:
        return  # rejected outright at submission -- also a correct fail-closed outcome
    status = eventually(
        f"{job_id} to settle",
        lambda: api.experiment(job_id)["status"],
        accept=lambda s: s in ("RUNNING", "QUEUED", "SUBMITTED", "REJECTED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert status != "RUNNING", (
        "job with an impossible CPU request (999999 cores) was admitted -- mixed CPU+accelerator fit not jointly checked"
    )


def test_small_fittable_mixed_job_admits_and_runs(api, mixed_pe, deadline, accelerator):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "250m", "accelerator_count": 1, "accelerator_type": accelerator.type},
    )
    assert code < 400, f"submission of a small, fittable mixed CPU+accelerator job was rejected outright (HTTP {code})"
    admitted = eventually(
        f"{job_id} to admit",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert admitted["status"] in ("RUNNING", "COMPLETED")
    _finish_if_completed(api, job_id, deadline)


def test_fractional_cpu_reaches_the_runtime_as_stated(api, mixed_pe, deadline, accelerator):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "333m", "accelerator_count": 1, "accelerator_type": accelerator.type},
    )
    assert code < 400, f"submission of a valid fractional-CPU (333m) job was rejected outright (HTTP {code})"
    admitted = eventually(
        f"{job_id} to reach RUNNING/COMPLETED",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert admitted["resolved_job"]["cpu"] == "333m", f"the spec the runtime was given holds other cpu: {admitted['resolved_job']}"
    _finish_if_completed(api, job_id, deadline)


def test_a_job_that_states_only_its_cores_runs_on_them_and_is_billed_core_hours(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(pe_id, agent, hours=PROBE_JOB_HOURS, job_overrides=cpu_job(2))
    assert code < 400, f"a job that states cpu: 2 and no accelerator was rejected outright (HTTP {code})"
    admitted = eventually(
        f"{job_id} to reach RUNNING/COMPLETED",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert admitted["accelerator_type"] == TEST_CPU_TYPE and admitted.get("accelerator_count") == 2, (
        f"cpu: 2 did not resolve to two units of the CPU type: {admitted}"
    )
    assert admitted["job"]["memory"] and admitted["job"]["cpu"] == "2", f"the job's cores changed at submission: {admitted['job']}"
    assert admitted["resolved_job"]["cpu"] == "2", f"the spec the runtime was given holds other cores: {admitted['resolved_job']}"
    want_cost = 2 * TEST_CPU_ACCH_RATE * PROBE_JOB_HOURS
    est_cost = admitted.get("estimated_cost_acch") or 0
    assert abs(est_cost - want_cost) < 1e-9, f"estimated_cost_acch={est_cost}, want {want_cost}: two cores are two core-hours an hour"
    _finish_if_completed(api, job_id, deadline)


def test_a_job_on_cores_states_whole_cores_and_one_count_and_no_accelerator(api, mixed_pe):
    pe_id, agent = mixed_pe
    for overrides in ({**cpu_job(2), "accelerator_count": 4}, {**cpu_job(2), "cpu": "500m"}, {**cpu_job(2), "cpu": "1.5"},
                      {**cpu_job(2), "acceptable_accelerator_types": ["nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"]}):
        code, _ = api.submit_job_expect(pe_id, agent, hours=PROBE_JOB_HOURS, job_overrides=overrides)
        assert code == 422, f"{overrides}: answered {code}, want 422"



def test_unset_required_resource_is_rejected_not_defaulted(api, mixed_pe):
    pe_id, agent = mixed_pe
    body = api.submission_body_for_id(api._job_id(), pe_id, agent, hours=PROBE_JOB_HOURS)
    for field in ("cpu", "memory", "storage"):
        job = {key: value for key, value in body["job"].items() if key != field}
        r = api.post_experiment_body({**body, "job": job})
        assert r.status_code == 422 and r.json()["reason"] == "required", f"a job that states no {field}: {r.status_code} {r.text}"
        assert f"job.{field} is required" in r.json()["message"] and [p["field"] for p in r.json()["problems"]] == [f"job.{field}"], r.text
    assert api.get(f"/experiments/{body['id']}").status_code == 404, "a refused job was stored"
