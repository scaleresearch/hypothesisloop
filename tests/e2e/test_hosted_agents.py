"""Agents the platform runs, end to end against the control plane: they join through
POST /platform-experiments/{id}/agents and in no other way, and the agent-launcher's desired list
follows each of them from open through running to cut and close, and through the exits of its
machine.

No launcher process and no provider is involved: each reconcile call made here is the one a
launcher makes, and what a launcher does with the answer is pinned by
runtime/cloud/internal/agentlaunch's own tests, one of which runs it against the control plane's
handler. The local control plane gives hosted agents to the launcher named in its config
(agents.launcher).

A hosted agent works in its platform experiment's repository, so hosting is refused for an
experiment that names none. The tests that host agents therefore need a repository to name, the
same sandbox test_experiment_repo.py pushes to, and are skipped without it (conftest's
sandbox_repository).

Exclusive: every test here asks as the one launcher the local control plane gives hosted agents
to, and a machine that launcher reported once and leaves out of a later report is an exit. Two
tests asking at once would each end the machines the other reported.
"""
from __future__ import annotations

import json
import os
import uuid

import pytest
import requests

from support import admin
from support.api import API
from support.wait import eventually

pytestmark = pytest.mark.exclusive

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


@pytest.fixture(scope="module")
def with_repo(api: API, sandbox_repository: str) -> API:
    """The staff caller whose platform experiments name the sandbox repository, so agents can be
    hosted in them. It works in the default org, which has clusters to run jobs on."""
    caller = api.as_caller(api.token, api.org_id, api.project_id)
    caller.repository = sandbox_repository
    return caller


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
    own = {i["env"]["AGENT_ID"]: i for i in desired["instances"]
           if i["env"]["PLATFORM_EXPERIMENT_ID"] == pe_id and "AGENT_ID" in i["env"]}
    return own, set(desired["ending"])


def coordinator_for(api: API, pe_id: str, observed: list[dict] | None = None) -> tuple[dict | None, set[str]]:
    """The instance the launcher is asked to keep for this platform experiment's coordinator, and
    the names of the ending ones."""
    desired = api.launcher_desired(LAUNCHER, observed)
    own = [i for i in desired["instances"] if i["env"]["PLATFORM_EXPERIMENT_ID"] == pe_id and "AGENT_ID" not in i["env"]]
    assert len(own) <= 1, f"more than one coordinator instance for {pe_id}: {[i['name'] for i in own]}"
    return (own[0] if own else None), set(desired["ending"])


def _hosted_run(project: API, name: str, **more) -> requests.Response:
    body = {
        "project_id": project.project_id, "name": name, "description": name, "budget_accelerator_hours": 1.0, "max_agents": 5,
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}],
        "starts_at": "0001-01-01T00:00:00Z", "ends_at": "2099-01-01T00:00:00Z",
        **({"repository": project.repository} if project.repository else {}), **more,
    }
    return project.post("/platform-experiments", body)


def roster(api: API, pe_id: str) -> dict[str, dict]:
    return {a["agent_id"]: a for a in api.agents(pe_id)}


