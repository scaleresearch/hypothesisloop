"""Agents the platform runs, end to end against the control plane: they join through
POST /platform-experiments/{id}/agents and in no other way, and the agent-launcher's desired list
follows each of them from open through running to cut and close, and through the exits of its
machine.

No launcher process and no provider is involved: each reconcile call made here is the one a
launcher makes, and what a launcher does with the answer is pinned by
runtime/cloud/internal/agentlaunch's own tests, one of which runs it against the control plane's
handler. The local control plane gives hosted agents to the launcher named in its config
(agents.launcher).

A hosted agent works in its project's repository, so hosting is refused for a project that has
none. The tests that host agents therefore need a repository to attach, the same sandbox
test_project_repo.py names, and are skipped without it:

    HL_E2E_GITHUB_INSTALLATION    the installation of the App that covers it
    HL_E2E_GITHUB_REPO_ID         its id on GitHub

API-only, parallel-safe: every assertion is about this test's own platform experiments.
"""
from __future__ import annotations

import json
import os
import uuid

import pytest

from support import admin
from support.api import API
from support.wait import eventually

pytestmark = pytest.mark.parallel

LAUNCHER = "main"
MAX_RESTARTS = 3  # domain.MaxAgentInstanceRestarts

STAGES = [
    {"length_pct": 20, "evict_pct": 50},
    {"length_pct": 30, "evict_pct": 25},
    {"length_pct": 50, "evict_pct": 0},
]
JOB_HOURS = 0.0028
RUN_SECONDS = 40
BUDGET = 0.03


def _new_project(api: API, label: str) -> API:
    """The staff caller in a fresh project of the default org, which has clusters to run jobs on."""
    created = api.post(f"/orgs/{api.org_id}/projects", {"name": f"hosted {label} {uuid.uuid4().hex[:8]}"})
    assert created.status_code == 201, f"create a project: {created.status_code} {created.text}"
    return api.as_caller(api.token, api.org_id, created.json()["id"])


@pytest.fixture(scope="module")
def with_repo(api: API) -> API:
    """A project with a repository attached, where agents can be hosted."""
    installation, repo_id = os.environ.get("HL_E2E_GITHUB_INSTALLATION"), os.environ.get("HL_E2E_GITHUB_REPO_ID")
    if not installation or not repo_id:
        pytest.skip("hosting an agent needs a repository: set HL_E2E_GITHUB_INSTALLATION and HL_E2E_GITHUB_REPO_ID")
    if api.get("/github/app").status_code == 503:
        pytest.skip("this deployment has no GitHub App configured")
    project = _new_project(api, "repo")
    admin.attach_repo(project.project_id, installation, repo_id)
    yield project
    project.session.delete(project._url(f"/projects/{project.project_id}/repo"), timeout=30)


@pytest.fixture
def hosted_pe(with_repo: API, run_id: str):
    created: list[str] = []

    def _create(name: str, budget: float, max_agents: int, **kw) -> str:
        pe_id = with_repo.create_platform_experiment(f"{name}-{run_id}", budget, max_agents, **kw)
        created.append(pe_id)
        return pe_id

    yield _create
    for pe_id in created:
        with_repo.close_platform_experiment(pe_id)


def desired_for(api: API, pe_id: str, observed: list[dict] | None = None) -> tuple[dict[str, dict], set[str]]:
    """This platform experiment's part of the launcher's desired state: its running instances by
    agent id, and the names of the ending ones."""
    desired = api.launcher_desired(LAUNCHER, observed)
    assert isinstance(desired["instances"], list) and isinstance(desired["ending"], list), desired.keys()
    own = {i["env"]["AGENT_ID"]: i for i in desired["instances"] if i["env"]["PLATFORM_EXPERIMENT_ID"] == pe_id}
    return own, set(desired["ending"])


def roster(api: API, pe_id: str) -> dict[str, dict]:
    return {a["agent_id"]: a for a in api.agents(pe_id)}


