"""Cluster loses connectivity to the control plane (cluster-agent stops reporting). Four phases
on one running stack, sharing the disconnect/verify setup:

  1. restore: reconnects after a short outage -- a new submission that failed closed while
     disconnected must get admitted once capacity reporting resumes. An already-RUNNING job must
     be completely undisturbed by the outage (job pods don't depend on cluster-agent's liveness).
  2. permanent: a second outage that is never restored before assertions run -- a job submitted
     during this outage must sit durably QUEUED for as long as it lasts, and an already-RUNNING
     job keeps running independently (its workload metrics/node heartbeats are direct liveness
     signals and do not stop merely because cluster-agent's own capacity reporting is down).
  3. desired deletion converges after reconciliation was unavailable -- cancelling a job while
     cluster-agent is disconnected must persist EVICTED in PostgreSQL immediately (the control
     plane decides; commands go one way only, per important.md), while the actual Kubernetes Job
     stays put until cluster-agent reconnects and reconciles it away.
  4. fault attribution -- none of the above is the agent's fault: attempt_count/infra_requeue_count
     must show zero retries spent across every job that lived through an outage, and
     evictions_by_class must show a policy (not workload) eviction for the cancelled job.

Ported 1:1 from tests/scenarios/connectivity-loss.sh. Disconnection is simulated by scaling the
cluster-agent Deployment to 0 replicas (tests/support/cluster.py::disconnect_cluster_agent) -- the
cleanest available proxy for "this cluster lost network reachability" on a single-cluster local
dev setup with no separate network segment to partition.

CLUSTER_EXCLUSIVE (scales a cluster-wide Deployment to 0, stopping ALL accelerator types'
capacity reporting) -- per tests/run.sh's LAST_EXCLUSIVE ordering this ran last of the
CLUSTER_EXCLUSIVE scenarios in the old suite; test_node_and_daemonset_faults.py's port note says
it must run before this one so its own node/DaemonSet mutations are settled first. Every path
through this test -- pass, fail, or timeout -- scales cluster-agent back to 1 replica and waits
for it to become ready in a `finally`, so it never leaves the shared dev cluster's capacity
reporting down for a scenario that runs after it (including its own repeated solo runs).

PROVEN against the live stack: 3x solo green, cluster (`kubectl get nodes`/`pods`, and
/internal/clusters/ showing the cluster connected again) verified healthy after each run (see the
port's report).
"""
from __future__ import annotations

import time

import pytest

from conftest import make_agent
from support.cluster import (
    cluster_agent_connected,
    cluster_agent_disconnected,
    cluster_agent_name,
    disconnect_cluster_agent,
    job_resource_exists,
    reconnect_cluster_agent,
)
from support.wait import Deadline, assert_stable, eventually

# order=-1: must run after test_node_and_daemonset_faults.py's order=-2 (see that file's
# docstring) -- pytest collects alphabetically by default, which would run them in the wrong
# order otherwise.
pytestmark = [pytest.mark.exclusive("cluster"), pytest.mark.order(-1)]

ADMITTED_OR_TERMINAL = ("RUNNING", "COMPLETED", "FAILED", "EVICTED")
TERMINAL = ("COMPLETED", "FAILED", "EVICTED")


@pytest.mark.timeout(480)
def test_connectivity_loss(api, run_id, deadline):
    cluster_name = cluster_agent_name()
    assert cluster_name, "cluster-agent deployment has no CLUSTER_NAME"

    agent = make_agent(api, run_id, "agent-connloss")
    pe_id = api.create_platform_experiment(f"connectivity-loss-{run_id}", 1.0, 1)
    # An autonomous agent defaults to the burst_only tier; this scenario submits tier="guaranteed"
    # jobs (matching connectivity-loss.sh's submit_job calls) and needs the reserved quota that
    # tier draws from -- see test_node_and_daemonset_faults.py for the same override.
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    try:
        _run(api, pe_id, agent, cluster_name, deadline)
    finally:
        # Safety net mirroring the bash trap: if this test is killed or fails mid-assertion while
        # cluster-agent is scaled to 0, it must not stay down for the rest of the suite -- that
        # would zero out capacity reporting for every scenario after this one.
        reconnect_cluster_agent()
        api.close_platform_experiment(pe_id)