def test_an_agent_is_hosted_only_in_a_platform_experiment_that_names_a_repository(api: API, run_id: str):
    project = api
    pe_id = project.create_platform_experiment(f"hosted-no-repo-{run_id}", 1.0, 5)
    try:
        r = project.post(f"/platform-experiments/{pe_id}/agents", {"hosted": True})
        assert r.status_code == 409 and r.json()["reason"] == "no_repository", f"hosted with no repository: {r.status_code} {r.text}"
        assert "names no repository" in r.json()["message"], r.text
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
    assert all(a["instance"] == {"state": "requested", "restarts": 0, "stopped": False} for a in hosted), hosted
    assert [a["name"] for a in hosted] == [f"hosted-{run_id}"] * 2
    r = api.post(f"/platform-experiments/{pe_id}/agents", {"count": 2})
    assert r.status_code == 422, f"a request that still asks for several agents: {r.status_code} {r.text}"
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
    r = as_hosted.post(f"/platform-experiments/{pe_id}/agents", {})
    assert r.status_code == 403, f"an agent adding agents: {r.status_code} {r.text}"
    stranger = api.as_caller(admin.token(f"agent:{pe_id}/ag-{uuid.uuid4().hex}"))
    r = stranger.get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 401, f"a token for an agent that never joined: {r.status_code} {r.text}"
    r = api.post(f"/platform-experiments/{pe_id}/signup", {"agent_id": own_id})
    assert r.status_code in (404, 405), f"the old signup path still answers: {r.status_code} {r.text}"

    # Who may read a launcher's list: that launcher and nobody it launched.
    as_launcher = api.as_caller(admin.add_launcher(LAUNCHER))
    assert sorted(desired_for(as_launcher, pe_id)[0]) == sorted(hosted_ids)
    question = api.launcher_question()
    r = as_launcher.post(f"/internal/launchers/other-{run_id}/reconcile", question)
    assert r.status_code == 403, f"a launcher asking for another's list: {r.status_code} {r.text}"
    r = as_launcher.get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 403, f"a launcher token on the public API: {r.status_code} {r.text}"
    unknown = api.as_caller(admin.token(f"launcher:nobody-{run_id}"))
    r = unknown.post(f"/internal/launchers/nobody-{run_id}/reconcile", question)
    assert r.status_code == 401, f"a launcher nobody registered: {r.status_code} {r.text}"
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
    r = api.post(f"/platform-experiments/{pe_id}/agents", {"hosted": True})
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
    left = roster(api, pe_id)[kept]["instance"]
    assert (left["name"], left["restarts"], left["stopped"]) == (kept_name, 0, False) and "end_reason" not in left, left


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
    kept = roster(api, pe_id)[steady]["instance"]
    assert (kept["name"], kept["restarts"], kept["stopped"]) == (steady_name, 0, False) and kept["seen_at"], kept
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

    # Reported as a launcher reports them, so that none is given up on as never started.
    up = [{"name": before[agent]["name"], "state": "running"} for agent in agents]
    instances, ending = desired_for(api, pe_id, up)
    assert set(instances) == survivors, f"running: {sorted(instances)}, survivors: {sorted(survivors)}"
    for agent in cut:
        assert before[agent]["name"] in ending, f"cut agent {agent} is not left to end"
    for agent in survivors:
        assert instances[agent] == before[agent], f"the survivor {agent} would be replaced by the cut"

    api.close_platform_experiment(pe_id)
    instances, ending = desired_for(api, pe_id, up)
    assert not instances
    for agent in survivors:
        assert before[agent]["name"] in ending, f"{agent} is not left to end after the close"


def test_a_coordinator_is_hosted_only_in_a_project_with_a_repository(api: API, run_id: str):
    project = api
    r = _hosted_run(project, f"coordinated-no-repo-{run_id}", coordinator_hosted=True)
    assert r.status_code == 409 and r.json()["reason"] == "no_repository", f"{r.status_code} {r.text}"
    r = _hosted_run(project, f"coordinated-no-repo-{run_id}", coordinator_model="some-model")
    assert r.status_code == 400, f"a model with no coordinator: {r.status_code} {r.text}"


