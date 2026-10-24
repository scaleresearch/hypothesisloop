"""What a gang leaves on a Kubernetes cluster, which only the cluster shows: where its pods ran
and the objects it made beside them. The rest of the suite drives the platform through its API
and hl alone; these read the cluster with kubectl, and skip unless the stack's jobs run on a
Kubernetes cluster that kubectl reaches.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess

import pytest

from conftest import DEFAULT_ORG, make_agent
from support.api import ENDED
from support.wait import eventually
from test_distributed_jobs import DIST_METRICS, JOB_HOURS, gang

pytestmark = [pytest.mark.parallel, pytest.mark.k8s_runtime]

KUBE_CONTEXT = os.environ.get("E2E_KUBE_CONTEXT", "k3s-local")
JOB_NAMESPACE = "hypothesisloop-jobs"
EXPERIMENT_LABEL = "hypothesisloop.io/experiment-id"


@pytest.fixture
def kube(api):
    """kubectl against the Kubernetes cluster the default org's gangs run on. Skips where the
    stack runs them on no such cluster or kubectl does not reach it."""
    if not any(c["multi_node"] and not c.get("on_demand") for c in api.capacity(DEFAULT_ORG)["clusters"]):
        pytest.skip("no Kubernetes cluster of the default org runs multi-node jobs")
    if shutil.which("kubectl") is None:
        pytest.skip("no kubectl")
    if subprocess.run(["kubectl", "--context", KUBE_CONTEXT, "get", "nodes"], capture_output=True, timeout=15).returncode != 0:
        pytest.skip(f"kubectl context {KUBE_CONTEXT} is not reachable")

    def objects(kind: str, job_id: str) -> list[dict]:
        out = subprocess.run(
            ["kubectl", "--context", KUBE_CONTEXT, "-n", JOB_NAMESPACE, "get", kind, "-l", f"{EXPERIMENT_LABEL}={job_id}", "-o", "json"],
            capture_output=True, text=True, timeout=30, check=True).stdout
        return json.loads(out)["items"]
    return objects


def _hosts(api) -> int:
    return max((c["hosts"] for c in api.capacity(DEFAULT_ORG)["clusters"] if c["multi_node"] and not c.get("on_demand")), default=0)


def test_a_spread_gang_puts_each_rank_on_a_node_of_its_own_and_a_cancel_removes_what_it_made(api, experiment, run_id, deadline, kube):
    """spread_across_hosts is the default of a gang, and two ranks on one node would pass every
    check the API can make, so it is read off the pods. A gang is pods, a headless Service and a
    NetworkPolicy: a cancel that leaves one of them holds a node, a name or a rule nobody owns."""
    if _hosts(api) < 2:
        pytest.skip("no Kubernetes cluster of the default org runs multi-node jobs on two nodes or more")
    agent = make_agent(api, run_id, "gang-k8s")
    pe_id = experiment("distributed-gang-k8s", [agent], budget=5.0, metrics=DIST_METRICS)

    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={**gang(2, RUN_SECONDS="600"), "topology": None})
    pods = eventually(f"{job_id}'s two pods to be scheduled", lambda: kube("pods", job_id),
                      accept=lambda ps: len(ps) == 2 and all(p["spec"].get("nodeName") for p in ps), deadline=deadline)
    nodes = [p["spec"]["nodeName"] for p in pods]
    assert len(set(nodes)) == 2, f"the ranks of a spread gang share a node: {nodes}"
    made = lambda: {kind: len(kube(kind, job_id)) for kind in ("pods", "services", "networkpolicies")}
    eventually(f"{job_id}'s pods, Service and NetworkPolicy", made,
               accept=lambda m: m == {"pods": 2, "services": 1, "networkpolicies": 1}, deadline=deadline)

    api.cancel_job(job_id)
    final = eventually(f"{job_id} to end after its cancel", lambda: api.experiment(job_id),
                       accept=lambda e: e["status"] in ENDED, deadline=deadline)
    assert final["status"] == "CANCELLED", f"the cancelled gang ended {final['status']}"
    eventually(f"everything {job_id} made to be gone", made,
               accept=lambda m: m == {"pods": 0, "services": 0, "networkpolicies": 0}, deadline=deadline)
