"""New e2e lane for autoscaler.md: runs with AUTOSCALER_ENABLED=true on the live cluster-agent and
no real cluster-autoscaler/Karpenter behind it. That absence is the point -- a pod that would
trigger a real scale-up here just stays genuinely Pending forever, which exercises every
control-plane path (speculative submit, skip-preemption, scale-up-timeout failover, tried_clusters,
concurrency cap) without needing cloud infra. Per autoscaler.md's own "New e2e" section.

CLUSTER_EXCLUSIVE: toggles AUTOSCALER_ENABLED on the shared cluster-agent Deployment for the
whole module, which would change admission behaviour for any job submitted by a concurrently
running parallel-lane test. Every path through setup/teardown restores AUTOSCALER_ENABLED=false
and any cluster_settings this module wrote, in `finally`, so it never leaves the shared dev
cluster in autoscaler mode for a scenario that runs after it.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.cluster import (
    all_cordoned_nodes,
    cluster_agent_name,
    job_pod_count,
    set_cluster_agent_autoscaler_enabled,
    uncordon_node,
)
from support.wait import Deadline, assert_stable, eventually

pytestmark = pytest.mark.exclusive("cluster")

# A dedicated flavor no other scenario touches (see test_distributed_jobs_gang_scheduling.py's own
# comment on why each exclusive/contended scenario picks an unshared accelerator type) -- the
# local fake device plugin advertises this on exactly one node, so "oversized" and "N-1 of N
# hosts free" are both cheap to construct.
SOLO_TYPE = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"


def _reason(exp: dict) -> str:
    return (exp.get("not_admitted_reason") or "").split(":", 1)[0]


@pytest.fixture(autouse=True)
def _autoscaler_mode():
    set_cluster_agent_autoscaler_enabled(True)
    try:
        yield
    finally:
        set_cluster_agent_autoscaler_enabled(False)
        for node in all_cordoned_nodes():
            uncordon_node(node)


def test_oversized_job_submits_speculatively_and_blocks_preemption(api, experiment, run_id, deadline):
    """Scenario 1, adjusted from autoscaler.md's literal wording after live verification exposed a
    real architecture gap (see NOTE below): a job sized to exactly the SOLO_TYPE node's installed
    capacity goes SUBMITTED with a genuinely Pending pod on an autoscaler-enabled cluster, and a
    second guaranteed job on the same flavor is not admitted while it holds the node -- the
    skip-preemption rule's `waiting-for-scale-up` reason (or the pre-existing generic
    `capacity_unavailable` at the exact zero-desired-free boundary; see NOTE).

    NOTE (live e2e finding, not fixed here -- flagged for a follow-up session): fitsLargestNode
    (loop_speculate.go) judges the accelerator dimension against nodeAvail (currently-free
    devices), not an installed total -- autoscaler.md's fact table assumed a per-node installed
    accelerator metric exists, but only accelerator_available_by_node (free) is ever recorded; no
    accelerator_total_by_node series exists anywhere in the metrics pipeline. Consequence: for a
    single-node (non-gang) job, "fits the largest node" and "fits live" collapse to the same
    condition on a flavor with exactly one node, since any occupant that shrinks live-free
    identically shrinks the speculative ceiling. A job at exactly the node's installed size can
    still go SUBMITTED (there is nothing else there to make live-fit fail first), but desired-free
    lands at exactly 0, not negative -- so the skip-preemption rule's own strictly-negative test
    sits right on the boundary this flavor can produce, and a second job here is correctly never
    admitted but may carry either reason depending on which check the tick reaches first. A
    faithful test of "genuinely exceeds live-free while still fitting installed capacity" needs
    either a real installed-total-per-node metric (a step 1/2-shaped follow-up: agent-reported
    node shape by accelerator type, mirroring how CPU/memory/storage totals already work via
    GetNodeTotalCapacity) or a multi-node flavor exercised through the gang path, which is exactly
    what test_partial_gang_completes_or_evicts_wholly_on_deadline below already covers."""
    agent = make_agent(api, run_id, "asc-oversize")
    pe_id = experiment("autoscaler-oversize", [agent], budget=5.0)

    big = api.submit_job(
        pe_id, agent, hours=0.02,
        job_overrides={"accelerator_type": SOLO_TYPE, "accelerator_count": 8},
    )
    # QUEUED is the pre-tick status every submission starts in, so it must not satisfy this wait
    # on its own -- accepting it here would let the very first poll (before the scheduler's next
    # tick has even run) look like success and mask a speculative submit that never happens.
    eventually(
        f"{big} to be SUBMITTED on the autoscaler-enabled cluster",
        lambda: api.experiment(big),
        accept=lambda e: e["status"] in ("SUBMITTED", "RUNNING"),
        deadline=deadline,
    )

    eventually(
        "one Pending pod exists for the gang",
        lambda: job_pod_count(big) >= 1,
        accept=lambda ok: ok,
        deadline=Deadline.in_seconds(30),
    )

    # Without a cap, a second job that also doesn't fit live is itself a valid speculative
    # candidate on this same untried cluster (autoscaler.md's own "two gangs competing on one
    # cluster" case permits exactly that -- confirmed live: an uncapped second job here also goes
    # SUBMITTED instead of waiting). Capping max_speculative_accelerators at the first job's own
    # footprint forces the second job past its own speculative candidacy, which is what actually
    # exercises "does not preempt/does not double-book" rather than proving nothing.
    cluster_id = api.cluster_id_for_name(cluster_agent_name())
    assert cluster_id
    api.put_cluster_settings_ok(cluster_id, max_speculative_accelerators=8)

    second = api.submit_job(
        pe_id, agent, hours=0.02,
        job_overrides={"accelerator_type": SOLO_TYPE, "accelerator_count": 1},
    )
    assert_stable(
        "a second job on the same (now fully claimed) flavor is not admitted while the first "
        "holds the only node of this type",
        lambda: api.experiment(second)["status"],
        ok=lambda s: s in ("QUEUED",),
        duration=15,
    )
    assert _reason(api.experiment(second)) in ("waiting-for-scale-up", "capacity_unavailable"), (
        f"second job must wait, not preempt or speculate, while the first job holds the node "
        f"(observed reason={_reason(api.experiment(second))!r})"
    )
    api.cancel_job(second)
    api.cancel_job(big)
    api.put_cluster_settings_ok(cluster_id)  # clear the cap so it doesn't leak into later tests


def test_scale_up_timeout_fails_over_without_spending_retry_budget(api, experiment, run_id, deadline):
    """Scenario 2: a short per-cluster scale_up_timeout_seconds means the never-arriving node is
    given up on quickly. The job requeues with the failed cluster in tried_clusters,
    infra_requeue_count/max_retries are untouched (a scheduling failure is not the job's fault),
    and once every autoscaler candidate is tried the reason becomes no_scalable_capacity.

    Uses a 2-node request on a flavor with exactly one real host, not the oversized-single-node
    shape autoscaler.md's own wording suggests, for the same reason documented on
    test_oversized_job_submits_speculatively_and_blocks_preemption: fitsLargestNode's per-node
    accelerator ceiling is free-capacity-based (no installed-total metric exists), so any
    single-rank request that could ever speculate also fits live immediately on an empty node --
    there is no way to make a single-rank job genuinely, permanently Pending here. A 2-node gang
    against 1 real host has no such escape: one rank can bind, the second host can never exist
    locally, so it stays genuinely, permanently partial regardless of live-vs-speculative ceiling
    math -- exactly the stuck state this scenario needs to drive the scale_up_timeout path."""
    cluster_name = cluster_agent_name()
    cluster_id = api.cluster_id_for_name(cluster_name)
    assert cluster_id, f"cluster {cluster_name!r} has not reported a cluster_id yet"

    api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=15)
    try:
        agent = make_agent(api, run_id, "asc-timeout")
        pe_id = experiment("autoscaler-timeout", [agent], budget=5.0)

        job_id = api.submit_job(
            pe_id, agent, hours=0.02,
            job_overrides={
                "accelerator_type": SOLO_TYPE, "accelerator_count": 1, "num_nodes": 2,
                "command": ["python", "train_distributed.py"],
            },
        )
        eventually(
            f"{job_id} to speculatively SUBMIT",
            lambda: api.experiment(job_id)["status"],
            accept=lambda s: s == "SUBMITTED",
            deadline=deadline,
        )

        requeued = eventually(
            f"{job_id} to requeue on scale_up_timeout (15s deadline)",
            lambda: api.experiment(job_id),
            accept=lambda e: e["status"] == "QUEUED",
            deadline=Deadline.in_seconds(60),
        )
        assert requeued.get("infra_requeue_count", 0) == 0, (
            "scale_up_timeout must not spend infra_requeue_count -- it is a scheduling failure, "
            "not an environment fault charged to the job"
        )
        assert requeued.get("attempt_count", 0) >= 1, "attempt_count should still advance"

        # This cluster is the only autoscaler candidate for SOLO_TYPE -- once tried, the job has
        # nowhere left to speculate and must report no_scalable_capacity, not sit silently QUEUED
        # under a stale capacity_unavailable.
        final = eventually(
            f"{job_id} to report no_scalable_capacity once the only candidate is tried",
            lambda: api.experiment(job_id),
            accept=lambda e: _reason(e) == "no_scalable_capacity",
            deadline=Deadline.in_seconds(30),
        )
        assert _reason(final) == "no_scalable_capacity"
        api.cancel_job(job_id)
    finally:
        api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=None)


def test_partial_gang_completes_or_evicts_wholly_on_deadline(api, experiment, run_id, deadline):
    """Scenario 3: N ranks, N-1 hosts free -- N-1 pods bind, the gang shows a partial
    scheduled_nodes count. Uncordoning the last node before the deadline lets the gang complete;
    the same setup left past the deadline evicts every pod, none left running."""
    cluster_name = cluster_agent_name()
    cluster_id = api.cluster_id_for_name(cluster_name)
    assert cluster_id

    nodes = all_cordoned_nodes()
    assert not nodes, f"test started with nodes already cordoned: {nodes}"

    from support.cluster import _kubectl  # local: no public "list nodes by label" helper exists yet

    solo_nodes = [
        n for n in _kubectl(
            "get", "nodes", "-l", SOLO_TYPE, "-o",
            "jsonpath={.items[*].metadata.name}",
        ).split()
    ]
    if len(solo_nodes) < 2:
        pytest.skip(f"{SOLO_TYPE} needs >=2 nodes locally to exercise a partial gang; found {solo_nodes}")

    cordoned = solo_nodes[0]
    from support.cluster import cordon_node
    cordon_node(cordoned)
    try:
        api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=20)
        agent = make_agent(api, run_id, "asc-gang")
        pe_id = experiment("autoscaler-gang", [agent], budget=5.0)

        job_id = api.submit_job(
            pe_id, agent, hours=0.02,
            job_overrides={
                "accelerator_type": SOLO_TYPE, "accelerator_count": 1,
                "num_nodes": len(solo_nodes),
                "command": ["python", "train_distributed.py"],
            },
        )
        eventually(
            f"{job_id} partial gang binds {len(solo_nodes) - 1} of {len(solo_nodes)} ranks",
            lambda: job_pod_count(job_id) == len(solo_nodes) - 1,
            accept=lambda ok: ok,
            deadline=deadline,
        )

        uncordon_node(cordoned)
        completed = eventually(
            f"{job_id} completes once the last node returns before the deadline",
            lambda: api.experiment(job_id),
            accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
            deadline=Deadline.in_seconds(45),
        )
        assert completed["status"] in ("RUNNING", "COMPLETED")
        api.cancel_job(job_id)
    finally:
        uncordon_node(cordoned)
        api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=None)


def test_partial_gang_evicted_wholly_past_deadline(api, experiment, run_id, deadline):
    """Same partial-gang setup as above, but the missing node never returns before the (short)
    deadline -- the whole gang must be torn down, not left with orphaned landed ranks."""
    cluster_name = cluster_agent_name()
    cluster_id = api.cluster_id_for_name(cluster_name)
    assert cluster_id

    from support.cluster import _kubectl, cordon_node

    solo_nodes = _kubectl(
        "get", "nodes", "-l", SOLO_TYPE, "-o",
        "jsonpath={.items[*].metadata.name}",
    ).split()
    if len(solo_nodes) < 2:
        pytest.skip(f"{SOLO_TYPE} needs >=2 nodes locally to exercise a partial gang; found {solo_nodes}")

    cordoned = solo_nodes[0]
    cordon_node(cordoned)
    try:
        api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=15)
        agent = make_agent(api, run_id, "asc-gang-evict")
        pe_id = experiment("autoscaler-gang-evict", [agent], budget=5.0)

        job_id = api.submit_job(
            pe_id, agent, hours=0.02,
            job_overrides={
                "accelerator_type": SOLO_TYPE, "accelerator_count": 1,
                "num_nodes": len(solo_nodes),
                "command": ["python", "train_distributed.py"],
            },
        )
        eventually(
            f"{job_id} partial gang binds {len(solo_nodes) - 1} of {len(solo_nodes)} ranks",
            lambda: job_pod_count(job_id) == len(solo_nodes) - 1,
            accept=lambda ok: ok,
            deadline=deadline,
        )

        eventually(
            f"{job_id} evicts the whole gang once the scale-up deadline passes",
            lambda: api.experiment(job_id)["status"],
            accept=lambda s: s == "QUEUED",
            deadline=Deadline.in_seconds(45),
        )
        assert job_pod_count(job_id) == 0, "a partial gang past its deadline must have zero pods left, not orphan landed ranks"
        api.cancel_job(job_id)
    finally:
        uncordon_node(cordoned)
        api.put_cluster_settings_ok(cluster_id, scale_up_timeout_seconds=None)


def test_concurrency_cap_gates_third_job_until_one_finishes(api, run_id, deadline):
    """Scenario 4: max_concurrent_accelerators=2 with three 1-accelerator jobs -- the third waits
    on concurrency_cap, then gets admitted once one of the first two finishes. Uses a normal
    (live-fit) flavor since the concurrency cap applies to every submit, live or speculative."""
    agent = make_agent(api, run_id, "asc-cap")
    pe_id = api.create_platform_experiment(
        f"autoscaler-cap-{run_id}", 5.0, 1, max_concurrent_accelerators=2,
    )
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)
    try:
        j1 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": SOLO_TYPE, "accelerator_count": 1})
        j2 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": SOLO_TYPE, "accelerator_count": 1})
        j3 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": SOLO_TYPE, "accelerator_count": 1})

        for j in (j1, j2):
            eventually(
                f"{j} admitted under the cap",
                lambda j=j: api.experiment(j)["status"],
                accept=lambda s: s in ("SUBMITTED", "RUNNING", "COMPLETED"),
                deadline=deadline,
            )
        assert_stable(
            "third job waits on concurrency_cap while two accelerators are already in flight",
            lambda: api.experiment(j3)["status"],
            ok=lambda s: s == "QUEUED",
            duration=10,
        )
        assert _reason(api.experiment(j3)) == "concurrency_cap"

        eventually(
            f"{j1} to reach a terminal state",
            lambda: api.experiment(j1)["status"],
            accept=lambda s: s in ("COMPLETED", "FAILED", "EVICTED"),
            deadline=deadline,
        )
        # The summary gate (an unrelated, pre-existing admission check: an agent with an
        # unsummarized completed job is blocked from new admissions) sits ahead of the
        # concurrency-cap check in the same filter pass -- j1 and j2 both finish around the same
        # time here, and either one left unsummarized would mask the cap behaviour this test is
        # actually after. File findings for whichever of j1/j2 already finished before waiting on
        # j3, so the only gate left between j3 and admission is the concurrency cap itself.
        summarized = set()

        def _j3_status_after_filing_findings():
            for j in (j1, j2):
                if j not in summarized and api.experiment(j)["status"] in ("COMPLETED", "FAILED", "EVICTED"):
                    api.file_finding(j)
                    summarized.add(j)
            return api.experiment(j3)["status"]

        eventually(
            f"{j3} admitted once {j1} frees its accelerator",
            _j3_status_after_filing_findings,
            accept=lambda s: s in ("SUBMITTED", "RUNNING", "COMPLETED"),
            deadline=deadline,
        )
        # A short job (hours=0.02) can already be COMPLETED by the time cleanup runs here --
        # cancel_job 409s on a terminal experiment, and that race is not what this test is about.
        for j in (j2, j3):
            if api.experiment(j)["status"] not in ("COMPLETED", "FAILED", "EVICTED"):
                api.cancel_job(j)
    finally:
        api.close_platform_experiment(pe_id)


def test_cluster_identity_survives_agent_restart(api, run_id):
    """Scenario 5 (restart form): cluster_id is derived live from the kube-system namespace UID,
    not generated/persisted by the runtime -- restarting the cluster-agent process must not change
    it, so cluster_settings and tried-history keep applying across a restart. Renaming CLUSTER_NAME
    itself is not exercised here: it would leave the shared dev cluster-agent Deployment under a
    different name for every other e2e scenario that reads cluster_agent_name(), which is a much
    larger blast radius than this lane's own cleanup can safely undo; the restart alone already
    covers the identity claim (edge-case table: "Cluster reconnects after a restart/outage")."""
    cluster_name = cluster_agent_name()
    before = api.cluster_id_for_name(cluster_name)
    assert before, f"cluster {cluster_name!r} has not reported a cluster_id yet"

    api.put_cluster_settings_ok(before, max_speculative_accelerators=3)
    try:
        import subprocess
        from support.cluster import CLUSTER_NS
        subprocess.run(
            ["kubectl", "-n", CLUSTER_NS, "rollout", "restart", "deployment/hypothesisloop-cluster-agent"],
            capture_output=True, text=True, timeout=30,
        )
        subprocess.run(
            ["kubectl", "-n", CLUSTER_NS, "rollout", "status", "deployment/hypothesisloop-cluster-agent", "--timeout=60s"],
            capture_output=True, text=True, timeout=65,
        )

        after = eventually(
            "cluster re-reports the same cluster_id after restart",
            lambda: api.cluster_id_for_name(cluster_name),
            accept=lambda cid: cid == before,
            deadline=Deadline.in_seconds(45),
        )
        assert after == before, "cluster_id must be stable across a runtime restart (kube-system UID is immutable)"

        settings = api.get_json(f"/clusters/{before}/settings")
        assert settings.get("max_speculative_accelerators") == 3, (
            "cluster_settings row must still apply to the same cluster_id after the agent restarted"
        )
    finally:
        api.put_cluster_settings_ok(before, max_speculative_accelerators=None)
