"""What the platform can tell an agent about a job that died. Ported 1:1 from
tests/scenarios/job-failure-diagnostics.sh. Both halves are about diagnosis surviving cleanup:
  1. A crashing job's own output must still be readable after its workload is gone (terminal
     status removes a job from desired state; the cluster-agent's next reconcile deletes the
     workload -- pod, logs, container termination reason with it -- so the runtime must capture
     and push that output *before* deleting).
  2. A job that can never be scheduled (a bad image) is evicted early as `unschedulable` and
     refunded, rather than sitting on a reservation until the generic stuck-pending timeout, and
     is attributed to the agent (workload fault), never the environment (infrastructure fault).

API-only and parallel-safe: neither job consumes an accelerator for any real length of time, and
neither touches shared cluster state.

PROVEN against the live stack: 3x solo green. Both halves needed quota_tier="guaranteed" at signup
(an agent-kind participant otherwise defaults to burst_only, so a guaranteed-tier job submission
402s with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def test_crashing_jobs_output_outlives_its_workload(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "failure-diag")
    pe_id = experiment("failure-diagnostics", [(agent, None, "guaranteed")], budget=10.0, report_interval_seconds=5)

    marker = f"HYPOTHESISLOOP-CRASH-MARKER-{run_id}"
    crash_job = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "command": ["/bin/sh", "-c"],
            "args": [f'echo "{marker}" >&2; exit 42'],
            "max_retries": 0,
        },
    )

    final = eventually(
        f"{crash_job} to finish",
        lambda: api.experiment(crash_job),
        accept=lambda e: e["status"] in ("FAILED", "EVICTED", "COMPLETED"),
        deadline=deadline,
    )
    # FAILED specifically: accepting EVICTED too would let an unrelated policy eviction satisfy
    # this while the crash diagnosis went missing.
    assert final["status"] == "FAILED", f"crashing job ended as {final['status']!r}, expected FAILED"

    eventually(
        "crashed job's log tail to reach the control plane",
        lambda: marker in api.logs(crash_job, n=200),
        accept=lambda found: found,
        deadline=deadline,
    )

    # Give the cluster-agent several reconcile passes to remove the workload, then re-read: the
    # record must still explain the failure.
    time.sleep(15)
    assert marker in api.logs(crash_job, n=200), "log tail disappeared once the workload was cleaned up"

    exp = api.experiment(crash_job)
    phase_detail = exp.get("phase_detail") or {}
    assert phase_detail.get("reason") == "container_failed"
    assert "container exited with code 42" in (phase_detail.get("message") or "")


def test_unschedulable_job_is_evicted_early_refunded_and_attributed_to_the_agent(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "failure-diag-unsched")
    pe_id = experiment("failure-diagnostics-unsched", [(agent, None, "guaranteed")], budget=10.0, report_interval_seconds=5)

    used_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    bad_job = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "image": "localhost/hypothesisloop-image-that-does-not-exist:nope",
            "max_retries": 0,
        },
    )

    final = eventually(
        f"{bad_job} to be terminated as unschedulable",
        lambda: api.experiment(bad_job),
        accept=lambda e: e["status"] in ("EVICTED", "FAILED", "REJECTED"),
        deadline=deadline,
    )
    assert final["status"] in ("EVICTED", "FAILED"), f"unpullable job never terminated: {final['status']!r}"
    assert final.get("eviction_reason") == "unschedulable"

    eventually(
        "unschedulable job's reservation to be returned",
        lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
        accept=lambda now: abs(now - used_before) < 1e-6,
        deadline=deadline,
    )

    if final["status"] == "EVICTED":
        assert final.get("infra_requeue_count") == 0, "workload's own bad image must not be requeued for free"
        assert final.get("attempt_count") == 0, "max_retries=0 means the job ended on its first attempt"

    workload_n = api.eviction_class_count(pe_id, agent, "workload")
    infra_n = api.eviction_class_count(pe_id, agent, "infrastructure")
    assert workload_n is not None and workload_n >= 1, "the agent's own bad image must be counted as its own fault"
    assert infra_n == 0, "nothing the agent did may be charged to the environment"

    class_total, reason_total, unclassified = api.eviction_class_coverage(pe_id, agent)
    assert class_total == reason_total and unclassified == 0