def test_a_hosted_coordinator_and_hosted_agents_each_get_a_machine_the_launcher_keeps(with_repo: API, run_id: str):
    r = _hosted_run(with_repo, f"coordinated-{run_id}", coordinator_hosted=True)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    try:
        assert r.json()["coordinator_hosted"] is True and r.json()["signup_count"] == 0, r.json()
        with_repo.add_agents(pe_id, count=2, hosted=True)
        pe = with_repo.get_json(f"/platform-experiments/{pe_id}")
        assert pe["coordinator"]["launcher"] == LAUNCHER and pe["coordinator"]["model"], pe.get("coordinator")
        assert pe["coordinator"]["instance"]["restarts"] == 0 and not pe["coordinator"]["instance"]["stopped"], pe["coordinator"]

        # The coordinator is no agent: the roster and the quotas hold the two agents and nothing else.
        agents = roster(with_repo, pe_id)
        assert len(agents) == 2 and all(a["hosted"] and a["launcher"] == LAUNCHER for a in agents.values()), agents
        assert {q["agent_id"] for q in with_repo.platform_experiment_quotas(pe_id)} <= set(agents), "the coordinator holds a quota"

        own, _ = desired_for(with_repo, pe_id)
        assert set(own) == set(agents), f"agents asked for: {set(own)}, joined: {set(agents)}"
        coordinator, _ = coordinator_for(with_repo, pe_id)
        assert coordinator is not None, "the launcher is not asked for the coordinator of an open platform experiment"
        env = coordinator["env"]
        assert env["AGENT_FLAVOR"] == "coordinator" and env["AGENT_MODEL"] == pe["coordinator"]["model"], env
        assert set(env) == {"API_URL", "PLATFORM_EXPERIMENT_ID", "AGENT_FLAVOR", "AGENT_MODEL", "HL_TOKEN"}, set(env)
        assert env["HL_TOKEN"] not in coordinator["name"] and coordinator["name"].count("-") == 2, coordinator["name"]
        assert with_repo.get_json(f"/platform-experiments/{pe_id}")["coordinator"]["instance"]["name"] == coordinator["name"]

        # Its token coordinates this platform experiment: it reads the roster and changes the brief.
        as_coordinator = with_repo.as_caller(env["HL_TOKEN"], with_repo.org_id, with_repo.project_id)
        assert len(as_coordinator.get_json(f"/platform-experiments/{pe_id}/agents")["agents"]) == 2
        changed = {key: pe[key] for key in ("name", "budget_accelerator_hours", "max_agents", "metrics",
                                            "report_interval_seconds", "starts_at", "ends_at")}
        changed["description"] = "the coordinator's brief"
        put = as_coordinator.put(f"/platform-experiments/{pe_id}", changed)
        assert put.status_code == 200, f"the coordinator updating the description: {put.status_code} {put.text}"
        assert with_repo.get_json(f"/platform-experiments/{pe_id}")["description"] == "the coordinator's brief"

        # Its findings are the platform experiment's summary, written through hl with no agent id.
        as_coordinator.hl("platform-experiments", "summary", pe_id, "--text", "what the run found so far")
        assert with_repo.get_json(f"/platform-experiments/{pe_id}")["summary"] == "what the run found so far"
        refused = as_coordinator.hl_expect("hypothesis", "comment", "hyp-any", "--text", "t")
        assert refused.returncode == 1 and "no agent" in refused.stderr, f"a coordinator commenting as nobody: {refused.stderr}"

        # Its machine exits: recorded on the platform experiment, asked for again under a new name.
        first = coordinator["name"]
        again, _ = coordinator_for(with_repo, pe_id, [{"name": first, "state": "exited", "log": "boom\nSystemExit: the reason"}])
        assert again is not None and again["name"] != first, f"after an exit the coordinator is asked for as {again}"
        instance = with_repo.get_json(f"/platform-experiments/{pe_id}")["coordinator"]["instance"]
        assert instance["restarts"] == 1 and "SystemExit: the reason" in instance["exit_log"] and not instance["stopped"], instance
        assert set(desired_for(with_repo, pe_id)[0]) == set(agents), "the agents' machines changed with the coordinator's exit"

        # Started, it is kept. Closed, it is told to end and is asked for no more.
        with_repo.start_platform_experiment(pe_id)
        kept, _ = coordinator_for(with_repo, pe_id, [{"name": again["name"], "state": "running"}])
        assert kept is not None and kept["name"] == again["name"], f"after the start: {kept}"
        with_repo.close_platform_experiment(pe_id)
        gone, ending = coordinator_for(with_repo, pe_id, [{"name": again["name"], "state": "running"}])
        assert gone is None and again["name"] in ending, f"after the close: instance {gone}, ending {ending}"

        # After the close its token still reads and still writes the summary, and nothing else.
        as_coordinator.hl("platform-experiments", "summary", pe_id, "--text", "the final summary")
        assert as_coordinator.get_json(f"/platform-experiments/{pe_id}")["summary"] == "the final summary"
        late = as_coordinator.put(f"/platform-experiments/{pe_id}", changed)
        assert late.status_code in (403, 409, 400), f"a description update after the close: {late.status_code} {late.text}"
    finally:
        with_repo.close_platform_experiment(pe_id)


