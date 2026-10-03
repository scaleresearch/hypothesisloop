"""Admission correctness on the CPU dimension, ported 1:1 from tests/scenarios/mixed-admission.sh:
mixed CPU+accelerator jobs are jointly fit-checked, fractional (millicore) CPU survives verbatim to
the pod spec, a CPU-only job (no accelerator dimension at all) is admitted and billed on CPU alone,
and a submission missing a required resource field is rejected rather than silently defaulted.
API-only, parallel-safe.
"""
from __future__ import annotations

import pytest

from conftest import TEST_CPU_ACCH_RATE, TEST_CPU_TYPE, cpu_job, make_agent
from support.cluster import pod_resource
from support.wait import eventually

pytestmark = pytest.mark.parallel

PROBE_JOB_HOURS = 0.003


@pytest.fixture
def mixed_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "mixed")
    pe_id = experiment("mixed-admission", [agent], budget=1.0)
    return pe_id, agent


def _finish_if_completed(api, job_id, deadline):
    """Files a finding once a job completes -- otherwise the next job of the same agent+PE
    is held in the queue with not_admitted_reason summary_gate."""
    final = eventually(
        f"{job_id} to reach a terminal status",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    if final["status"] == "COMPLETED":
        api.file_finding(job_id)
    return final


def test_impossible_cpu_request_is_not_admitted(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "999999", "accelerator_count": 1, "accelerator_type": "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
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


def test_small_fittable_mixed_job_admits_and_runs(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "250m", "accelerator_count": 1, "accelerator_type": "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
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


def test_fractional_cpu_retains_millicore_precision_in_pod_spec(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "333m", "accelerator_count": 1, "accelerator_type": "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
    )
    assert code < 400, f"submission of a valid fractional-CPU (333m) job was rejected outright (HTTP {code})"
    admitted = eventually(
        f"{job_id} to reach RUNNING/COMPLETED",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert admitted["status"] in ("RUNNING", "COMPLETED")
    req, lim = pod_resource(job_id, "cpu")
    assert req == "333m" and lim == "333m", (
        f"fractional CPU precision lost: got request={req} limit={lim}, expected 333m/333m"
    )
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
    req, lim = pod_resource(job_id, "cpu")
    assert req == "2" and lim == "2", f"pod cpu: request={req} limit={lim}, want 2/2"
    assert pod_resource(job_id, "nvidia.com/gpu") == (None, None), "a job on cores was given an accelerator"
    _finish_if_completed(api, job_id, deadline)


def test_a_job_on_cores_states_whole_cores_and_one_count_and_no_accelerator(api, mixed_pe):
    pe_id, agent = mixed_pe
    for overrides in ({**cpu_job(2), "accelerator_count": 4}, {**cpu_job(2), "cpu": "500m"}, {**cpu_job(2), "cpu": "1.5"},
                      {**cpu_job(2), "acceptable_accelerator_types": ["nvidia.com/gpu.product=NVIDIA-L40"]}):
        code, _ = api.submit_job_expect(pe_id, agent, hours=PROBE_JOB_HOURS, job_overrides=overrides)
        assert code == 400, f"{overrides}: answered {code}, want 400"



def test_unset_required_resource_is_rejected_not_defaulted(api, mixed_pe):
    pe_id, agent = mixed_pe
    body = api.submission_body_for_id(api._job_id(), pe_id, agent, hours=PROBE_JOB_HOURS)
    del body["job"]["cpu"]
    r = api.post_experiment_body(body)
    assert r.status_code == 422 and r.json()["reason"] == "cpu_required", f"a job that states no cpu: {r.status_code} {r.text}"
