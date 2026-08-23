"""Job groups, ported 1:1 from tests/scenarios/distributed-jobs.sh Part 4: a heterogeneous
grouped job -- one `trainer` replica holding 4 accelerators alongside three smaller
single-accelerator `worker` replicas: 4 nodes, 7 accelerators. That shape is the whole argument
for the feature -- submitted as an ungrouped num_nodes=4 job it would have to take the trainer's
shape on every node and pay for 16 accelerators to use 7, and split into two jobs it would be two
experiments that never meet at a rendezvous.

Groups are a second way to express a gang: everything the gang-scheduling tests prove about a
gang has to hold when the gang's nodes are not identical, and the way to show that is the same
reduced value out of the same workload. API + kubectl checks; parallel-safe.
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.cluster import job_pod_count
from support.wait import eventually

pytestmark = [pytest.mark.parallel, pytest.mark.accelerator]

ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
H100_ACCH_RATE = 1.0
DIST_METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "world_reduced_sum", "direction": "maximize"},
    {"key": "accelerators_per_node", "direction": "maximize"},
    {"key": "gang_attempt", "direction": "maximize"},
]

# 30s: the settled-cost band below is measured against wall clock, and the two figures it has to
# separate (7 accelerators vs. 16) only stay apart once the run is long enough that a few seconds
# of measurement slack cannot close the gap.
GROUP_RUN_SECONDS = 30
TRAINER_ACCELERATORS = 4
WORKER_ACCELERATORS = 1
WORKER_REPLICAS = 3
GROUP_TOTAL_ACCELERATORS = TRAINER_ACCELERATORS + WORKER_ACCELERATORS * WORKER_REPLICAS  # 7
GROUP_NODES = 1 + WORKER_REPLICAS  # 4


def groups_cmd(max_retries: int) -> dict:
    # 4 nodes and only two hosts advertise H100, so the multi-node default of hard host spreading
    # cannot be satisfied -- the same non-weakening reasoning as gang_cmd(colocated=True) in the
    # gang-scheduling tests: hard spreading is asserted elsewhere, and what this needs is four
    # ranks of two different shapes that must find each other.
    #
    # The per-node fields job.yaml carries are deleted (None -> merge/delete in job_overrides)
    # because domain.JobSpec.ValidateGroups rejects a spec that states a node's shape both at the
    # top level and per group -- one way to say a thing.
    return {
        "command": ["python", "train_distributed.py"],
        "max_retries": max_retries,
        "cpu": None,
        "memory": None,
        "storage": None,
        "accelerator_count": None,
        "topology": {"spread_across_hosts": False},
        "groups": [
            {"name": "trainer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi",
             "accelerator_count": TRAINER_ACCELERATORS},
            {"name": "worker", "replicas": WORKER_REPLICAS, "cpu": "100m", "memory": "64Mi", "storage": "256Mi",
             "accelerator_count": WORKER_ACCELERATORS},
        ],
    }


def gang_pods_gone(job_id: str) -> bool:
    return job_pod_count(job_id) == 0


def test_heterogeneous_group_gang_forms_one_process_group_and_bills_by_shape(api, experiment, run_id, deadline):
    """One process group across both groups, plus cost summed over the shapes (not replicated
    over the largest one)."""
    agent = make_agent(api, run_id, "groups-form")
    pe_id = experiment("distributed-jobs-groups-form", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(GROUP_RUN_SECONDS)},
            **groups_cmd(0),
        },
    )

    # Reserved cost is exact arithmetic on desired state, with no timing in it: 7 accelerators,
    # not 4 nodes x the largest shape. Those two are 0.14 and 0.32 AccH apart, so this assertion
    # alone distinguishes "sums the groups" from "replicates the biggest one".
    exp = api.experiment(job_id)
    group_total = exp.get("accelerator_count")
    assert group_total == GROUP_TOTAL_ACCELERATORS, (
        f"experiment accelerator_count={group_total}, want {GROUP_TOTAL_ACCELERATORS} "
        f"({TRAINER_ACCELERATORS} + {WORKER_REPLICAS}x{WORKER_ACCELERATORS})"
    )
    est_groups = exp.get("estimated_cost_acch")
    expected_groups = round(GROUP_TOTAL_ACCELERATORS * 0.02 * H100_ACCH_RATE, 6)
    worst_groups = round(GROUP_NODES * TRAINER_ACCELERATORS * 0.02 * H100_ACCH_RATE, 6)
    assert abs(float(est_groups or 0) - expected_groups) < 0.01, (
        f"estimated_cost_acch={est_groups}, want ~{expected_groups} -- a grouped job billed as "
        f"{GROUP_NODES}x its largest shape would read ~{worst_groups}"
    )

    exp = eventually(
        f"{job_id} to run",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )

    # Timed from here, with the job already confirmed running, so what is measured is the interval
    # the platform bills for rather than the queueing before it.
    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    group_start = time.monotonic()
    final = eventually(
        f"grouped gang {job_id} to complete",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    group_elapsed = time.monotonic() - group_start
    assert final["status"] == "COMPLETED", (
        f"grouped job ended {final['status']} after {group_elapsed:.0f}s, want COMPLETED "
        f"(eviction_reason={final.get('eviction_reason')})"
    )

    # 4 nodes across 2 groups: the only correct reduced value is 0+1+2+3 = 6. A worker computing
    # its rank group-locally would contribute 0,1,2 and reduce to 3; the trainer and the first
    # worker both claiming rank 0 is what the platform refuses to let happen by not setting RANK on
    # a grouped pod at all.
    reduced_groups = api.metric_max(job_id, "world_reduced_sum")
    expected_reduced = GROUP_NODES * (GROUP_NODES - 1) // 2
    assert reduced_groups is not None, (
        "the grouped gang never reported world_reduced_sum -- it completed without proving one process group formed"
    )
    assert abs(reduced_groups - expected_reduced) < 0.001, (
        f"all_reduce = {reduced_groups}, want {expected_reduced} -- the groups did not form a single "
        "gang, or two ranks collided"
    )
    api.file_finding(job_id, "distributed e2e: a heterogeneous 2-group gang formed one process group.")

    # Settled cost, on observed consumption. The band is deliberately wide, because the only
    # runtime figure this test can see is its own wall clock: at GROUP_TOTAL_ACCELERATORS
    # accelerators the debit is GROUP_TOTAL_ACCELERATORS x T/3600, and the band below spans 5 to
    # 11 accelerators over T +/- 10s. What it still separates is the failure it exists to catch:
    # GROUP_NODES nodes of the trainer's shape would be GROUP_NODES x TRAINER_ACCELERATORS =
    # 16xT/3600, above the upper bound for any T over ~22s -- which is why this job runs
    # GROUP_RUN_SECONDS (30s) rather than a shorter gang run.
    group_lo = round(5 * max(1, group_elapsed - 10) / 3600.0 * H100_ACCH_RATE, 6)
    group_hi = round(11 * (group_elapsed + 10) / 3600.0 * H100_ACCH_RATE, 6)
    group_worst = round(GROUP_NODES * TRAINER_ACCELERATORS * group_elapsed / 3600.0 * H100_ACCH_RATE, 6)
    # used_guaranteed_acch is served from a separate, eventually-consistent metrics store -- a
    # read immediately after COMPLETED can still be mid-flight, so wait for it to clear the band's
    # own floor rather than trusting the terminal status alone.
    quota_after = eventually(
        f"{job_id}'s settled cost to clear {group_lo} AccH",
        lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
        accept=lambda v: (v - quota_before) >= group_lo,
        deadline=deadline,
    )
    group_debit = round(quota_after - quota_before, 6)
    assert group_lo <= group_debit <= group_hi, (
        f"settled {group_debit} AccH over {group_elapsed:.0f}s falls outside {group_lo}..{group_hi} -- "
        f"{GROUP_NODES} nodes of the trainer's shape would be ~{group_worst}"
    )


def test_group_worker_failure_takes_trainer_with_it(api, experiment, run_id, deadline):
    """A gang is one unit whatever its shapes: the trainer is told to run for 600s, so if it
    survives its worker the wall clock says so unmistakably. FAIL_RANK is the job-global rank 3,
    i.e. the last worker -- which only resolves to that pod if the group offset arithmetic is
    right, so this also fails loudly if the trainer or the first worker were the one to die."""
    agent = make_agent(api, run_id, "groups-fail")
    pe_id = experiment("distributed-jobs-groups-fail", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": "600", "FAIL_RANK": "3"},
            **groups_cmd(0),
        },
    )
    exp = eventually(
        f"{job_id} to start",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert exp["status"] in ("RUNNING", "FAILED"), (
        f"the failing grouped gang never reached RUNNING (status={exp['status']}) -- nothing about "
        "cross-group teardown was exercised"
    )

    group_fail_start = time.monotonic()
    final = eventually(
        f"grouped gang {job_id} to terminate after a worker fails",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("FAILED", "EVICTED", "COMPLETED"),
        deadline=deadline,
    )
    group_fail_elapsed = time.monotonic() - group_fail_start
    assert final["status"] == "FAILED", f"grouped gang reached {final['status']} after a worker died, want FAILED"
    assert group_fail_elapsed < 150, (
        f"the grouped gang took {group_fail_elapsed:.0f}s to terminate -- the trainer ran on after "
        "its worker died"
    )

    eventually(
        "the trainer's pod terminates with its workers",
        lambda: job_pod_count(job_id),
        accept=lambda n: n == 0,
        deadline=deadline,
    )
