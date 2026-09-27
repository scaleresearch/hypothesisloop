"""The whole hosted path on real providers, with nothing but an API key of an ordinary org: a
hosted coordinator and two hosted researchers in rented sandboxes work in a real repository,
their jobs run on a rented GPU cluster and report metrics, one sandbox is lost at the vendor and
one harness is frozen, and the experiment still ends with a summary and nothing left rented.

It rents machines and spends model tokens, so it is no part of the suite: it carries the
real_providers marker, which every default selection leaves out, and it skips unless the
environment says where it runs:

    API_URL, HL_TOKEN                 the hosted dev stack (make dev-hosted) and its staff token,
                                      used here only to make the org and connect the repository
    HL_E2E_GITHUB_INSTALLATION        the installation of the platform's GitHub App that covers
    HL_E2E_GITHUB_REPO                the repository, which must hold .hypothesisloop/experiment.yaml,
                                      mlp/job.yaml and a workload that reports val_accuracy
    HOSTED_SANDBOX_PROVIDER           fly or e2b, as the stack's launcher is configured
    FLY_APP, FLY_API_TOKEN            for fly
    E2B_API_KEY                       for e2b
    VAST_API_KEY                      the account the cloud cluster rents jobs on

    cd tests && uv run pytest e2e/test_real_providers.py -m real_providers -s
"""
from __future__ import annotations

from datetime import datetime, timezone

import os
import time
import uuid
from pathlib import Path

import pytest
import requests
import yaml

from support import admin, vendors
from support.api import API
from support.wait import Deadline, eventually

pytestmark = [pytest.mark.real_providers, pytest.mark.exclusive, pytest.mark.timeout(3600)]

NEEDED = {
    "fly": ("FLY_APP", "FLY_API_TOKEN"),
    "e2b": ("E2B_API_KEY",),
}
# domain.AgentProgressLimit on the hosted stack (agents.progress_limit_seconds), and the quarter
# of it the platform may take to write a marker down.
PROGRESS_LIMIT_S = 300
MINUTE = 60.0


def _instance(api: API, pe_id: str, agent_id: str | None) -> dict:
    if agent_id is None:
        return api.get_json(f"/platform-experiments/{pe_id}")["coordinator"]["instance"]
    return next(a for a in api.agents(pe_id) if a["agent_id"] == agent_id)["instance"]


def _jobs(api: API, pe_id: str) -> list[dict]:
    return api.get_json("/experiments", params={"platform_experiment_id": pe_id, "limit": 50})


@pytest.fixture(scope="module")
def provider() -> str:
    provider = os.environ.get("HOSTED_SANDBOX_PROVIDER", "")
    if provider not in NEEDED:
        pytest.skip("needs real providers: set HOSTED_SANDBOX_PROVIDER to fly or e2b, as the stack's launcher is configured")
    missing = [key for key in (*NEEDED[provider], "VAST_API_KEY", "HL_E2E_GITHUB_INSTALLATION", "HL_E2E_GITHUB_REPO") if not os.environ.get(key)]
    if missing:
        pytest.skip(f"needs real providers: {', '.join(missing)} not set")
    return provider


@pytest.fixture(scope="module")
def org(api: API, run_id: str, provider: str) -> API:
    """An ordinary org with one API key, which is all the test works with from here on, and the
    sandbox repository's installation connected to it."""
    subject = f"user-real-{uuid.uuid4().hex[:10]}-{run_id}"
    home = admin.add_user(subject, f"Real providers {run_id}")
    key = admin.add_api_key(subject, home["org_id"])
    assert key.startswith("hlk_")
    admin.connect_github(home["org_id"], os.environ["HL_E2E_GITHUB_INSTALLATION"])
    return api.as_caller(key, home["org_id"], home["project_id"])


