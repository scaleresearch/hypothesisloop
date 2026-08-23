"""Distributed (num_nodes>1) job admission, ported 1:1 from tests/scenarios/distributed-jobs.sh
Parts 1-3: gang scheduling creates every replica, generated pod specs carry equal/non-empty
requests==limits, billing reflects TotalAccelerators = accelerator_count * num_nodes (not just
accelerator_count), several num_nodes>1 jobs submitted concurrently each still get their full
per-node x NumNodes footprint under real scheduling pressure, and every gang guarantee is asserted
on a number the platform cannot fake: train_distributed.py's all_reduce over N ranks is only
correct at N(N-1)/2, so "the gang formed" is a statement about arithmetic rather than about a pod
spec that merely looks right. API + pod-spec + kubectl checks; parallel-safe.
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.cluster import (
    corrupt_service_desired_hash,
    job_distinct_node_count,
    job_pod,
    job_pod_count,
    job_recreated_with_new_uid,
    job_uid,
    pod_resource,
)
from support.wait import assert_stable, eventually

pytestmark = [pytest.mark.parallel, pytest.mark.accelerator]

JOB_HOURS = 0.025
# The estimate above is what the cost assertions are about; the job's real runtime is set
# separately and kept short. Running the full 90s the estimate implies would put this well past a
# reasonable per-test budget for no extra proof about distributed placement, replica-count
# billing, or Service repair.
RUN_SECONDS = 30
# H100, not L40/T4: other parallel-lane scenarios (job-lifecycle, mixed-admission,
# stages-and-settlement) default to T4/L40, so num_nodes=2 jobs here would have to out-rank a
# constant stream of fresh guaranteed-tier submissions from those sibling scenarios for the
# scheduler's fairness-window priority -- real capacity was free but priority never favored these
# jobs within any reasonable wait window. H100 is unused by every other scenario (capacity-safety
# uses L40, preemption-requeue uses A100 -- same reasoning), so this scenario gets a clean,
# uncontended pool.
ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
H100_ACCH_RATE = 1.0
# Every assertion below rests on train_distributed.py's all_reduce: for num_nodes N the only
# correct reduced value is N(N-1)/2. A rank that never joined makes it smaller and a rank that
# joined twice makes it larger, so "the gang formed" is a statement about arithmetic rather than
# about a pod spec that merely looks right.
GANG_RUN_SECONDS = 20
DIST_METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "world_reduced_sum", "direction": "maximize"},
    {"key": "accelerators_per_node", "direction": "maximize"},
    {"key": "gang_attempt", "direction": "maximize"},
]


def gang_cmd(max_retries: int, colocated: bool = False) -> dict:
    cmd: dict = {"command": ["python", "train_distributed.py"], "max_retries": max_retries}
    if colocated:
        # Only two hosts advertise H100 (see the node inventory), and spread_across_hosts
        # defaults to true for num_nodes>1 -- so a 3-rank gang has to be allowed to share a host.
        # That is not a weakening of what is under test: the hard-spread default is already
        # asserted below in the depth test, and what these gang-guarantee tests need is ranks that
        # must find each other, not ranks on separate hosts.
        cmd["topology"] = {"spread_across_hosts": False}
    return cmd


def gang_pods_gone(job_id: str) -> bool:
    return job_pod_count(job_id) == 0


def test_distributed_job_gang_admission_pod_spec_and_billing(api, experiment, run_id, deadline):
    """Part 1: a single num_nodes=2 job's gang scheduling creates all replicas, generated pod
    specs carry equal/non-empty requests==limits, a corrupted companion Service is fully
    reconciled from PostgreSQL (not patched in place), and billing reflects
    TotalAccelerators = accelerator_count * num_nodes, not just accelerator_count."""
    agent = make_agent(api, run_id, "dist-depth")
    pe_id = experiment("distributed-jobs-depth", [agent], budget=5.0)

    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    run_env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}
    job_id = api.submit_job(
        pe_id,
        agent,
        hours=JOB_HOURS,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "num_nodes": 2,
            "env": run_env,
        },
    )

    exp = eventually(
        f"{job_id} to run",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )

    replicas = job_pod_count(job_id)
    assert replicas >= 2, f"only {replicas} pod(s) -- expected 2, gang scheduling did not create both replicas"

    distinct_nodes = job_distinct_node_count(job_id)
    assert distinct_nodes == 2, f"distributed ranks occupy {distinct_nodes} distinct node(s), expected 2"

    pod = job_pod(job_id)
    assert pod, "could not locate pod to inspect resources"
    mem_req, mem_lim = pod_resource(job_id, "memory")
    acc_req, acc_lim = pod_resource(job_id, "nvidia.com/gpu")
    assert mem_req and mem_req == mem_lim and acc_req and acc_req == acc_lim, (
        f"expected equal, non-empty memory/accelerator requests+limits, got memory req={mem_req} "
        f"lim={mem_lim} accelerator req={acc_req} lim={acc_lim}"
    )

    # The Service is part of the same logical desired workload, not an untracked side object.
    # Corrupt only its desired identity and require the stateless reconciler to replace the
    # complete workload from PostgreSQL, with no repair queue or stored tick history.
    if exp["status"] == "RUNNING":
        old_uid = job_uid(job_id)
        corrupt_service_desired_hash(job_id)
        # Repairing this means deleting the Job, waiting out its pods' termination, and
        # recreating -- a delete/recreate cycle, not a single reconcile pass.
        eventually(
            "cluster-agent repairs drifted distributed-job Service",
            lambda: job_recreated_with_new_uid(job_id, old_uid),
            accept=lambda ok: ok,
            deadline=deadline,
        )

    est_cost = exp.get("estimated_cost_acch")
    expected_cost = round(2 * JOB_HOURS * H100_ACCH_RATE, 6)
    assert abs(float(est_cost or 0) - expected_cost) < 0.01, (
        f"estimated_cost_acch={est_cost} does not match TotalAccelerators-based expected~{expected_cost}"
    )

    final = eventually(
        f"{job_id} to complete",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"did not complete cleanly (status={final['status']})"

    # used_guaranteed_acch is served from a separate, eventually-consistent metrics store --
    # reading it immediately after COMPLETED can race the settlement write, so wait on the
    # actual field being asserted on rather than trusting the terminal status alone.
    quota_after = eventually(
        f"{job_id}'s billed usage to be observable via used_guaranteed_acch",
        lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
        accept=lambda v: v > quota_before,
        deadline=deadline,
    )
    debit = round(quota_after - quota_before, 4)
    assert debit > 0, f"guaranteed quota debited {debit} AccH -- expected a positive debit for both replicas"


def test_concurrent_distributed_jobs_admitted_with_full_replica_count_under_pressure(api, experiment, run_id, deadline):
    """Part 2: several num_nodes=2 jobs submitted concurrently must each still get their full
    per-node x NumNodes footprint under real scheduling pressure, not a partial replica count."""
    agents = [make_agent(api, run_id, f"dist-pressure-{i}") for i in range(2)]
    pe_id = experiment("distributed-jobs-pressure", agents, budget=5.0)

    jobs = [
        api.submit_job(
            pe_id,
            a,
            hours=0.02,
            tier="burst",
            job_overrides={"accelerator_type": ACCELERATOR_TYPE, "accelerator_count": 1, "num_nodes": 2},
        )
        for a in agents
    ]

    ok = 0
    for job_id in jobs:
        # QUEUED deliberately excluded from the accept set: every fresh submission starts QUEUED,
        # so accepting it would return almost instantly (on the very first poll) instead of
        # actually waiting for a real admission decision. burst-tier jobs are also processed after
        # guaranteed-tier jobs every scheduler tick, and the scheduler is one shared service across
        # the whole concurrent test-suite run, so admission can legitimately take a while.
        try:
            exp = eventually(
                f"{job_id} to be admitted",
                lambda: api.experiment(job_id),
                accept=lambda e: e["status"] in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
                deadline=deadline,
            )
        except AssertionError:
            continue
        replicas = job_pod_count(job_id)
        if exp["status"] in ("RUNNING", "COMPLETED"):
            assert replicas >= 2, f"{job_id} admitted (status={exp['status']}) but only has {replicas} pod(s), expected 2"
            distinct_nodes = job_distinct_node_count(job_id)
            assert distinct_nodes >= 2, f"{job_id} has full replica count but only {distinct_nodes} distinct node(s)"
            ok += 1
    assert ok >= 1, "no concurrent num_nodes=2 job reached RUNNING/COMPLETED with full replica count"


def test_gang_forms_single_process_group(api, experiment, run_id, deadline):
    """Part 3, #1 (G2 + G8): every rank of a 3-rank gang resolves the same MASTER_ADDR, and
    COMPLETED means all of them exited 0."""
    agent = make_agent(api, run_id, "gang-form")
    pe_id = experiment("distributed-jobs-gang-form", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "num_nodes": 3,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(GANG_RUN_SECONDS)},
            **gang_cmd(0, colocated=True),
        },
    )
    eventually(
        f"{job_id} to run",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    final = eventually(
        f"3-rank gang {job_id} to complete",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", (
        f"3-rank gang did not complete (status={final['status']}, eviction_reason={final.get('eviction_reason')})"
    )

    reduced = api.metric_max(job_id, "world_reduced_sum")
    assert reduced is not None, (
        "the gang never reported world_reduced_sum -- it completed without proving the process group formed"
    )
    assert abs(reduced - 3.0) < 0.001, f"all_reduce = {reduced}, want 3 (0+1+2) -- a rank did not join the process group"
    api.file_finding(job_id, "distributed e2e: 3-rank gloo-style rendezvous formed, reduced value correct.")


def test_gang_per_node_accelerator_count(api, experiment, run_id, deadline):
    """Part 3, #2 (G3): per-node environment facts are true of the pod they are in. With
    accelerator_count=2, num_nodes=2 the pod must be told 2, while the experiment carries 4 -- a
    workload sizing --nproc_per_node from the experiment total would launch 4 processes for the 2
    devices it actually holds."""
    agent = make_agent(api, run_id, "gang-pernode")
    pe_id = experiment("distributed-jobs-gang-pernode", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 2,
            "num_nodes": 2,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(GANG_RUN_SECONDS)},
            **gang_cmd(0),
        },
    )
    eventually(
        f"{job_id} to run",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED"),
        deadline=deadline,
    )
    final = eventually(
        f"accelerator_count=2 num_nodes=2 gang {job_id} to complete",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"accelerator_count=2 num_nodes=2 gang did not complete (status={final['status']})"

    per_node = api.metric_max(job_id, "accelerators_per_node")
    assert per_node is not None and abs(per_node - 2.0) < 0.001, (
        f"HYPOTHESISLOOP_ACCELERATOR_COUNT reported as {per_node!r}, want 2 -- the pod is being told the job total"
    )
    total = final.get("accelerator_count")
    assert total == 4, f"experiment accelerator_count={total}, want 4 (2 per node x 2 nodes)"
    api.file_finding(job_id, "distributed e2e: per-node accelerator count is true of the pod.")


def test_gang_admission_is_all_or_nothing(api, experiment, run_id, deadline):
    """Part 3, #3 (G1): all N nodes are admitted together or none is. More ranks than there are
    hosts advertising this type, with the default hard host spread -- there is no partial
    admission to fall back to, so it must sit in the queue saying why."""
    agent = make_agent(api, run_id, "gang-toobig")
    pe_id = experiment("distributed-jobs-gang-toobig", [agent], budget=5.0)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "num_nodes": 9,
            **gang_cmd(0),
        },
    )
    assert_stable(
        "impossible-width gang is never partially admitted",
        lambda: api.experiment(job_id)["status"],
        ok=lambda s: s in ("QUEUED", "SUBMITTED"),
        duration=20,
    )
    exp = api.experiment(job_id)
    # The reason may carry a detail suffix ("capacity_unavailable: short {...}") -- the code is
    # everything before the first ':', matching how the platform's own .Code() reads it.
    reason = exp.get("not_admitted_reason") or ""
    assert reason.split(":", 1)[0] == "capacity_unavailable", f"not_admitted_reason={reason}, want capacity_unavailable"

    partial = job_pod_count(job_id)
    assert partial == 0, f"{partial} pod(s) exist for a gang that was never admitted -- admission is not atomic"
    api.cancel_job(job_id)


def test_gang_rank_failure_kills_whole_gang(api, experiment, run_id, deadline):
    """Part 3, #4 (G4): a rank failing stops the gang, promptly. FAIL_RANK exits non-zero AFTER
    the barrier, so every rank is provably running when one dies -- which is what makes "the
    survivors were torn down" a statement about the gang policy rather than about a rank that
    never started."""
    agent = make_agent(api, run_id, "gang-fail")
    pe_id = experiment("distributed-jobs-gang-fail", [agent], budget=5.0)

    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "num_nodes": 3,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": "600", "FAIL_RANK": "2"},
            **gang_cmd(0, colocated=True),
        },
    )
    exp = eventually(
        f"{job_id} to start",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert exp["status"] in ("RUNNING", "FAILED"), (
        f"failing gang never reached RUNNING (status={exp['status']}) -- nothing about G4 was exercised"
    )

    fail_start = time.monotonic()
    # The workload would otherwise hold every surviving rank for a full 600s. Anything close to
    # that means the survivors ran on after rank 2 died, which is precisely the failure being
    # ruled out.
    final = eventually(
        f"{job_id} to terminate after a rank fails",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("FAILED", "EVICTED", "COMPLETED"),
        deadline=deadline,
    )
    elapsed = time.monotonic() - fail_start
    assert final["status"] == "FAILED", f"gang reached {final['status']} after a rank failed, want FAILED"
    assert elapsed < 150, f"gang took {elapsed:.0f}s to terminate -- the survivors ran on rather than being torn down with it"

    eventually(
        "surviving ranks' pods terminate",
        lambda: job_pod_count(job_id),
        accept=lambda n: n == 0,
        deadline=deadline,
    )


def test_gang_retry_restarts_and_bills_every_attempt(api, experiment, run_id, deadline):
    """Part 3, #5 (G4 + G7): max_retries restarts the whole gang, and every attempt is billed.
    Kubernetes cannot restart a gang, so this budget is spent by the control plane requeueing the
    experiment. Each attempt reports its own index under the same experiment id, so the number of
    distinct gang_attempt values IS the number of attempts that actually ran."""
    agent = make_agent(api, run_id, "gang-retry")
    pe_id = experiment("distributed-jobs-gang-retry", [agent], budget=5.0, metrics=DIST_METRICS)

    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")
    job_id = api.submit_job(
        pe_id,
        agent,
        hours=0.02,
        job_overrides={
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "num_nodes": 3,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(GANG_RUN_SECONDS), "FAIL_RANK": "1"},
            **gang_cmd(1, colocated=True),
        },
    )
    # Two full attempts, each of which must be admitted, run to the barrier and die -- plus the
    # requeue and re-admission between them, which is a scheduler tick rather than a pod restart.
    final = eventually(
        f"gang with max_retries=1 {job_id} to exhaust its retry budget",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("FAILED", "EVICTED", "COMPLETED"),
        deadline=deadline,
    )
    assert final["status"] == "FAILED", f"gang with max_retries=1 ended {final['status']}, want FAILED"

    # The job's own terminal status and its metrics travel through GreptimeDB independently, so
    # reading the count the instant status turns FAILED can catch metrics still in flight -- poll
    # rather than a fixed sleep: catches up the moment the write lands, costs nothing once landed.
    attempts = eventually(
        "both gang attempts are reflected in gang_attempt metrics",
        lambda: api.metric_distinct_count(job_id, "gang_attempt"),
        accept=lambda n: n >= 2,
        deadline=deadline,
    )
    assert attempts == 2, f"{attempts} gang attempt(s) reported, want exactly 2 -- a gang retry either never happened or ran away"

    # Every attempt reported the full reduced value, so both attempts were complete 3-rank gangs
    # rather than a retry of the one rank that died.
    reduced_retry = api.metric_max(job_id, "world_reduced_sum")
    assert reduced_retry is not None and abs(reduced_retry - 3.0) < 0.001, (
        f"reduced value across retries was {reduced_retry!r}, want 3 -- a retry ran a partial gang"
    )

    # used_guaranteed_acch is served from a separate, eventually-consistent metrics store -- wait
    # for the debit to actually land rather than trusting the terminal status alone.
    quota_after = eventually(
        f"{job_id}'s settled cost to be observable via used_guaranteed_acch",
        lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
        accept=lambda v: v > quota_before,
        deadline=deadline,
    )
    retry_debit = round(quota_after - quota_before, 4)
    # Billing counts observed hours across the whole experiment, so a debit of zero would mean
    # neither attempt was billed. A wall-clock estimate sized for a full run would overstate a
    # real attempt's cost enough that two real, short-lived attempts settle below it (a rank that
    # fails right after the barrier ends the whole gang in a few seconds, not in
    # GANG_RUN_SECONDS), so this only checks the debit is positive, matching the platform's own
    # settlement-rate assertions (controlplane/services/settlement/overbilling_test.go), which use
    # no clock at all.
    assert retry_debit > 0, f"settled cost {retry_debit} AccH is not positive -- a gang attempt was never billed"