def test_a_coordinators_machine_that_keeps_exiting_is_stopped_and_shown(with_repo: API, run_id: str):
    r = _hosted_run(with_repo, f"coordinator-crashing-{run_id}", coordinator_hosted=True)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    try:
        seen: set[str] = set()
        coordinator, _ = coordinator_for(with_repo, pe_id)
        for exit_number in range(1, MAX_RESTARTS + 2):
            assert coordinator is not None and coordinator["name"] not in seen, f"exit {exit_number}: {coordinator}"
            seen.add(coordinator["name"])
            coordinator, _ = coordinator_for(with_repo, pe_id, [{"name": coordinator["name"], "state": "exited", "log": f"exit {exit_number}"}])
        assert coordinator is None, f"after {MAX_RESTARTS} restarts the coordinator is still asked for: {coordinator}"
        instance = with_repo.get_json(f"/platform-experiments/{pe_id}")["coordinator"]["instance"]
        assert instance["stopped"] and instance["restarts"] == MAX_RESTARTS and instance["exit_log"] == f"exit {MAX_RESTARTS + 1}", instance
    finally:
        with_repo.close_platform_experiment(pe_id)


def test_a_coordinator_closes_its_platform_experiment_through_hl(with_repo: API, run_id: str):
    r = _hosted_run(with_repo, f"coordinator-closing-{run_id}", coordinator_hosted=True)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    try:
        coordinator, _ = coordinator_for(with_repo, pe_id)
        assert coordinator is not None
        as_coordinator = with_repo.as_caller(coordinator["env"]["HL_TOKEN"], with_repo.org_id, with_repo.project_id)
        # A platform experiment starts with at least one agent in it.
        with_repo.add_agents(pe_id)
        with_repo.start_platform_experiment(pe_id)
        as_coordinator.hl("platform-experiments", "summary", pe_id, "--text", "why it is closed")
        as_coordinator.hl("platform-experiments", "close", pe_id)
        pe = with_repo.get_json(f"/platform-experiments/{pe_id}")
        assert pe["status"] == "closed" and pe["summary"] == "why it is closed", pe
    finally:
        with_repo.close_platform_experiment(pe_id)


def test_creating_a_platform_experiment_adds_no_agent(with_repo: API, run_id: str):
    name = f"hosted-with-agents-{run_id}"
    r = _hosted_run(with_repo, name, hosted_agents=3)
    assert r.status_code == 422, f"a create that asks for agents: {r.status_code} {r.text}"
    listed = with_repo.get_json(f"/platform-experiments?q={name}&limit=50")
    assert not [pe for pe in listed if pe["name"] == name], f"a refused create left a platform experiment: {listed}"


