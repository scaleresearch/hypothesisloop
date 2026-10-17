"""What the platform can tell an agent about a job that died. Both halves are about diagnosis
surviving cleanup:
  1. A crashing job's own output must still be readable after its workload is gone (terminal
     status removes a job from desired state; the cluster-agent's next reconcile deletes the
     workload -- pod, logs, container termination reason with it -- so the runtime must capture
     and push that output *before* deleting).
  2. An image no registry knows is refused when it is recorded, before any job could fail to pull
     it, and the jobs keep running in the image recorded before.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.api import ENDED
from support.wait import assert_stable, eventually

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

    # The runtime removes an ended job's workload on its next pass, within seconds.
    assert_stable(
        "the log tail stays readable once the workload is gone",
        lambda: api.logs(crash_job, n=200),
        ok=lambda logs: marker in logs,
        duration=20,
        interval=2,
    )

    exp = api.experiment(crash_job)
    phase_detail = exp.get("phase_detail") or {}
    assert phase_detail.get("reason") == "container_failed"
    assert "container exited with code 42" in (phase_detail.get("message") or "")


def test_an_image_no_registry_knows_is_refused_and_jobs_keep_the_one_recorded(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "failure-diag-image")
    pe_id = experiment("failure-diagnostics-image", [(agent, None, "guaranteed")], budget=10.0, report_interval_seconds=5)
    recorded = api.get_json(f"/platform-experiments/{pe_id}")["image"]

    r = api.put(f"/platform-experiments/{pe_id}/image", {"image": f"docker.io/library/hypothesisloop-no-such-image-{run_id}:nope"})
    assert r.status_code == 422 and r.json()["reason"] == "image_unresolved", f"an unknown image: {r.status_code} {r.text}"
    assert api.get_json(f"/platform-experiments/{pe_id}")["image"] == recorded, "a refused image replaced the recorded one"

    job = api.submit_job(pe_id, agent, hours=0.01, job_overrides={"command": ["true"], "max_retries": 0})
    final = eventually(f"{job} to end", lambda: api.experiment(job), accept=lambda e: e["status"] in ENDED, deadline=deadline)
    assert final["status"] == "COMPLETED", f"{job} ended {final['status']}: {final.get('eviction_reason')}"
    assert final["image"] == recorded["image"], f"{job} ran in {final['image']}, its platform experiment records {recorded['image']}"
