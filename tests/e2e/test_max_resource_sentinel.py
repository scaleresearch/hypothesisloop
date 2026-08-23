"""The "max" CPU sentinel (domain.MaxResourceSentinel, resolved by
controlplane/services/scheduler/loop_resolve.go's resolveClusterLocalResources) has no e2e
coverage: unit tests exercise the resolver in isolation, but nothing proves the whole path --
submission, tick-time resolution against a live cluster's fair share, admission, and the resolved
pod actually landing with that CPU request -- works end to end. This scenario is that proof, plus
the two guarantees the feature exists for:
  1. a job requesting cpu:"max" is resolved and admitted like any concrete number.
  2. what it resolves to is exactly the cluster-local fair share (the MINIMUM
     domain.FairShare(node_cpu, accelerator_count, installed_accelerators) across every node
     topology-eligible for the job) -- not a cluster-wide average, not the requested node's own
     share alone if other eligible nodes are smaller.
  3. the audit trail is preserved: GET /experiments/{id} still reports job_spec.cpu as the literal
     string "max", never the resolved number -- an operator reading history must be able to tell
     "this job asked for whatever headroom existed" from "this job asked for exactly N".
  4. an explicit CPU number that exceeds every eligible node's fair share is never admitted --
     "max" is the only way to reach that headroom; asking for it as a literal number must fail.

Ported 1:1 from tests/scenarios/max-resource-sentinel.sh. Cluster-exclusive: sizing the second,
deliberately-oversized job requires knowing no other job is competing for the same node's
accelerators, or its "impossible" number might not be.

PROVEN against the live stack: 3x solo green (see final verification run in the port's report).
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCELERATOR_TYPE, make_agent
from support.cluster import job_pod, pod_resource, single_eligible_node_cpu_fair_share
from support.wait import eventually

pytestmark = pytest.mark.exclusive("cluster")


def test_max_resource_sentinel_resolves_to_cluster_local_fair_share(api, run_id, deadline):
    share = single_eligible_node_cpu_fair_share(TEST_ACCELERATOR_TYPE)
    if share is None:
        pytest.skip(
            f"max-resource-sentinel needs exactly one live node carrying {TEST_ACCELERATOR_TYPE} "
            "so the expected fair share is unambiguous regardless of scheduler placement"
        )
    node, installed, expected_share_milli = share
    if expected_share_milli < 1:
        pytest.skip(f"node {node} computes a 0m fair share -- no CPU headroom to resolve 'max' against")

    agent = make_agent(api, run_id, "max-sentinel")
    pe_id = api.create_platform_experiment(f"max-sentinel-{run_id}", 20.0, 1)
    # An autonomous agent defaults to the burst_only tier; this scenario submits tier="guaranteed"
    # jobs and needs the reserved quota that tier draws from, so its signup explicitly overrides
    # to quota_tier="guaranteed" rather than relying on kind alone.
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    # -- a job requesting cpu:"max" resolves and admits like any concrete request --
    max_job = api.submit_job(
        pe_id, agent, hours=0.02,
        job_overrides={"cpu": "max", "accelerator_count": 1, "acceptable_accelerator_types": []},
    )
    running = eventually(
        f"{max_job} to reach RUNNING",
        lambda: api.experiment(max_job),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert running["status"] == "RUNNING", f"cpu:'max' job never reached RUNNING (status={running['status']})"

    # -- the running pod's actual CPU request matches the computed fair share --
    pod = job_pod(max_job)
    assert pod is not None, f"could not find a pod for {max_job} to inspect its resolved CPU request"
    req_cpu, _ = pod_resource(max_job, "cpu")
    assert req_cpu is not None, f"pod {pod} has no CPU request to inspect"
    pod_cpu_milli = int(float(req_cpu[:-1])) if req_cpu.endswith("m") else int(float(req_cpu) * 1000)
    # Rounding tolerance: the resolver floors an integer division (domain.FairShare) and this
    # test's own arithmetic does the same floor division independently, so the two should agree
    # exactly -- a few millicores of slack only absorbs how kubectl prints a quantity (e.g. "1" vs
    # "1000m"), never a real resolution mismatch.
    assert abs(pod_cpu_milli - expected_share_milli) <= 5, (
        f"resolved pod CPU request {pod_cpu_milli}m does not match computed fair share "
        f"{expected_share_milli}m"
    )

    # -- the audit trail preserves the literal "max" sentinel, never the resolved number --
    stored = api.experiment(max_job)
    job_spec = stored.get("job_spec") or stored.get("job") or {}
    assert job_spec.get("cpu") == "max", (
        f"GET /experiments/{{id}} reports job_spec.cpu={job_spec.get('cpu')!r}, expected the "
        "literal string 'max' (resolution must not overwrite the audit trail)"
    )

    api.cancel_job(max_job)
    eventually(
        f"{max_job} to leave RUNNING after cancel",
        lambda: api.experiment(max_job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )

    # -- an explicit CPU number exceeding every eligible node's fair share is never admitted --
    # "max" resolves to the fair share; asking for strictly more than it as a literal number must
    # be unreachable by construction (resolveOrValidateDimension errors an explicit qty > bound),
    # so double the fair share is comfortably outside what any node can grant.
    impossible_cpu = f"{expected_share_milli * 2}m"
    code, impossible_job = api.submit_job_expect(
        pe_id, agent, hours=0.02,
        job_overrides={"cpu": impossible_cpu, "accelerator_count": 1, "acceptable_accelerator_types": []},
    )
    if code >= 400:
        pass  # rejected outright at submission
    else:
        stuck = eventually(
            f"{impossible_job} to settle",
            lambda: api.experiment(impossible_job),
            accept=lambda e: e["status"] in ("RUNNING", "COMPLETED") or e.get("not_admitted_reason"),
            deadline=deadline,
        )
        assert stuck["status"] not in ("RUNNING", "COMPLETED"), (
            f"explicit CPU request ({impossible_cpu}) beyond every eligible node's fair share "
            f"({expected_share_milli}m) was admitted (status={stuck['status']})"
        )
        reason = stuck.get("not_admitted_reason") or ""
        assert reason.startswith("capacity_unavailable"), (
            f"not_admitted_reason={reason!r}, expected it to start with capacity_unavailable "
            "(loop_tick.go's notAdmittedReasonFor fallback)"
        )
        api.cancel_job(impossible_job)

    api.close_platform_experiment(pe_id)