def test_a_machine_never_reported_costs_nothing_inside_the_boot_limit_and_one_reported_and_gone_is_an_exit(with_repo: API, hosted_pe, run_id: str):
    """Past the boot limit (agents.boot_limit_seconds, minutes on the local stack) a machine never
    reported or still starting spends a restart, which the handler's and the store's own tests
    follow with a clock of their own."""
    api = with_repo
    pe_id = hosted_pe("hosted-seen-gone", 1.0, 5)
    agent_id = api.add_agents(pe_id, hosted=True)[0]["agent_id"]
    name = desired_for(api, pe_id)[0][agent_id]["name"]

    # A create that failed is said by nobody: the name is asked for again and nothing is spent.
    for _ in range(2):
        assert desired_for(api, pe_id)[0][agent_id]["name"] == name
    instance = roster(api, pe_id)[agent_id]["instance"]
    assert instance["restarts"] == 0 and not instance["stopped"] and "seen_at" not in instance, instance

    # Reported, it is kept for as long as it is reported.
    for state in ("starting", "running"):
        assert desired_for(api, pe_id, [{"name": name, "state": state}])[0][agent_id]["name"] == name
    instance = roster(api, pe_id)[agent_id]["instance"]
    assert instance["seen_at"] and instance["restarts"] == 0, instance

    # Reported before and in no report now: the provider took it, which is an exit.
    again = desired_for(api, pe_id)[0][agent_id]["name"]
    assert again != name, "a machine that was running and is gone is still asked for under its name"
    instance = roster(api, pe_id)[agent_id]["instance"]
    assert instance["restarts"] == 1 and "no longer lists it" in instance["exit_log"] and "seen_at" not in instance, instance
    assert instance["end_reason"] == "gone" and instance["state"] == "requested", instance


def test_a_running_machines_progress_is_shown_with_its_state(with_repo: API, hosted_pe, run_id: str):
    """A machine whose marker stands still for agents.progress_limit_seconds (minutes on the local
    stack) is ended as silent, which the handler's and the store's own tests follow with a clock
    of their own, and test_real_providers.py with a real harness."""
    api = with_repo
    pe_id = hosted_pe("hosted-progress", 1.0, 5)
    agent_id = api.add_agents(pe_id, hosted=True)[0]["agent_id"]
    name = desired_for(api, pe_id)[0][agent_id]["name"]
    assert roster(api, pe_id)[agent_id]["instance"]["state"] == "requested"

    desired_for(api, pe_id, [{"name": name, "state": "starting"}])
    instance = roster(api, pe_id)[agent_id]["instance"]
    assert instance["state"] == "starting" and "progress_at" not in instance, instance

    # Running with output that could not be read, then with no marker in it: no progress yet.
    for observed in ({"name": name, "state": "running"}, {"name": name, "state": "running", "progress": ""}):
        desired_for(api, pe_id, [observed])
        instance = roster(api, pe_id)[agent_id]["instance"]
        assert instance["state"] == "running" and "progress_at" not in instance, instance

    desired_for(api, pe_id, [{"name": name, "state": "running", "progress": "1"}])
    first = roster(api, pe_id)[agent_id]["instance"]["progress_at"]
    assert first, "a first marker left no progress time"
    # The same marker again, and a new one moments later, write nothing more.
    for marker in ("1", "2"):
        assert desired_for(api, pe_id, [{"name": name, "state": "running", "progress": marker}])[0][agent_id]["name"] == name
        assert roster(api, pe_id)[agent_id]["instance"]["progress_at"] == first

    r = api.post(f"/internal/launchers/{LAUNCHER}/reconcile", api.launcher_question([{"name": name, "state": "starting", "progress": "3"}]))
    assert r.status_code == 400, f"progress of a machine that is not running: {r.status_code} {r.text}"


def test_an_agent_and_a_coordinator_keep_their_own_session_and_nobody_else_reads_it(with_repo: API, run_id: str):
    api = with_repo
    r = _hosted_run(api, f"hosted-sessions-{run_id}", coordinator_hosted=True)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    try:
        _sessions_are_their_owners(api, pe_id)
    finally:
        api.close_platform_experiment(pe_id)