def test_an_agent_is_hosted_only_in_a_project_with_a_repository(api: API, run_id: str):
    project = _new_project(api, "bare")
    pe_id = project.create_platform_experiment(f"hosted-no-repo-{run_id}", 1.0, 5)
    try:
        r = project.post(f"/platform-experiments/{pe_id}/agents", {"count": 2, "hosted": True})
        assert r.status_code == 409 and r.json()["reason"] == "no_repository", f"hosted with no repository: {r.status_code} {r.text}"
        assert project.project_id in r.json()["message"], r.text
        assert project.agents(pe_id) == [], "a refused hosted join left agents behind"

        # An agent its owner runs needs nothing of the platform's to start, and joins. What it
        # runs is resolved here and shown: the default flavor and the platform's default model.
        own = project.add_agents(pe_id)[0]
        assert not own["hosted"] and own["token"].startswith("hls_") and "instance" not in own, own
        assert own["flavor"] == "generalist" and own["model"], own
        named = project.add_agents(pe_id, flavor="data-search", model="another-model-1")[0]
        assert (named["flavor"], named["model"]) == ("data-search", "another-model-1"), named

        r = project.post(f"/platform-experiments/{pe_id}/agents", {"flavor": "no-such-flavor"})
        assert r.status_code == 400 and "no-such-flavor" in r.text, f"an unknown flavor: {r.status_code} {r.text}"
        r = project.post(f"/platform-experiments/{pe_id}/agents", {"hyperparameters": {"blob": "x" * 5000}})
        assert r.status_code == 400 and "4096" in r.text, f"oversize hyperparameters: {r.status_code} {r.text}"
        assert len(project.agents(pe_id)) == 2

        # Removal takes the place and leaves the agent, which joins again by its id.
        assert project.remove_agent(pe_id, own["agent_id"]).status_code == 200
        again = project.post_json(f"/platform-experiments/{pe_id}/agents", {"agent_id": own["agent_id"]})["agents"][0]
        assert again["agent_id"] == own["agent_id"] and again["token"] == own["token"], again
    finally:
        project.close_platform_experiment(pe_id)


