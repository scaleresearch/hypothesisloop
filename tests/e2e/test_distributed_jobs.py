"""Jobs that span nodes: a gang of num_nodes identical nodes, or groups of different shapes. Each
guarantee is asserted on a number the platform cannot fake: train_distributed.py's all_reduce over
N ranks is N(N-1)/2 only when every rank joined once, so "the gang formed" is arithmetic, and a
gang that fails or retries says so in its status and in what each attempt reported.
"""
from __future__ import annotations

import json
import re
import time

import pytest

from conftest import DEFAULT_ORG, TEST_CPU_ACCH_RATE, make_agent
from support.api import ENDED
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.02
GANG_RUN_SECONDS = 20
DIST_METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "world_reduced_sum", "direction": "maximize"},
    {"key": "accelerators_per_node", "direction": "maximize"},
    {"key": "gang_attempt", "direction": "maximize"},
]


def gang(num_nodes: int, max_retries: int = 0, **env: str) -> dict:
    """A gang of num_nodes one-core nodes running train_distributed.py. The ranks may share a host:
    what is under test is that they find each other, on however many hosts the cluster has."""
    return {
        "command": ["python", "-u", "train_distributed.py"], "max_retries": max_retries, "num_nodes": num_nodes,
        "topology": {"spread_across_hosts": False},
        "env": {"RUN_SECONDS": str(GANG_RUN_SECONDS), **env},
    }


TRAINER_CORES = 2
WORKER_REPLICAS = 3
GROUP_NODES = 1 + WORKER_REPLICAS
GROUP_CORES = TRAINER_CORES + WORKER_REPLICAS


def groups(max_retries: int = 0, **env: str) -> dict:
    """One trainer of two cores beside three one-core workers. The node's shape is stated per
    group only, so the default job's cpu, memory and storage are dropped."""
    return {
        "command": ["python", "-u", "train_distributed.py"], "max_retries": max_retries,
        "cpu": None, "memory": None, "storage": None,
        "topology": {"spread_across_hosts": False},
        "env": {"RUN_SECONDS": str(GANG_RUN_SECONDS), **env},
        "groups": [
            {"name": "trainer", "replicas": 1, "cpu": str(TRAINER_CORES), "memory": "128Mi", "storage": "512Mi"},
            {"name": "worker", "replicas": WORKER_REPLICAS, "cpu": "1", "memory": "64Mi", "storage": "256Mi"},
        ],
    }


def _ended(api, job_id, deadline, description):
    return eventually(description, lambda: api.experiment(job_id), accept=lambda e: e["status"] in ENDED, deadline=deadline)


RANK_HEADER = re.compile(r"^--- rank (\d+)( failed, exit (\d+))? \((group ([^,]+), )?(pod|instance) [^)]+\) ---$")


def _rank_headers(api, job_id) -> list[re.Match]:
    """The rank section headers of a gang's reported log tail, in the order the tail has them."""
    return [m for m in map(RANK_HEADER.match, json.loads(api.logs(job_id, n=200))) if m]


def _multi_node_hosts(api) -> int:
    """The most hosts of a cluster of the default org that runs a job over several nodes."""
    return max((c["hosts"] for c in api.capacity(DEFAULT_ORG)["clusters"] if c["multi_node"]), default=0)


def test_a_gang_forms_one_process_group_and_is_billed_for_every_node(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "gang-form")
    pe_id = experiment("distributed-gang-form", [agent], budget=5.0, metrics=DIST_METRICS)
    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=gang(3))
    submitted = api.experiment(job_id)
    assert submitted["accelerator_count"] == 3, f"a gang of three one-core nodes holds {submitted['accelerator_count']} units"
    assert submitted["estimated_cost_acch"] == pytest.approx(3 * JOB_HOURS * TEST_CPU_ACCH_RATE), submitted["estimated_cost_acch"]

    final = _ended(api, job_id, deadline, f"3-rank gang {job_id} to end")
    assert final["status"] == "COMPLETED", f"3-rank gang ended {final['status']} ({final.get('eviction_reason')})"
    reduced = eventually("the gang's world_reduced_sum", lambda: api.metric_max(job_id, "world_reduced_sum"),
                         accept=lambda v: v is not None, deadline=deadline)
    assert reduced == pytest.approx(3.0), f"all_reduce = {reduced}, want 3 (0+1+2): a rank did not join the process group"
    eventually(f"{job_id}'s cost to be billed", lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
               accept=lambda used: used > quota_before, deadline=deadline)
    api.file_finding(job_id, "distributed e2e: 3-rank rendezvous formed, reduced value correct.")


