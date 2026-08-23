"""acceptable_accelerator_types: a job must get its concrete flavor selected at admission time
from accelerator_type + acceptable_accelerator_types, not just at k8s node-affinity time. If the
requested flavor is saturated but a listed alternative is free, the job must admit onto the
alternative instead of sitting QUEUED next to idle accelerators.

Regression test for findings.md's P1 "acceptable_accelerator_types cannot be scheduled
correctly": scheduler capacity selection used to use only Experiment.AcceleratorType (the
requested flavor) -- see resolveClusterAndFootprint in loop_tick.go for the fix.

Ported 1:1 from tests/scenarios/acceptable-accelerator-types.sh. Cluster-exclusive because the
assertion deliberately consumes one live accelerator pool (every L40 device) -- run with -n 0,
never alongside another exclusive test or the parallel suite.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.cluster import A100, L40, node_allocatable_gpu_total
from support.wait import eventually

pytestmark = [pytest.mark.exclusive("l40", "a100")]

REQUESTED = L40
ALTERNATIVE = A100


def test_acceptable_accelerator_types_admits_onto_free_alternative(api, run_id, deadline):
    requested_capacity = node_allocatable_gpu_total(REQUESTED)
    alternative_capacity = node_allocatable_gpu_total(ALTERNATIVE)
    if requested_capacity <= 0 or alternative_capacity <= 0:
        pytest.skip("acceptable accelerator scenario requires live L40 and A100 native node inventory")

    # The catalog an agent reads before submitting must agree with live node inventory, under the
    # very strings a job spec takes. L40 and A100 both advertise nvidia.com/gpu, so a catalog that
    # keyed on the extended resource alone would report one merged pool matching no submittable
    # accelerator_type -- checked here since it costs no job and no extra wall time.
    catalog = api.capacity()
    for want_type, want_total in [(REQUESTED, requested_capacity), (ALTERNATIVE, alternative_capacity)]:
        got_total = sum(
            a["total"] for c in catalog["clusters"] for a in c["accelerators"] if a["accelerator_type"] == want_type
        )
        assert got_total == want_total, (
            f"catalog reports {want_type} total={got_total}; live node inventory has {want_total}"
        )

    flex_agent = make_agent(api, run_id, "agt-flex")
    fillers = [make_agent(api, run_id, f"agt-fill-{i}") for i in range(requested_capacity)]
    agents = [flex_agent, *fillers]
    pe_id = api.create_platform_experiment(f"acceptable-accelerator-types-{run_id}", 50.0, len(agents))
    api.signup_and_start(pe_id, agents)

    # Unknown accelerator types must fail at the API boundary.
    code, _ = api.submit_job_expect(
        pe_id, flex_agent, hours=0.01,
        job_overrides={"accelerator_count": 1, "accelerator_type": "nvidia.com/gpu.product=not-in-catalog"},
    )
    assert 400 <= code < 500, f"unknown requested accelerator type returned HTTP {code}; expected a client error"

    code, _ = api.submit_job_expect(
        pe_id, flex_agent, hours=0.01,
        job_overrides={
            "accelerator_count": 1,
            "accelerator_type": ALTERNATIVE,
            "acceptable_accelerator_types": ["nvidia.com/gpu.product=not-in-catalog"],
        },
    )
    assert 400 <= code < 500, f"unknown acceptable accelerator type returned HTTP {code}; expected a client error"

    # Saturate every observed REQUESTED device.
    filler_jobs = [
        api.submit_job(pe_id, a, hours=0.05, job_overrides={"accelerator_type": REQUESTED, "accelerator_count": 1})
        for a in fillers
    ]

    def fillers_saturated():
        return [api.experiment(j)["status"] for j in filler_jobs]

    eventually(
        f"all {REQUESTED} capacity to be occupied",
        fillers_saturated,
        accept=lambda statuses: all(s in ("RUNNING", "COMPLETED") for s in statuses),
        deadline=deadline,
    )

    # A job requesting REQUESTED with acceptable_accelerator_types=[ALTERNATIVE] must admit onto
    # the alternative instead of queueing next to saturated capacity.
    code, flex_job = api.submit_job_expect(
        pe_id, flex_agent, hours=0.02,
        job_overrides={
            "accelerator_count": 1,
            "accelerator_type": REQUESTED,
            "acceptable_accelerator_types": [ALTERNATIVE],
        },
    )
    assert code < 400, f"submission with an acceptable alternative was rejected outright (HTTP {code})"

    exp = eventually(
        f"{flex_job} to leave QUEUED",
        lambda: api.experiment(flex_job),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    status = exp["status"]
    assert status in ("RUNNING", "COMPLETED"), (
        f"job stayed QUEUED (status={status}) with idle {ALTERNATIVE} capacity available -- "
        "admission only considered the requested flavor, not acceptable_accelerator_types"
    )
    admitted_type = exp.get("accelerator_type")
    assert admitted_type == ALTERNATIVE, (
        f"job reached status={status} but admitted accelerator_type={admitted_type} "
        f"(expected {ALTERNATIVE} -- requested {REQUESTED} was saturated)"
    )

    final = eventually(
        f"{flex_job} to complete",
        lambda: api.experiment(flex_job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED", "QUEUED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", "flexible accelerator job did not complete cleanly"
    api.file_finding(flex_job)

    for j in filler_jobs:
        api.cancel_job(j)
    api.close_platform_experiment(pe_id)