def _run(api, pe_id: str, agent: str, cluster_name: str, deadline: Deadline) -> None:
    # === Phase 1: disconnect, then restore =======================================================
    running_job = api.submit_job(pe_id, agent, hours=0.03)
    running = eventually(
        f"{running_job} to reach RUNNING before disconnect",
        lambda: api.experiment(running_job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert running["status"] == "RUNNING", (
        f"job never reached RUNNING before disconnect (status={running['status']}) -- "
        "cannot exercise connectivity loss"
    )

    disconnect_cluster_agent()
    eventually(
        "cluster-agent reports disconnected",
        cluster_agent_disconnected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(15),
    )

    assert_stable(
        "already-running job undisturbed by connectivity loss "
        "(should not depend on cluster-agent liveness)",
        lambda: api.experiment(running_job)["status"],
        ok=lambda s: s in ("RUNNING", "COMPLETED"),
        duration=10,
    )

    queued_job = api.submit_job(pe_id, agent, hours=0.02)
    # Poll briefly (bash used a 10s wait_for_status budget) rather than the shared test deadline --
    # RUNNING/COMPLETED here would be a fail, so this must not wait out the full deadline to prove
    # the negative. eventually() has no "wait to confirm absence" mode, so just poll a short fixed
    # window and take the last observed status.
    end = time.monotonic() + 10
    s3 = api.experiment(queued_job)["status"]
    while time.monotonic() < end and s3 not in ("RUNNING", "COMPLETED"):
        time.sleep(1)
        s3 = api.experiment(queued_job)["status"]
    assert s3 not in ("RUNNING", "COMPLETED"), (
        f"new job was admitted (status={s3}) while cluster-agent is disconnected -- "
        "capacity should fail closed"
    )

    reconnect_cluster_agent()
    eventually(
        "cluster-agent reports connected",
        cluster_agent_connected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(30),
    )

    queued_final = eventually(
        f"{queued_job} to be admitted once capacity reporting resumed",
        lambda: api.experiment(queued_job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert queued_final["status"] in ("RUNNING", "COMPLETED"), (
        f"queued job never got admitted after reconnect (status={queued_final['status']})"
    )

    # Both jobs are already confirmed RUNNING/admitted above, so this is a completion-only wait.
    for job_id in (running_job, queued_job):
        final = eventually(
            f"{job_id} to reach a terminal state",
            lambda job_id=job_id: api.experiment(job_id),
            accept=lambda e: e["status"] in TERMINAL,
            deadline=deadline,
        )
        if final["status"] == "COMPLETED":
            api.file_finding(job_id)

    # === Phase 2: disconnect again, never restore before asserting ==============================
    # 0.15h (540s) keeps the workload actively posting improving metrics through the full
    # disconnect-detect plus heartbeat expiry, so it can't finish and flatline early and
    # spuriously trip metric-decline before the silence path applies.
    running_job2 = api.submit_job(pe_id, agent, hours=0.15)
    running2 = eventually(
        f"{running_job2} to reach RUNNING before second disconnect",
        lambda: api.experiment(running_job2),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert running2["status"] == "RUNNING", (
        f"job never reached RUNNING before second (permanent) disconnect (status={running2['status']})"
    )

    disconnect_cluster_agent()
    eventually(
        "cluster-agent reports disconnected in permanent-outage phase",
        cluster_agent_disconnected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(15),
    )

    eventually(
        "cluster capacity ages out of live metrics",
        lambda: api.cluster_absent_from_live_metrics(cluster_name),
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(45),
    )

    s2 = api.experiment(running_job2)["status"]
    assert s2 in ("RUNNING", "COMPLETED"), (
        f"already-running job ended as {s2} during a cluster-agent outage"
    )
    if s2 == "COMPLETED":
        api.file_finding(running_job2)

    stuck_job = api.submit_job(pe_id, agent, hours=0.02)
    assert_stable(
        "job submitted after cluster capacity became stale stays durably queued",
        lambda: api.experiment(stuck_job)["status"],
        ok=lambda s: s == "QUEUED",
        duration=15,
    )
    # A cluster that has aged out of live heartbeats must never be treated as a speculative
    # scale-up candidate (GetFlavorCapacity's !connected skip, autoscaler.md pre-work) --
    # not_admitted_reason must not read as "waiting on this dead cluster to scale up".
    stuck_reason = (api.experiment(stuck_job).get("not_admitted_reason") or "").split(":", 1)[0]
    assert stuck_reason not in ("waiting-for-scale-up", "no_scalable_capacity"), (
        f"not_admitted_reason={stuck_reason} implies an unreachable cluster was speculated on"
    )

    reconnect_cluster_agent()
    # Must actually assert this, not just attempt it and move on: a later CLUSTER_EXCLUSIVE
    # scenario needs a healthy cluster-agent from its very first submission -- leaving
    # reconnection unverified here would silently poison that scenario's result instead of
    # attributing the failure to where it belongs.
    eventually(
        "cluster-agent reconnected after the permanent-outage phase",
        cluster_agent_connected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(45),
    )
    # Drain the now-unstuck job so it doesn't linger into phase 3 (best-effort, mirrors the bash
    # `|| true`).
    try:
        eventually(
            f"{stuck_job} to leave QUEUED after reconnect",
            lambda: api.experiment(stuck_job),
            accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
            deadline=Deadline.in_seconds(45),
        )
    except AssertionError:
        pass

    # === Phase 3: desired deletion converges after reconciliation was unavailable ===============
    delete_job = api.submit_job(pe_id, agent, hours=0.08)
    delete_running = eventually(
        f"{delete_job} to reach RUNNING before delete-convergence disconnect",
        lambda: api.experiment(delete_job),
        accept=lambda e: e["status"] in ADMITTED_OR_TERMINAL,
        deadline=deadline,
    )
    assert delete_running["status"] == "RUNNING", (
        f"delete-convergence job did not reach RUNNING (status={delete_running['status']})"
    )

    disconnect_cluster_agent()
    eventually(
        "cluster-agent reports disconnected in delete-convergence phase",
        cluster_agent_disconnected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(15),
    )

    api.cancel_job(delete_job)
    delete_status = api.experiment(delete_job)["status"]
    assert delete_status == "EVICTED", (
        f"cancelled job did not become EVICTED in PostgreSQL (status={delete_status})"
    )
    assert job_resource_exists(delete_job), (
        "actual Job disappeared before cluster-agent reconnected -- deletion was pushed instead "
        "of reconciled"
    )

    reconnect_cluster_agent()
    eventually(
        "cluster-agent reconnected for delete convergence",
        cluster_agent_connected,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(45),
    )
    eventually(
        f"cluster-agent removes no-longer-desired Job {delete_job}",
        lambda: not job_resource_exists(delete_job),
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(30),
    )

    # === Fault attribution: an outage costs the agent nothing ====================================
    # Three jobs here lived through a cluster-agent outage; not one of them was the agent's fault,
    # and its record must say so.
    #
    # Deliberately NOT asserted here: the refund at the ceiling. A refund lands only when a job
    # ENDS evicted on an infrastructure fault (settlement.Settle), and the only reason this outage
    # can raise is cluster_unreachable -- which controller.checkSilence will not conclude until a
    # job has been silent for min_silence_window_seconds (300) with the cluster's last snapshot
    # older than cluster_status_silence_ceiling_seconds (900). Both are far past this scenario's
    # ceiling, so driving one requeue here is not possible at any budget worth spending. That path
    # is pinned by the scheduler and settlement unit tests; what this scenario can prove is that
    # the outage cost the agent no retry budget and left no workload-class mark on its record.
    for job_id in (running_job, running_job2, delete_job):
        state = api.experiment(job_id)
        attempts = state.get("attempt_count")
        infra = state.get("infra_requeue_count")
        assert attempts is not None and infra is not None, (
            f"{job_id} does not report attempt_count/infra_requeue_count -- the retry budget and "
            "the generation counter are not tracked apart"
        )
        assert attempts - infra == 0, (
            f"{job_id} used {attempts - infra} of the agent's retry allowance "
            f"(attempt_count={attempts} infra_requeue_count={infra}) -- an outage it did not "
            "cause was charged to it"
        )

    # The cancelled job in phase 3 is EVICTED with reason `cancelled`, which is FaultPolicy: the
    # platform decided and the job was fine. So this agent must have a policy eviction and no
    # workload one -- nothing it submitted was ever its own fault in this scenario.
    policy_n = api.eviction_class_count(pe_id, agent, "policy")
    workload_n = api.eviction_class_count(pe_id, agent, "workload")
    assert policy_n is not None and policy_n >= 1, (
        f"evictions_by_class.policy is {policy_n!r} -- a cancellation is the platform's own "
        "decision and must be reported as one"
    )
    assert workload_n == 0, (
        f"evictions_by_class.workload is {workload_n!r}, expected 0 -- an outage the agent did "
        "not cause landed on its record"
    )
    # infrastructure is deliberately not asserted to be 0: it is legitimately reachable here if
    # the suite runs long enough for a silence window to elapse, and pinning it would make this
    # scenario fail for being slow rather than for being wrong.

    class_total, reason_total, unclassified = api.eviction_class_coverage(pe_id, agent)
    assert class_total == reason_total and unclassified == 0, (
        f"class breakdown does not account for the evictions: by_class={class_total} "
        f"by_reason={reason_total} unclassified={unclassified}"
    )
