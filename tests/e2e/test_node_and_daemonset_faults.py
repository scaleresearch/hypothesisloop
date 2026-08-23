"""Four infra faults chained against the same running job, cheapest/least-disruptive first:

  1. The per-node metrics DaemonSet (node-agent) gets redeployed underneath it -- a different pod
     on the same node, so the job must be completely unaffected and stay on its original node, and
     a fresh job submitted right after must still get admitted normally (no duplicate/stuck
     capacity accounting from the restart).
  2. Its desired-spec identity is externally corrupted -- the next stateless pass detects drift
     and recreates the Job from PostgreSQL.
  3. Its Kubernetes Job is deleted externally while PostgreSQL still desires it -- the next
     stateless cluster-agent pass recreates it with a new UID.
  4. Its node then dies outright (cordon + force-delete its pod) -- cluster-agent's desired-state
     reconciliation must self-heal it onto a different node without operator intervention, and
     dashboard metrics must stay available across the gap.

Then: none of that is the agent's fault -- attempt_count/infra_requeue_count must show zero
retries spent, and evictions_by_class must show zero workload-class failures.

Ported 1:1 from tests/scenarios/node-and-daemonset-faults.sh.

CLUSTER_EXCLUSIVE (mutates real node/DaemonSet state) -- must run before
test_connectivity_loss.py per tests/run.sh's LAST_EXCLUSIVE ordering: this scenario kills/
cordons real nodes and restarts a real DaemonSet, and connectivity-loss's own fault injection
needs those left clean. Every path through this test -- pass, fail, or timeout -- uncordons any
node it cordoned in a `finally`, so it never leaves the shared dev cluster broken for a scenario
that runs after it (including its own repeated solo runs).

PROVEN against the live stack: 3x solo green, cluster (`kubectl get nodes`/`pods`) verified
healthy after each run (see the port's report).
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.cluster import (
    H100,
    all_cordoned_nodes,
    corrupt_job_desired_hash,
    delete_job_resource,
    job_node,
    job_recreated_with_new_uid,
    job_rescheduled_off,
    job_uid,
    kill_node_running_job,
    restart_node_agent_daemonset,
    uncordon_node,
)
from support.wait import Deadline, eventually

# order=-2: must run before test_connectivity_loss.py's order=-1 (see that file's docstring) --
# pytest collects alphabetically by default, which would run them in the wrong order otherwise.
pytestmark = [pytest.mark.exclusive("h100"), pytest.mark.order(-2)]

JOB_HOURS = 0.02
PROBE_JOB_HOURS = 0.003
ADMITTED_OR_TERMINAL = ("RUNNING", "COMPLETED", "FAILED", "EVICTED")
TERMINAL = ("FAILED", "EVICTED", "REJECTED")


# 480s was tight enough to flake in the exclusive lane: this test chains four sequential
# fault-recovery steps -- each its own multi-tick reconcile wait plus real pod
# scheduling/admission -- against a single shared deadline (see conftest.py's `deadline`
# fixture), so it has no per-fault budget of its own. Under real cluster load (running after
# several other exclusive-lane tests) faults 1-3 alone can consume enough of that budget to
# starve fault 4's node-death reschedule wait, which is the fault this scenario cares most
# about proving. This test is I/O-bound waiting on the real cluster, not CPU-bound, so a
# larger wall-clock budget costs nothing but wall-clock time.
@pytest.mark.timeout(720)
def test_node_and_daemonset_faults(api, run_id, deadline):
    agent = make_agent(api, run_id, "agent-infra-faults")
    pe_id = api.create_platform_experiment(f"node-daemonset-faults-{run_id}", 1.0, 1)
    # An autonomous agent defaults to the burst_only tier; this scenario submits tier="guaranteed"
    # jobs and needs the reserved quota that tier draws from (see test_concurrent_admission_race.py
    # for the same override).
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    try:
        _run(api, pe_id, agent, deadline)
    finally:
        # Best-effort: whatever node this test cordoned (fault 4) must come back schedulable
        # before any later scenario -- including a repeated solo run of this one -- can rely on
        # a healthy cluster. Uncordoning by scanning current state (rather than only the one
        # variable captured mid-test) also recovers from a failure between cordon and the
        # in-test uncordon.
        for node in all_cordoned_nodes():
            uncordon_node(node)
        api.close_platform_experiment(pe_id)


def _run(api, pe_id: str, agent: str, deadline: Deadline) -> None:
    # H100 has two distinct local worker nodes; pinning this fault target makes genuine
    # rescheduling possible after one node is cordoned instead of relying on an interchangeable
    # single-node type.
    job = api.submit_job(
        pe_id, agent, hours=JOB_HOURS,
        job_overrides={"accelerator_type": H100, "accelerator_count": 1},
    )
    running = eventually(
        f"{job} to reach RUNNING",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert running["status"] == "RUNNING", f"job never reached RUNNING (status={running['status']}) -- cannot exercise infra-fault scenarios"

    # -- Fault 1: node-agent DaemonSet redeploy --------------------------------------------------
    node_before = job_node(job)
    restart_node_agent_daemonset()

    s2 = api.experiment(job)["status"]
    assert s2 in ("RUNNING", "COMPLETED"), f"job status became {s2} after an unrelated DaemonSet restart"
    node_after = job_node(job)
    assert node_after == node_before, f"job's node changed after unrelated DaemonSet restart ({node_before} -> {node_after})"

    post_ds_job = api.submit_job(pe_id, agent, hours=PROBE_JOB_HOURS)
    # QUEUED deliberately excluded -- it's the job's initial state on every submission, so
    # accepting it here would return immediately without ever waiting for admission.
    post_ds_running = eventually(
        f"{post_ds_job} to be admitted right after the DaemonSet redeploy",
        lambda: api.experiment(post_ds_job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert post_ds_running["status"] in ("RUNNING", "COMPLETED"), (
        f"new job failed to get admitted after the DaemonSet redeploy (status={post_ds_running['status']})"
    )
    post_ds_final = eventually(
        f"{post_ds_job} to complete cleanly",
        lambda: api.experiment(post_ds_job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert post_ds_final["status"] == "COMPLETED", f"post-DaemonSet job did not complete cleanly (status={post_ds_final['status']})"
    api.file_finding(post_ds_job)

    # -- Fault 2: actual Job drift from PostgreSQL desired spec ----------------------------------
    assert api.experiment(job)["status"] == "RUNNING", (
        f"job no longer RUNNING before desired-spec drift fault (status={api.experiment(job)['status']})"
    )
    old_uid = job_uid(job)
    assert old_uid, "could not read original Kubernetes Job UID"
    corrupt_job_desired_hash(job)
    eventually(
        "cluster-agent to replace drifted Job",
        lambda: job_recreated_with_new_uid(job, old_uid),
        accept=lambda ok: ok,
        deadline=deadline,
    )

    # -- Fault 3: actual Job deleted while PostgreSQL still desires it ---------------------------
    assert api.experiment(job)["status"] == "RUNNING", (
        f"job no longer RUNNING before external-delete fault (status={api.experiment(job)['status']})"
    )
    old_uid = job_uid(job)
    assert old_uid, "could not read pre-delete Kubernetes Job UID"
    delete_job_resource(job)
    eventually(
        "cluster-agent to recreate desired Job",
        lambda: job_recreated_with_new_uid(job, old_uid),
        accept=lambda ok: ok,
        deadline=deadline,
    )
    assert api.experiment(job)["status"] == "RUNNING", (
        f"desired lifecycle changed unexpectedly after actual Job recreation (status={api.experiment(job)['status']})"
    )

    # -- Fault 4: node death mid-run -> cluster-agent self-heal -----------------------------------
    assert api.experiment(job)["status"] == "RUNNING", (
        f"job no longer RUNNING before node-death fault (status={api.experiment(job)['status']})"
    )
    # The experiment is RUNNING, but that is the control plane's view: the pod behind it can be
    # mid-restart or mid-reschedule at this instant, and this fault needs a live one to kill.
    # Retry briefly for a pod rather than reporting the absence of one as an inability to run.
    node = None
    kill_deadline = time.monotonic() + 60
    while time.monotonic() < kill_deadline:
        node = kill_node_running_job(job)
        if node:
            break
        time.sleep(2)
    assert node, "could not locate job's pod/node to kill"

    eventually(
        f"job rescheduled off {node}",
        lambda: job_rescheduled_off(job, node),
        accept=lambda ok: ok,
        deadline=deadline,
    )

    # One H100 node remains schedulable and the running job consumes one of its eight devices. An
    # 8-device job therefore cannot fit until the cordoned node returns -- proves new admission
    # uses current cluster metrics, not configured/static capacity.
    shrink_job = api.submit_job(
        pe_id, agent, hours=PROBE_JOB_HOURS,
        job_overrides={"accelerator_type": H100, "accelerator_count": 8},
    )
    shrink_status = eventually(
        f"{shrink_job} status while H100 capacity is reduced",
        lambda: api.experiment(shrink_job),
        accept=lambda e: e["status"] in ("QUEUED",) + ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert shrink_status["status"] == "QUEUED", (
        f"8-device job became {shrink_status['status']} while only seven H100 devices were free"
    )
    # The reason is a code optionally followed by ": <detail>" -- match the code, not the whole
    # string.
    reason = shrink_status.get("not_admitted_reason") or ""
    assert reason.startswith("capacity_unavailable"), (
        f"reduced-capacity job has wrong not_admitted_reason={reason!r}"
    )

    uncordon_node(node)
    shrink_status = eventually(
        f"{shrink_job} to be admitted once H100 capacity returned",
        lambda: api.experiment(shrink_job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert shrink_status["status"] in ("RUNNING", "COMPLETED"), (
        f"job did not admit after H100 node returned (status={shrink_status['status']})"
    )
    assert not shrink_status.get("not_admitted_reason"), (
        f"admitted job retained stale not_admitted_reason={shrink_status.get('not_admitted_reason')!r}"
    )

    final = eventually(
        f"{job} to complete after reschedule",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"job did not complete cleanly after reschedule (status={final['status']})"

    metrics = api.metrics(job)
    assert metrics, "no metrics returned for completed workload -- dashboard metrics must stay available across the reschedule gap"

    shrink_final = eventually(
        f"{shrink_job} to complete cleanly",
        lambda: api.experiment(shrink_job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert shrink_final["status"] == "COMPLETED", f"capacity-growth probe did not complete cleanly (status={shrink_final['status']})"
    api.file_finding(shrink_job)

    # -- Fault attribution: a dead node is not the agent's fault ---------------------------------
    # The job above survived a DaemonSet redeploy, two external mutations of its workload and the
    # outright death of its node. None of that was the agent's doing, and its record must not
    # read as if it were.
    #
    # Deliberately NOT asserted here: the free requeue itself and the refund at its ceiling. Both
    # need the control plane to actually raise an infrastructure-class eviction, and the two this
    # scenario's faults can produce (workload_gone, cluster_unreachable) are only concluded after
    # min_silence_window_seconds (300) of silence -- longer than this scenario's whole ceiling.
    # What is observable here is the accounting that makes the requeue free, and the class
    # breakdown the agent reads.
    job_state = api.experiment(job)
    attempts = job_state.get("attempt_count")
    infra_requeues = job_state.get("infra_requeue_count")
    assert attempts is not None and infra_requeues is not None, (
        f"{job} does not report attempt_count/infra_requeue_count -- the retry budget and the "
        "generation counter are not tracked apart"
    )
    assert attempts - infra_requeues == 0, (
        f"{job} used {attempts - infra_requeues} of the agent's retry allowance "
        f"(attempt_count={attempts} infra_requeue_count={infra_requeues}) -- a node death was charged to the agent"
    )

    # Every job this agent submitted here either completed or is still running: not one of them
    # failed by its own fault, so its workload-class count must be a computed zero.
    workload_n = api.eviction_class_count(pe_id, agent, "workload")
    assert workload_n == 0, (
        f"evictions_by_class.workload is {workload_n!r}, expected a computed 0 -- node and "
        "DaemonSet faults are the environment's, and a missing field is not a zero"
    )

    class_total, reason_total, unclassified = api.eviction_class_coverage(pe_id, agent)
    assert class_total == reason_total and unclassified == 0, (
        f"class breakdown does not account for the evictions: by_class={class_total} "
        f"by_reason={reason_total} unclassified={unclassified}"
    )