def _sessions_are_their_owners(api: API, pe_id: str) -> None:
    first, second = (api.add_agents(pe_id, hosted=True)[0]["agent_id"] for _ in range(2))
    instances, _ = desired_for(api, pe_id)
    coordinator, _ = coordinator_for(api, pe_id)
    tokens = {first: instances[first]["env"]["HL_TOKEN"], second: instances[second]["env"]["HL_TOKEN"],
              "coordinator": coordinator["env"]["HL_TOKEN"]}
    paths = {first: f"/platform-experiments/{pe_id}/agents/{first}/session",
             second: f"/platform-experiments/{pe_id}/agents/{second}/session",
             "coordinator": f"/platform-experiments/{pe_id}/coordinator/session"}

    def call(method: str, who: str, whose: str, body: bytes | None = None) -> requests.Response:
        headers = {"Authorization": f"Bearer {tokens[who]}", "Content-Type": "application/octet-stream"}
        return requests.request(method, api._url(paths[whose]), headers=headers, data=body, timeout=30)

    for who in tokens:
        assert call("GET", who, who).status_code == 404, f"{who} has a session before it stored one"
        snapshot = b"\x1f\x8b\x00" + who.encode() + os.urandom(64)
        stored = call("PUT", who, who, snapshot)
        assert stored.status_code == 200 and stored.json()["size_bytes"] == len(snapshot), f"{who}: {stored.status_code} {stored.text}"
        read = call("GET", who, who)
        assert read.status_code == 200 and read.content == snapshot, f"{who} read back something else"
        replaced = snapshot + b"a second turn"
        assert call("PUT", who, who, replaced).status_code == 200
        assert call("GET", who, who).content == replaced

    for who in tokens:
        for whose in tokens:
            if who != whose:
                for method, body in (("GET", None), ("PUT", b"overwritten")):
                    r = call(method, who, whose, body)
                    assert r.status_code == 403, f"{who} {method} the session of {whose}: {r.status_code} {r.text}"
    for whose in tokens:
        for method, body in (("GET", None), ("PUT", b"overwritten")):
            r = api.session.request(method, api._url(paths[whose]), data=body, timeout=30)
            assert r.status_code == 403, f"the org's own caller {method} the session of {whose}: {r.status_code} {r.text}"
        assert b"overwritten" not in call("GET", whose, whose).content

    assert call("PUT", first, first, b"").status_code == 400
    assert call("PUT", first, first, b"x" * ((32 << 20) + 1)).status_code == 413


def test_a_launcher_is_answered_while_it_is_registered_and_refused_when_its_pass_cannot_fit(api: API, run_id: str):
    name = f"e2e{uuid.uuid4().hex[:10]}"
    token = admin.token(f"launcher:{name}")
    r = api.as_caller(token).post(f"/internal/launchers/{name}/reconcile", api.launcher_question())
    assert r.status_code == 401, f"a launcher nobody registered: {r.status_code} {r.text}"

    assert admin.add_launcher(name) == token, "the token printed for a launcher is not the one its scope derives"
    try:
        as_launcher = api.as_caller(token)
        # No agent is given to it: an empty list, on which a launcher destroys what it holds.
        desired = as_launcher.post_json(f"/internal/launchers/{name}/reconcile", api.launcher_question())
        assert (desired["instances"], desired["ending"]) == ([], []), desired

        # Ten new machines an answer, a minute between two creates: no pass ends inside any boot
        # limit a deployment would set, and the refusal says the arithmetic.
        slow = {**api.launcher_question(), "create_pause_milliseconds": 3_600_000}
        r = as_launcher.post(f"/internal/launchers/{name}/reconcile", slow)
        assert r.status_code == 409 and "boot_limit_seconds" in r.text and "create_pause" in r.text, f"{r.status_code} {r.text}"
        unpaced = {"launch_digest": API.LAUNCH_DIGEST, "instances": []}
        r = as_launcher.post(f"/internal/launchers/{name}/reconcile", unpaced)
        assert r.status_code in (400, 422), f"a question that does not say how the launcher paces itself: {r.status_code} {r.text}"
    finally:
        admin.remove_launcher(name)
    r = api.as_caller(token).post(f"/internal/launchers/{name}/reconcile", api.launcher_question())
    assert r.status_code == 401, f"a launcher that was removed: {r.status_code} {r.text}"