def test_a_failing_rank_ends_the_whole_gang(api, experiment, run_id, deadline):
    """FAIL_RANK exits non-zero after the barrier, so every rank is running when one dies; the
    others are told to run for 600s, so a gang that outlives its rank says so in the wall clock."""
    agent = make_agent(api, run_id, "gang-fail")
    pe_id = experiment("distributed-gang-fail", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=gang(3, RUN_SECONDS="600", FAIL_RANK="2"))
    eventually(f"{job_id} to start", lambda: api.experiment(job_id),
               accept=lambda e: e["status"] in ("RUNNING", "FAILED"), reject=lambda e: e["status"] in ENDED - {"FAILED"}, deadline=deadline)
    started = time.monotonic()
    final = _ended(api, job_id, deadline, f"{job_id} to end after a rank fails")
    assert final["status"] == "FAILED", f"the gang ended {final['status']} after a rank failed, want FAILED"
    assert time.monotonic() - started < 150, "the surviving ranks ran on after rank 2 died"

    # The tail has a section per rank whose pod still exists, and the failed rank's is at its very
    # end, where the failure cause and hl job logs read. The other ranks' pods may be gone by then.
    headers = eventually(f"{job_id}'s log tail to show the failed rank", lambda: _rank_headers(api, job_id),
                         accept=lambda h: bool(h) and h[-1].group(1) == "2", deadline=deadline)
    assert headers[-1].group(3) == "3", f"rank 2's section reads {headers[-1].group(0)!r}, want it failed with exit 3"


def test_a_gang_retry_restarts_every_rank_and_bills_every_attempt(api, experiment, run_id, deadline):
    """Each attempt reports its own index under the same job id, so the distinct gang_attempt
    values are the attempts that ran, and the reduced value says each was a whole gang."""
    agent = make_agent(api, run_id, "gang-retry")
    pe_id = experiment("distributed-gang-retry", [agent], budget=5.0, metrics=DIST_METRICS)
    quota_before = api.quota_field(pe_id, agent, "used_guaranteed_acch")

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=gang(3, max_retries=1, FAIL_RANK="1"))
    final = _ended(api, job_id, deadline, f"{job_id} to spend its one retry")
    assert final["status"] == "FAILED", f"a gang that fails twice with max_retries=1 ended {final['status']}"
    attempts = eventually("both attempts' gang_attempt", lambda: api.metric_distinct_count(job_id, "gang_attempt"),
                          accept=lambda n: n >= 2, deadline=deadline)
    assert attempts == 2, f"{attempts} attempts reported, want 2"
    assert api.metric_max(job_id, "world_reduced_sum") == pytest.approx(3.0), "a retry ran a partial gang"
    eventually(f"{job_id}'s attempts to be billed", lambda: api.quota_field(pe_id, agent, "used_guaranteed_acch"),
               accept=lambda used: used > quota_before, deadline=deadline)


