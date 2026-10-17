"""The whole sandboxed path on whatever providers the stack rents from, with nothing but an API
key of an ordinary org: a sandboxed coordinator and two sandboxed researchers work in a real
repository, their jobs run on a cluster the platform shares with every org and report metrics,
and the experiment ends with a summary and every machine released.

It rents machines and spends model tokens, so it carries the real_providers marker, which runs
only with make e2e RENT=1. It skips unless the environment names the repository, which must hold
mlp/job.yaml and a workload that reports val_accuracy:

    TEST_GIT_HOST_GITHUB_INSTALLATION   the installation of the platform's GitHub App that covers it
    TEST_GIT_HOST_GITHUB_REPOSITORY     the repository

    make e2e RENT=1 E2E_ARGS='e2e/test_real_providers.py -s'
"""
from __future__ import annotations

import os
import uuid
from datetime import datetime, timezone
from pathlib import Path

import pytest
import yaml

from support import admin
from support.api import API, ENDED, SANDBOX_BRANCH
from support.wait import Deadline, eventually

pytestmark = [pytest.mark.real_providers, pytest.mark.exclusive, pytest.mark.timeout(3600)]

MINUTE = 60.0
SANDBOX_ENV = ("TEST_GIT_HOST_GITHUB_INSTALLATION", "TEST_GIT_HOST_GITHUB_REPOSITORY")


def _instance(api: API, pe_id: str, agent_id: str | None) -> dict:
    if agent_id is None:
        return api.get_json(f"/platform-experiments/{pe_id}")["coordinator"]["instance"]
    return next(a for a in api.agents(pe_id) if a["agent_id"] == agent_id)["instance"]


def _jobs(api: API, pe_id: str) -> list[dict]:
    return api.get_json("/experiments", params={"platform_experiment_id": pe_id, "limit": 50})


@pytest.fixture(scope="module")
def org(api: API, run_id: str) -> API:
    """An ordinary org with one API key, which is all the test works with from here on, and the
    repository's installation connected to it."""
    missing = [name for name in SANDBOX_ENV if not os.environ.get(name)]
    if missing:
        pytest.skip(f"needs a repository: {', '.join(missing)} not set")
    subject = f"user-real-{uuid.uuid4().hex[:10]}-{run_id}"
    home = admin.add_user(subject, f"Real providers {run_id}")
    key = admin.add_api_key(subject, home["org_id"])
    admin.connect_github(home["org_id"], os.environ["TEST_GIT_HOST_GITHUB_INSTALLATION"])
    return api.as_caller(key, home["org_id"], home["project_id"])


def test_a_sandboxed_experiment_runs_end_to_end_on_rented_machines(org: API, run_id: str):
    deadline = Deadline.in_seconds(50 * MINUTE)
    repository = os.environ["TEST_GIT_HOST_GITHUB_REPOSITORY"]
    # The repository's own experiment file is another experiment, so this one states its brief
    # and metrics, which point at mlp/job.yaml there, and leaves the rest to the platform.
    stated = yaml.safe_load((Path(__file__).parent / "real_providers_experiment.yaml").read_text())
    created = org.post("/platform-experiments", {
        "project_id": org.project_id, "repository": repository, "branch": SANDBOX_BRANCH,
        "description": stated["description"], "metrics": stated["metrics"],
        "name": f"real-providers-{run_id}", "budget_accelerator_hours": 1, "max_agents": 3,
        "coordinator_in_agentsandbox": True, "duration_hours": 24,
    })
    assert created.status_code == 201, f"create: {created.status_code} {created.text}"
    pe_id = created.json()["id"]
    org.created_platform_experiments.append(pe_id)
    researchers = [org.add_agents(pe_id, in_agentsandbox=True, agent_type=agent_type)[0]["agent_id"]
                   for agent_type in ("hyperparameter-search", "architecture-search")]
    everyone: list[str | None] = [None, *researchers]
    # Its start time is now. The create made its agent of role baseline, and when that agent
    # finishes with one completed job the platform declares it and starts the run.
    org.patch(f"/platform-experiments/{pe_id}", {"starts_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}).raise_for_status()
    try:
        eventually("the baseline to be measured and declared and the platform to start the run", lambda: org.get_json(f"/platform-experiments/{pe_id}"),
                   accept=lambda pe: pe["status"] == "running" and pe.get("baseline_job_id"), reject=lambda pe: pe["status"] == "closed",
                   deadline=deadline, interval=10)
        eventually("the coordinator and both researchers run and report progress", lambda: [_instance(org, pe_id, who) for who in everyone],
                   accept=lambda instances: all(i["state"] == "running" and i.get("progress_at") for i in instances),
                   reject=lambda instances: any(i["stopped"] for i in instances), deadline=Deadline.in_seconds(10 * MINUTE), interval=5)

        # The researchers clone the repository, register a hypothesis, submit, and file a finding.
        finished = eventually("each researcher has a completed job", lambda: _jobs(org, pe_id), deadline=deadline, interval=15,
                              accept=lambda jobs: {j["agent_id"] for j in jobs if j["status"] == "COMPLETED"} >= set(researchers))
        for job in finished:
            if job["status"] == "COMPLETED":
                assert job["cluster_name"] and job["code_ref"].startswith(f"https://github.com/{repository}.git@"), job
        standings = next(m["standings"] for m in org.results(pe_id)["metrics"] if m["metric"] == "val_accuracy")
        assert {row["agent_id"] for row in standings} >= set(researchers) and all(0 < row["best"] <= 1 for row in standings), standings
        closed = eventually("the coordinator closes the experiment with a summary", lambda: org.get_json(f"/platform-experiments/{pe_id}"),
                            deadline=deadline, interval=15, accept=lambda pe: pe["status"] == "closed" and len(pe.get("summary") or "") > 40)
        assert all(not _instance(org, pe_id, who)["stopped"] for who in everyone), closed
    finally:
        org.close_platform_experiment(pe_id)
        # Nothing rented outlives the experiment: machines go after the wind-down, and jobs end.
        eventually("every machine released and every job ended",
                   lambda: ([_instance(org, pe_id, who) for who in everyone], _jobs(org, pe_id)), deadline=Deadline.in_seconds(8 * MINUTE),
                   accept=lambda seen: all(i["state"] == "stopped" for i in seen[0]) and all(j["status"] in ENDED for j in seen[1]),
                   interval=15)