def test_the_machines_of_one_platform_experiment_are_bounded_whatever_their_role(with_repo: API, run_id: str):
    r = _hosted_run(with_repo, f"hosted-bound-{run_id}", coordinator_hosted=True, max_agents=1)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id, bound = r.json()["id"], r.json()["max_hosted"]
    try:
        assert bound >= 2, f"max_hosted = {bound}: the local settings leave room for no agent beside the coordinator"
        # max_agents counts competitors, so reviewers are added until the machines run out.
        for _ in range(bound - 1):
            r = with_repo.post(f"/platform-experiments/{pe_id}/agents", {"hosted": True, "role": "reviewer"})
            assert r.status_code == 201, f"a hosted reviewer below the bound: {r.status_code} {r.text}"
        for body in ({"hosted": True, "role": "reviewer"}, {"hosted": True, "role": "baseline"}, {"hosted": True}):
            r = with_repo.post(f"/platform-experiments/{pe_id}/agents", body)
            assert r.status_code == 400 and r.json()["reason"] == "max_hosted_reached", f"{body} at the bound: {r.status_code} {r.text}"
        r = with_repo.post(f"/platform-experiments/{pe_id}/agents", {"role": "reviewer"})
        assert r.status_code == 201, f"a self-run reviewer at the bound: {r.status_code} {r.text}"
        assert len([a for a in roster(with_repo, pe_id).values() if a["hosted"]]) == bound - 1
    finally:
        with_repo.close_platform_experiment(pe_id)


def test_a_coordinators_token_changes_the_description_only_and_acts_as_no_agent(with_repo: API, run_id: str):
    r = _hosted_run(with_repo, f"coordinator-bounds-{run_id}", coordinator_hosted=True)
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe = r.json()
    pe_id = pe["id"]
    try:
        with_repo.add_agents(pe_id, hosted=True)
        coordinator, _ = coordinator_for(with_repo, pe_id)
        agent_id = next(iter(roster(with_repo, pe_id)))
        as_coordinator = with_repo.as_caller(coordinator["env"]["HL_TOKEN"], with_repo.org_id, with_repo.project_id)
        live = {key: pe[key] for key in ("name", "description", "budget_accelerator_hours", "max_agents", "metrics",
                                         "report_interval_seconds", "starts_at", "ends_at")}
        for field, value in (("budget_accelerator_hours", 1000.0), ("max_agents", 50), ("ends_at", "2199-01-01T00:00:00Z"),
                             ("stages", [{"length_pct": 50, "evict_pct": 50}, {"length_pct": 50, "evict_pct": 0}])):
            r = as_coordinator.put(f"/platform-experiments/{pe_id}", {**live, field: value})
            assert r.status_code == 403 and field in r.text, f"a coordinator changing {field}: {r.status_code} {r.text}"
        after = with_repo.get_json(f"/platform-experiments/{pe_id}")
        changed = {key: (pe[key], after[key]) for key in (*live, "stages") if after[key] != pe[key]}
        assert not changed, f"a refused update changed {changed}"

        # It acts as no agent, with or without one named.
        r = as_coordinator.post(f"/platform-experiments/{pe_id}/data-key", {"agent_id": agent_id})
        assert r.status_code == 403, f"a coordinator fetching an agent's data key: {r.status_code} {r.text}"
        r = as_coordinator.post("/donations", {"agent_id": agent_id, "platform_experiment_id": pe_id, "credits_want": 1, "reason": "r"})
        assert r.status_code == 403, f"a coordinator asking for quota as an agent: {r.status_code} {r.text}"
        refused = as_coordinator.hl_expect("--agent", agent_id, "hypothesis", "comment", "hyp-any", "--text", "t")
        assert refused.returncode == 1 and "coordinator" in refused.stderr, f"hl --agent under a coordinator's token: {refused.stderr}"
    finally:
        with_repo.close_platform_experiment(pe_id)