def test_a_hosted_experiment_runs_end_to_end_on_rented_machines_and_survives_a_lost_and_a_hung_sandbox(org: API, provider: str, run_id: str):
    assert not vendors.sandboxes(provider) and not vendors.rented_instances(), "something is rented before the test starts"
    credit = vendors.job_credit()
    deadline = Deadline.in_seconds(50 * MINUTE)

    # The repository's own experiment file is its CPU experiment, so this one states its brief
    # and metrics, which point at mlp/job.yaml there, and takes the rest from the file.
    stated = yaml.safe_load((Path(__file__).parent / "real_providers_experiment.yaml").read_text())
    created = org.post("/platform-experiments", {
        "project_id": org.project_id, "from_repo": True, "repository": os.environ["HL_E2E_GITHUB_REPO"],
        "description": stated["description"], "metrics": stated["metrics"],
        "name": f"real-providers-{run_id}", "budget_accelerator_hours": 1, "max_agents": 3,
        "coordinator_hosted": True, "duration_hours": 24,
    })
    assert created.status_code == 201, f"create: {created.status_code} {created.text}"
    pe_id = created.json()["id"]
    org.created_platform_experiments.append(pe_id)
    assert created.json()["repository_url"].endswith(os.environ["HL_E2E_GITHUB_REPO"] + ".git")
    researchers = [org.add_agents(pe_id, hosted=True, flavor=flavor)[0]["agent_id"] for flavor in ("hyperparameter-search", "architecture-search")]
    # Its start time is now, and the platform starts it once its coordinator has had the baseline
    # measured by an agent it added and has declared it: no researcher's machine is rented before.
    org.patch(f"/platform-experiments/{pe_id}", {"starts_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}).raise_for_status()
    eventually("the coordinator to declare a baseline and the platform to start the run", lambda: org.get_json(f"/platform-experiments/{pe_id}"),
               accept=lambda pe: pe["status"] == "running" and pe.get("baseline_job_id"), reject=lambda pe: pe["status"] == "closed",
               deadline=deadline, interval=10)
    everyone: list[str | None] = [None, *researchers]

    try:
        # Each of the three is a sandbox at the vendor, running, with its harness reporting progress.
        def all_working() -> list[dict]:
            return [_instance(org, pe_id, who) for who in everyone]
        eventually("the coordinator and both researchers run and report progress", all_working, deadline=Deadline.in_seconds(6 * MINUTE),
                   accept=lambda instances: all(i["state"] == "running" and i.get("progress_at") for i in instances),
                   reject=lambda instances: any(i["stopped"] for i in instances), interval=5)
        rented = vendors.sandboxes(provider)
        names = {who: _instance(org, pe_id, who)["name"] for who in everyone}
        assert len(rented) == 3 and all(any(name in vendor_name for vendor_name in rented) for name in names.values()), \
            f"the vendor holds {sorted(rented)}, the platform asked for {sorted(names.values())}"

        # The researchers clone the repository, register a hypothesis and submit: a job reaches
        # the rented cluster and runs there.
        eventually("a job runs on the rented cluster", lambda: _jobs(org, pe_id), deadline=Deadline.in_seconds(15 * MINUTE),
                   accept=lambda jobs: any(j["status"] in ("RUNNING", "COMPLETED") for j in jobs),
                   reject=lambda jobs: any(j["status"] in ("FAILED", "EVICTED", "REJECTED") for j in jobs), interval=10)
        assert vendors.rented_instances() or any(j["status"] == "COMPLETED" for j in _jobs(org, pe_id)), "a job runs and nothing is rented for it"

        # The vendor loses the coordinator's sandbox. The platform sees a machine that is gone,
        # spends one restart and starts another, which takes up the stored session.
        def vendor_id(who: str | None) -> str:
            name = _instance(org, pe_id, who)["name"]
            return next(vendor for vendor_name, vendor in vendors.sandboxes(provider).items() if name in vendor_name)
        vendors.lose_sandbox(provider, vendor_id(None))
        lost_at = time.monotonic()
        replaced = eventually("the lost coordinator is replaced and works again", lambda: _instance(org, pe_id, None), deadline=Deadline.in_seconds(5 * MINUTE),
                              accept=lambda i: i["restarts"] == 1 and i["state"] == "running" and i.get("progress_at") and i["name"] != names[None], interval=5)
        assert replaced["end_reason"] == "gone" and "no longer lists it" in replaced["exit_log"], replaced
        print(f"\nlost sandbox replaced and working after {time.monotonic() - lost_at:.0f}s")

        # A harness that hangs with its machine still running: only its silence says so.
        hung = researchers[0]
        vendors.freeze_harness(provider, vendor_id(hung))
        frozen_at = time.monotonic()
        ended = eventually("the frozen researcher is ended as silent and replaced", lambda: _instance(org, pe_id, hung),
                           deadline=Deadline.in_seconds(1.25 * PROGRESS_LIMIT_S + 4 * MINUTE),
                           accept=lambda i: i["restarts"] == 1 and i["name"] != names[hung] and i["state"] == "running", interval=10)
        assert ended["end_reason"] == "silent" and "no progress" in ended["exit_log"], ended
        assert time.monotonic() - frozen_at >= PROGRESS_LIMIT_S - 30, "a working agent was ended before its limit"
        assert _instance(org, pe_id, researchers[1])["restarts"] == 0, "the researcher that kept working was replaced"

        # Both jobs finish with the declared metric, each pinned to the commit it ran, and the
        # coordinator files its summary and closes.
        finished = eventually("each researcher has a completed job", lambda: _jobs(org, pe_id), deadline=deadline,
                              accept=lambda jobs: {j["agent_id"] for j in jobs if j["status"] == "COMPLETED"} == set(researchers), interval=15)
        results = org.get_json(f"/platform-experiments/{pe_id}/results")
        standings = next(m["standings"] for m in results["metrics"] if m["metric"] == "val_accuracy")
        assert {row["agent_id"] for row in standings} == set(researchers) and all(0.5 < row["best"] <= 1 for row in standings), standings
        for job in finished:
            if job["status"] == "COMPLETED":
                assert job["cluster_name"] and "@" in job["code_ref"] and os.environ["HL_E2E_GITHUB_REPO"] in job["code_ref"], job
        closed = eventually("the coordinator closes the experiment with a summary", lambda: org.get_json(f"/platform-experiments/{pe_id}"), deadline=deadline,
                            accept=lambda pe: pe["status"] == "closed" and len(pe.get("summary") or "") > 40, interval=15)
        assert all(not _instance(org, pe_id, who)["stopped"] for who in everyone), closed
    finally:
        org.session.post(org._url(f"/platform-experiments/{pe_id}/close"), timeout=30)
        # Nothing rented outlives the experiment: sandboxes go after the wind-down, instances
        # with their jobs.
        left = eventually("no sandbox and no instance is rented any more", lambda: (vendors.sandboxes(provider), vendors.rented_instances()),
                          deadline=Deadline.in_seconds(8 * MINUTE), accept=lambda rented: not rented[0] and not rented[1], interval=15)
        assert left == ({}, {})
        print(f"\njobs cost {credit - vendors.job_credit():.3f} USD at the GPU vendor")