def test_groups_of_different_shapes_form_one_process_group_and_are_billed_by_shape(api, experiment, run_id, deadline):
    """Four nodes in two groups are one gang: the only correct reduced value is 0+1+2+3 = 6, and
    the job holds the cores of each group summed, not four nodes of the trainer's shape."""
    agent = make_agent(api, run_id, "groups-form")
    pe_id = experiment("distributed-groups-form", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=groups())
    submitted = api.experiment(job_id)
    assert submitted["accelerator_count"] == GROUP_CORES, f"the groups hold {submitted['accelerator_count']} cores, want {GROUP_CORES}"
    assert submitted["estimated_cost_acch"] == pytest.approx(GROUP_CORES * JOB_HOURS * TEST_CPU_ACCH_RATE), submitted["estimated_cost_acch"]

    final = _ended(api, job_id, deadline, f"grouped gang {job_id} to end")
    assert final["status"] == "COMPLETED", f"the grouped gang ended {final['status']} ({final.get('eviction_reason')})"
    want = GROUP_NODES * (GROUP_NODES - 1) // 2
    reduced = eventually("the groups' world_reduced_sum", lambda: api.metric_max(job_id, "world_reduced_sum"),
                         accept=lambda v: v is not None, deadline=deadline)
    assert reduced == pytest.approx(want), f"all_reduce = {reduced}, want {want}: the groups did not form one gang, or two ranks collided"
    api.file_finding(job_id, "distributed e2e: a two-group gang formed one process group.")


def test_a_failing_worker_ends_the_trainer_of_its_groups(api, experiment, run_id, deadline):
    """FAIL_RANK 3 is the last worker only if the group offsets add up, and the trainer is told to
    run for 600s, so a trainer that outlives its worker says so in the wall clock."""
    agent = make_agent(api, run_id, "groups-fail")
    pe_id = experiment("distributed-groups-fail", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=groups(RUN_SECONDS="600", FAIL_RANK="3"))
    eventually(f"{job_id} to start", lambda: api.experiment(job_id),
               accept=lambda e: e["status"] in ("RUNNING", "FAILED"), reject=lambda e: e["status"] in ENDED - {"FAILED"}, deadline=deadline)
    started = time.monotonic()
    final = _ended(api, job_id, deadline, f"{job_id} to end after a worker fails")
    assert final["status"] == "FAILED", f"the grouped gang ended {final['status']} after a worker died, want FAILED"
    assert time.monotonic() - started < 150, "the trainer ran on after its worker died"

    headers = eventually(f"{job_id}'s log tail to show the failed worker", lambda: _rank_headers(api, job_id),
                         accept=lambda h: bool(h) and h[-1].group(1) == "3", deadline=deadline)
    assert headers[-1].group(3) == "3" and headers[-1].group(5) == "worker", \
        f"the last section reads {headers[-1].group(0)!r}, want rank 3 of group worker failed with exit 3"


def test_each_node_of_a_gang_is_told_its_own_accelerators(api, experiment, run_id, deadline, accelerator):
    """With accelerator_count=2 and num_nodes=2 a pod is told 2 while the job holds 4: a workload
    sizing its processes from the job's total would start 4 for the 2 devices it has."""
    if accelerator.per_node < 2:
        pytest.skip(f"no node offers two {accelerator.type}")
    agent = make_agent(api, run_id, "gang-pernode")
    pe_id = experiment("distributed-gang-pernode", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={
        **gang(2), "cpu": "250m", "accelerator_type": accelerator.type, "accelerator_count": 2})
    final = _ended(api, job_id, deadline, f"{job_id} to end")
    assert final["status"] == "COMPLETED", f"accelerator_count=2 num_nodes=2 ended {final['status']}"
    assert final["accelerator_count"] == 4, f"the job holds {final['accelerator_count']}, want 4 (2 per node x 2 nodes)"
    per_node = eventually("accelerators_per_node", lambda: api.metric_max(job_id, "accelerators_per_node"),
                          accept=lambda v: v is not None, deadline=deadline)
    assert per_node == pytest.approx(2.0), f"HYPOTHESISLOOP_ACCELERATOR_COUNT reads {per_node}, want 2: the pod is told the job's total"
    api.file_finding(job_id, "distributed e2e: per-node accelerator count is true of the pod.")