def test_agents_join_through_the_api_and_the_launcher_is_told_what_to_run(with_repo: API, hosted_pe, run_id: str):
    api = with_repo
    pe_id = hosted_pe("hosted-agents", 1.0, 5)

    hosted = api.add_agents(pe_id, count=2, hosted=True, name=f"hosted-{run_id}", flavor="data-search",
                            model="claude-sonnet-5", hyperparameters={"batch_size": 64})
    self_run = api.add_agents(pe_id, flavor="generalist")
    assert len(hosted) == 2 and len(self_run) == 1
    hosted_ids = [a["agent_id"] for a in hosted]
    own_id, own_token = self_run[0]["agent_id"], self_run[0]["token"]
    assert all(a["hosted"] and a["launcher"] == LAUNCHER and "token" not in a for a in hosted), hosted
    assert all(a["instance"] == {"restarts": 0, "stopped": False} for a in hosted), hosted
    assert [a["name"] for a in hosted] == [f"hosted-{run_id}-1", f"hosted-{run_id}-2"]
    assert not self_run[0]["hosted"] and "launcher" not in self_run[0] and own_token.startswith("hls_")
    assert len(set(hosted_ids + [own_id])) == 3 and all(i.startswith("ag-") for i in hosted_ids + [own_id])

    listed = api.agents(pe_id)
    assert [a["agent_id"] for a in listed] == hosted_ids + [own_id]
    assert all("token" not in a for a in listed), "the roster lists a token"
    for agent_id in hosted_ids + [own_id]:
        assert api.signup_role(pe_id, agent_id) == (200, "competitor")

    instances, ending = desired_for(api, pe_id)
    assert sorted(instances) == sorted(hosted_ids), f"the launcher is told to run {sorted(instances)}"
    names = {i["name"] for i in instances.values()}
    assert len(names) == 2 and not names & ending
    for agent_id, instance in instances.items():
        env = instance["env"]
        assert set(env) == {"API_URL", "AGENT_ID", "PLATFORM_EXPERIMENT_ID", "AGENT_FLAVOR",
                            "AGENT_HYPERPARAMETERS", "AGENT_MODEL", "HL_TOKEN"}, sorted(env)
        assert env["AGENT_FLAVOR"] == "data-search" and env["AGENT_MODEL"] == "claude-sonnet-5"
        assert json.loads(env["AGENT_HYPERPARAMETERS"]) == {"batch_size": 64}
        assert env["API_URL"].startswith("http") and instance["image"]
        assert env["HL_TOKEN"] not in instance["name"]
        assert instance["name"] == instance["name"].lower() and len(instance["name"]) <= 58
        # The token in the environment is that agent's own: it reads its own signup and may name
        # no other agent.
        as_agent = api.as_caller(env["HL_TOKEN"])
        assert as_agent.signup_role(pe_id, agent_id) == (200, "competitor")
        r = as_agent.post("/hypotheses", {"agent_id": own_id, "platform_experiment_id": pe_id, "text": "as another",
                                          "links": [{"type": "builds_on", "to": pe_id}], "distinct_from": []})
        assert r.status_code == 403, f"a hosted agent's token naming another agent: {r.status_code} {r.text}"
    # The name a machine is asked for under is on the roster.
    assert {a: roster(api, pe_id)[a]["instance"]["name"] for a in hosted_ids} == {a: i["name"] for a, i in instances.items()}

    # Asking twice answers the same: the list is derived from rows, and nothing is minted anew.
    again, _ = desired_for(api, pe_id)
    assert again == instances

    # The self-run agent's token works the same way and its agent is in no launcher's list.
    assert api.as_caller(own_token).signup_role(pe_id, own_id) == (200, "competitor")

    # An agent cannot add itself or anyone else, and a token for an agent nobody added is refused.
    as_hosted = api.as_caller(instances[hosted_ids[0]]["env"]["HL_TOKEN"])
    r = as_hosted.post(f"/platform-experiments/{pe_id}/agents", {"count": 1})
    assert r.status_code == 403, f"an agent adding agents: {r.status_code} {r.text}"
    stranger = api.as_caller(admin.token(f"agent:{pe_id}/ag-{uuid.uuid4().hex}"))
    r = stranger.get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 401, f"a token for an agent that never joined: {r.status_code} {r.text}"
    r = api.post(f"/platform-experiments/{pe_id}/signup", {"agent_id": own_id})
    assert r.status_code in (404, 405), f"the old signup path still answers: {r.status_code} {r.text}"

    # Who may read a launcher's list: that launcher and nobody it launched.
    as_launcher = api.as_caller(admin.token(f"launcher:{LAUNCHER}"))
    assert sorted(desired_for(as_launcher, pe_id)[0]) == sorted(hosted_ids)
    question = {"launch_digest": API.LAUNCH_DIGEST, "instances": []}
    r = as_launcher.post(f"/internal/launchers/other-{run_id}/reconcile", question)
    assert r.status_code == 403, f"a launcher asking for another's list: {r.status_code} {r.text}"
    r = as_launcher.get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 403, f"a launcher token on the public API: {r.status_code} {r.text}"
    unknown = api.as_caller(admin.token(f"launcher:nobody-{run_id}"))
    r = unknown.post(f"/internal/launchers/nobody-{run_id}/reconcile", question)
    assert r.status_code == 401, f"a launcher nothing names: {r.status_code} {r.text}"
    for scope in (f"agent:{pe_id}/{hosted_ids[0]}", f"coordinator:{pe_id}"):
        r = api.as_caller(admin.token(scope)).post(f"/internal/launchers/{LAUNCHER}/reconcile", question)
        assert r.status_code == 403, f"{scope} reading the launcher's list: {r.status_code} {r.text}"
    r = as_launcher.post(f"/internal/launchers/{LAUNCHER}/reconcile", {"instances": []})
    assert r.status_code in (400, 422), f"a question that does not say what the launcher starts with: {r.status_code} {r.text}"

    # Removing a hosted agent before the start takes its instance out of the list and ends its token.
    removed, kept = hosted_ids
    removed_token, removed_name = instances[removed]["env"]["HL_TOKEN"], instances[removed]["name"]
    r = api.remove_agent(pe_id, removed)
    assert r.status_code == 200, f"remove before start: {r.status_code} {r.text}"
    instances, ending = desired_for(api, pe_id)
    assert sorted(instances) == [kept] and removed_name not in ending
    assert api.signup_role(pe_id, removed) == (404, None)
    r = api.as_caller(removed_token).get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 401, f"a removed agent's token: {r.status_code} {r.text}"

    # Started: the roster is fixed. Nobody joins, nobody is removed, and the instance stays.
    api.start_platform_experiment(pe_id)
    r = api.post(f"/platform-experiments/{pe_id}/agents", {"count": 1, "hosted": True})
    assert r.status_code == 400 and r.json()["reason"] == "signup_closed", f"join after start: {r.status_code} {r.text}"
    r = api.remove_agent(pe_id, kept)
    assert r.status_code == 409 and r.json()["reason"] == "experiment_started", f"remove after start: {r.status_code} {r.text}"
    running, _ = desired_for(api, pe_id)
    assert running == instances, "starting the experiment changed what the launcher is told to run"

    # Closed: the instance is no longer wanted and is named as ending under the name it was
    # created with. The launcher tells an ending instance to stop, which starts its agent's last
    # turn, and otherwise leaves it: nothing is started in its place. Its exit is no crash.
    kept_name = instances[kept]["name"]
    api.close_platform_experiment(pe_id)
    instances, ending = desired_for(api, pe_id, [{"name": kept_name, "state": "running"}])
    assert not instances, f"instances of a closed platform experiment: {sorted(instances)}"
    assert kept_name in ending, f"{kept_name} is not left to end: {sorted(ending)}"
    _, ending = desired_for(api, pe_id, [{"name": kept_name, "state": "exited", "log": "last turn done"}])
    assert kept_name in ending
    assert roster(api, pe_id)[kept]["instance"] == {"name": kept_name, "restarts": 0, "stopped": False}


