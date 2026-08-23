"""Admission correctness on the CPU dimension, ported 1:1 from tests/scenarios/mixed-admission.sh:
mixed CPU+accelerator jobs are jointly fit-checked, fractional (millicore) CPU survives verbatim to
the pod spec, a CPU-only job (no accelerator dimension at all) is admitted and billed on CPU alone,
and a submission missing a required resource field is rejected rather than silently defaulted.
API-only, parallel-safe.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.cluster import pod_cpu_resources
from support.wait import eventually

pytestmark = pytest.mark.parallel

PROBE_JOB_HOURS = 0.003


@pytest.fixture
def mixed_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "mixed")
    pe_id = experiment("mixed-admission", [agent], budget=1.0)
    return pe_id, agent


def _finish_if_completed(api, job_id, deadline):
    """Files a finding once a job completes -- otherwise the next submission for the same
    agent+PE gets 403 summary_required (see tests/lib/api.sh's needs_summary comment)."""
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
        accept=lambda s: s in ("RUNNING", "ADMITTED", "QUEUED", "SUBMITTED", "REJECTED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert status not in ("RUNNING", "ADMITTED"), (
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
    req, lim = pod_cpu_resources(job_id)
    assert req == "333m" and lim == "333m", (
        f"fractional CPU precision lost: got request={req} limit={lim}, expected 333m/333m"
    )
    _finish_if_completed(api, job_id, deadline)


def test_cpu_only_job_admits_and_bills_on_cpu_alone(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": "500m", "accelerator_count": 0, "accelerator_type": None, "acceptable_accelerator_types": None},
    )
    assert code < 400, f"submission of a CPU-only (accelerator_count=0) job was rejected outright (HTTP {code})"
    admitted = eventually(
        f"{job_id} to reach RUNNING/COMPLETED",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert admitted["status"] in ("RUNNING", "COMPLETED"), (
        f"CPU-only job never reached RUNNING/COMPLETED (status={admitted['status']}) -- CPU-only admission may be broken"
    )
    assert admitted.get("accelerator_count") == 0
    est_cost = admitted.get("estimated_cost_acch") or 0
    assert est_cost == 0, (
        f"CPU-only job admitted but estimated_cost_acch={est_cost} (expected 0 -- an accelerator-free job "
        "must not bill accelerator-hours)"
    )
    _finish_if_completed(api, job_id, deadline)


def test_unset_required_resource_is_rejected_not_defaulted(api, mixed_pe, deadline):
    pe_id, agent = mixed_pe
    code, job_id = api.submit_job_expect(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"cpu": None, "accelerator_count": 1, "accelerator_type": "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
    )
    if code >= 400:
        return  # rejected at submission time -- also correct
    final = eventually(
        f"{job_id} to settle",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "ADMITTED", "REJECTED", "FAILED"),
        deadline=deadline,
    )
    assert final["status"] == "REJECTED", (
        f"submission with unset cpu request was accepted (HTTP {code}, status={final['status']}) instead of rejected"
    )