def test_a_spread_gang_with_more_ranks_than_hosts_is_refused_and_told_how_to_run(api, experiment, run_id):
    """A gang that needs a host per rank on a cluster with fewer hosts never runs, so it is refused
    at submit, naming the hosts it needs and the setting that lets its ranks share them."""
    hosts = _multi_node_hosts(api)
    if hosts == 0:
        pytest.skip("no cluster of the default org runs multi-node jobs")
    agent = make_agent(api, run_id, "gang-few-hosts")
    pe_id = experiment("distributed-gang-few-hosts", [agent], budget=5.0, metrics=DIST_METRICS)

    body = api.submission_body_for_id(api._job_id(), pe_id, agent)
    body["job"].update({**gang(hosts + 1), "topology": {"spread_across_hosts": True}})
    refused = api.post_experiment_body(body)
    assert refused.status_code == 422, f"a spread gang of {hosts + 1} ranks on {hosts} hosts: {refused.status_code} {refused.text}"
    answer = refused.json()
    assert answer["reason"] == "exceeds_cluster_capacity", answer
    assert f"{hosts + 1} hosts" in answer["message"] and "topology.spread_across_hosts: false" in answer["message"], answer["message"]

    stacked = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=gang(hosts + 1))
    assert api.experiment(stacked)["status"] not in ENDED - {"COMPLETED"}, "the same gang with its ranks on shared hosts was not taken"
    api.cancel_job(stacked)


@pytest.mark.timeout(600)
def test_a_torch_gloo_gang_all_reduces(api, experiment, run_id, deadline):
    """torch.distributed over gloo, the way a real job forms its process group: env:// reads the
    variables the runtime sets, and gloo connects every pair of ranks, not only rank 0's port."""
    agent = make_agent(api, run_id, "gang-torch")
    pe_id = experiment("distributed-gang-torch", [agent], budget=5.0, metrics=DIST_METRICS)

    install = "pip install -q --root-user-action=ignore torch --index-url https://download.pytorch.org/whl/cpu"
    job_id = api.submit_job(pe_id, agent, hours=0.1, job_overrides={
        **gang(2), "command": ["sh", "-c", f"{install} && exec python -u train_distributed_torch.py"],
        "memory": "2Gi", "storage": "4Gi"})
    final = _ended(api, job_id, deadline, f"torch gang {job_id} to end")
    assert final["status"] == "COMPLETED", f"the torch gang ended {final['status']} ({final.get('eviction_reason')})"
    reduced = eventually("the torch gang's world_reduced_sum", lambda: api.metric_max(job_id, "world_reduced_sum"),
                         accept=lambda v: v is not None, deadline=deadline)
    assert reduced == pytest.approx(1.0), f"gloo all_reduce = {reduced}, want 1 (0+1)"


def test_cancelling_a_running_gang_ends_it_and_releases_what_it_held(api, experiment, run_id, deadline):
    """A cancel of a running gang ends every rank of it: the job reads CANCELLED through hl, and
    what it held of its agent's quota is released, so nothing of it is left holding anything."""
    agent = make_agent(api, run_id, "gang-cancel")
    pe_id = experiment("distributed-gang-cancel", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides=gang(2, RUN_SECONDS="600"))
    eventually(f"{job_id} to run", lambda: api.experiment(job_id), accept=lambda e: e["status"] == "RUNNING",
               reject=lambda e: e["status"] in ENDED, deadline=deadline)
    assert api.quota_field(pe_id, agent, "held_acch") > 0, "a running gang holds nothing of its agent's quota"

    cancelled = json.loads(api.hl("job", "cancel", job_id))
    assert cancelled["status"] == "CANCELLED", cancelled
    final = eventually(f"{job_id} to read ended through hl", lambda: json.loads(api.hl("job", "status", job_id)),
                       accept=lambda e: e["status"] in ENDED, deadline=deadline)
    assert final["status"] == "CANCELLED", f"the cancelled gang ended {final['status']}"
    eventually(f"what {job_id} held to be released", lambda: api.quota_field(pe_id, agent, "held_acch"),
               accept=lambda held: held == pytest.approx(0), deadline=deadline)
