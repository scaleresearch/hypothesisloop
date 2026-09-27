"""The resource-disbalance evictor: a running job whose CPU request is far above the share its
accelerators get on its node strands that node's other accelerators. They are free, nothing can
reach them, and extra hardware does not help because the blockage is a ratio. Preemption ranks
victims by tier and progress, so this is the one pass that terminates a running job the queue
never asked to stop, which is why the scenario also checks that the eviction explains itself.

Admission refuses an explicit request above the per-accelerator share (exceeds_accelerator_share,
see test_max_resource_sentinel.py), so a job only becomes disbalanced when its node's shape
changes after it was admitted. The scenario does exactly that: the node advertises one accelerator
while the hog is admitted, then all of them, the way a node does whose devices come up late. The
admitted hog is then above resource_disbalance_tolerance times its share and the pass evicts it.

Cluster-exclusive: it changes a node's inventory and takes most of its CPU. Needs exactly one node
carrying TEST_ACCELERATOR_TYPE (with a second one the share is the minimum across both and the
blocked job could land elsewhere) and enough accelerators on it to exceed the tolerance.
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCELERATOR_TYPE, make_agent
from support.cluster import disbalance_node_shape, set_node_accelerators
from support.wait import eventually

pytestmark = pytest.mark.exclusive("cluster")

# controlplane/settings/hypothesisloop.yaml: scheduler.resource_disbalance_tolerance.
TOLERANCE = 3.0
# The hog takes this fraction of the node's CPU for one accelerator, so it is HOG_CPU_FRACTION * N
# times its share once all N accelerators are back.
HOG_CPU_FRACTION = 0.45
# Above what the hog leaves free, below the share of the N-1 accelerators the blocked job asks for.
BLOCKED_CPU_FRACTION = 0.70


def test_resource_disbalance_evicts_and_explains_itself(api, run_id, deadline):
    shape = disbalance_node_shape(TEST_ACCELERATOR_TYPE)
    if shape.flavor_nodes != 1:
        pytest.skip(f"{shape.flavor_nodes} nodes carry {TEST_ACCELERATOR_TYPE}; the scenario needs exactly one")
    if HOG_CPU_FRACTION * shape.installed <= TOLERANCE:
        pytest.skip(
            f"node {shape.node} carries {shape.installed} accelerator(s); too few for a hog to "
            f"exceed {TOLERANCE}x its share"
        )

    free_milli = int(shape.free_cpu_cores * 1000)
    hog_cpu = f"{int(free_milli * HOG_CPU_FRACTION)}m"
    blocked_cpu = f"{int(free_milli * BLOCKED_CPU_FRACTION)}m"

    agent = make_agent(api, run_id, "agent-disbalance")
    pe_id = api.create_platform_experiment(f"disbalance-{run_id}", 20.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.declare_baseline_and_wait_running(pe_id)

    set_node_accelerators(shape.node, TEST_ACCELERATOR_TYPE, 1)
    try:
        hog = api.submit_job(
            pe_id, agent, hours=0.1,
            job_overrides={"cpu": hog_cpu, "accelerator_count": 1, "acceptable_accelerator_types": []},
        )
        # Refused as above its share until the scheduler reads the one-accelerator node.
        eventually(
            f"{hog} (hog) to reach RUNNING on a node advertising one accelerator",
            lambda: api.experiment(hog),
            accept=lambda e: e["status"] == "RUNNING",
            reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
            deadline=deadline,
        )
    finally:
        set_node_accelerators(shape.node, TEST_ACCELERATOR_TYPE, shape.installed)

    # Wants every other accelerator on the node and is short only on the CPU the hog holds.
    blocked = api.submit_job(
        pe_id, agent, hours=0.05,
        job_overrides={
            "cpu": blocked_cpu,
            "accelerator_count": shape.installed - 1,
            "acceptable_accelerator_types": [],
        },
    )

    hog_final = eventually(
        f"{hog} (hog) to be evicted while stranding idle accelerators",
        lambda: api.experiment(hog),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    reason = hog_final.get("eviction_reason") or ""
    assert hog_final["status"] == "EVICTED" and reason.startswith("resource_disbalance:"), (
        f"hog ended as {hog_final['status']!r} with eviction_reason={reason!r}, "
        "expected EVICTED for resource_disbalance with an explanation"
    )
    for phrase in ("accelerator", "share", "stranding", shape.node):
        assert phrase in reason, f"explanation is missing {phrase!r}: {reason}"

    blocked_final = eventually(
        f"{blocked} to be admitted once the disproportionate job was gone",
        lambda: api.experiment(blocked),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert blocked_final["status"] in ("RUNNING", "COMPLETED")

    api.cancel_job(blocked)
    api.close_platform_experiment(pe_id)