def test_an_exited_machine_is_recorded_restarted_a_few_times_and_then_stops_its_agent(with_repo: API, hosted_pe, run_id: str):
    api = with_repo
    pe_id = hosted_pe("hosted-agents-exit", 1.0, 5)
    crashing, steady = (a["agent_id"] for a in api.add_agents(pe_id, count=2, hosted=True))
    api.start_platform_experiment(pe_id)
    instances, _ = desired_for(api, pe_id)
    steady_name = instances[steady]["name"]
    seen = set()

    for exit_number in range(1, MAX_RESTARTS + 2):
        name = instances[crashing]["name"]
        assert name not in seen, f"exit {exit_number}: the agent is asked for under {name} again"
        seen.add(name)
        log = f"Traceback\nSystemExit: agent: exit {exit_number}"
        observed = [{"name": name, "state": "exited", "log": log}, {"name": steady_name, "state": "running"}]
        instances, ending = desired_for(api, pe_id, observed)
        assert instances[steady]["name"] == steady_name, "the agent that never exited was renamed"
        assert name not in ending and name not in {i["name"] for i in instances.values()}, "the exited machine is still wanted"

        recorded = roster(api, pe_id)[crashing]["instance"]
        assert recorded["exit_log"] == log and recorded["exited_at"], recorded
        if exit_number <= MAX_RESTARTS:
            assert recorded["restarts"] == exit_number and not recorded["stopped"], recorded
            assert crashing in instances, f"exit {exit_number}: the agent is not asked for again"
            # The launcher could not destroy the old machine yet and reports it again.
            repeated, _ = desired_for(api, pe_id, observed)
            assert repeated[crashing]["name"] == instances[crashing]["name"], "one exit was counted twice"
            assert roster(api, pe_id)[crashing]["instance"]["restarts"] == exit_number
        else:
            assert recorded["stopped"] and recorded["restarts"] == MAX_RESTARTS, recorded
            assert crashing not in instances, "an agent with no restart left is still asked for"

    for _ in range(3):
        instances, _ = desired_for(api, pe_id, [{"name": steady_name, "state": "running"}])
        assert sorted(instances) == [steady]
    assert roster(api, pe_id)[steady]["instance"] == {"name": steady_name, "restarts": 0, "stopped": False}
    # The agent itself is still in the platform experiment: its place, quota and token stand.
    assert api.signup_role(pe_id, crashing) == (200, "competitor")


def test_a_cut_hosted_agent_is_left_to_end_while_survivors_keep_running(with_repo: API, hosted_pe, run_id: str, deadline):
    api = with_repo
    # Six agents and a 50% first cut: above the roster floor for a cut, see test_stage_ladder_cut.
    pe_id = hosted_pe("hosted-agents-cut", BUDGET, 6, report_interval_seconds=10, stages=STAGES)
    agents = [a["agent_id"] for a in api.add_agents(pe_id, count=6, hosted=True, quota_tier="guaranteed")]
    before, _ = desired_for(api, pe_id)
    assert sorted(before) == sorted(agents)
    api.start_platform_experiment(pe_id)

    run_env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}
    for agent in agents:
        api.submit_job(pe_id, agent, hours=JOB_HOURS, job_overrides={"env": run_env})

    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )
    cut = {c["agent_id"] for c in stages["cut_agents"]}
    survivors = set(stages["active_agents"])
    assert cut | survivors == set(agents)

    instances, ending = desired_for(api, pe_id)
    assert set(instances) == survivors, f"running: {sorted(instances)}, survivors: {sorted(survivors)}"
    for agent in cut:
        assert before[agent]["name"] in ending, f"cut agent {agent} is not left to end"
    for agent in survivors:
        assert instances[agent] == before[agent], f"the survivor {agent} would be replaced by the cut"

    api.close_platform_experiment(pe_id)
    instances, ending = desired_for(api, pe_id)
    assert not instances
    for agent in survivors:
        assert before[agent]["name"] in ending, f"{agent} is not left to end after the close"
