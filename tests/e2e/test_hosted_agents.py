"""Agents the platform runs, end to end against the control plane: they join through
POST /platform-experiments/{id}/agents and in no other way, and the agent-launcher's desired list
follows each of them from open through running to cut and close.

No launcher process and no provider is involved: the list read here is exactly what a launcher is
handed, and what it does with one is pinned by runtime/cloud/internal/agentlaunch's own tests. The
local control plane gives hosted agents to the launcher named in its config (agents.launcher).
API-only, parallel-safe: every assertion is about this test's own platform experiment.
"""
from __future__ import annotations

import json
import uuid

import pytest

from support import admin
from support.wait import eventually

pytestmark = pytest.mark.parallel

LAUNCHER = "main"

STAGES = [
    {"length_pct": 20, "evict_pct": 50},
    {"length_pct": 30, "evict_pct": 25},
    {"length_pct": 50, "evict_pct": 0},
]
JOB_HOURS = 0.0028
RUN_SECONDS = 40
BUDGET = 0.03


def desired_for(api, pe_id: str) -> tuple[dict[str, dict], set[str]]:
    """This platform experiment's part of the launcher's desired state: its running instances by
    agent id, and the names of its ending ones."""
    desired = api.launcher_desired(LAUNCHER)
    assert isinstance(desired["instances"], list) and isinstance(desired["ending"], list), desired.keys()
    own = {i["env"]["AGENT_ID"]: i for i in desired["instances"] if i["env"]["PLATFORM_EXPERIMENT_ID"] == pe_id}
    return own, set(desired["ending"])


def test_agents_join_through_the_api_and_the_launcher_is_told_what_to_run(api, pe, run_id):
    pe_id = pe("hosted-agents", 1.0, 5)

    hosted = api.add_agents(pe_id, count=2, hosted=True, name=f"hosted-{run_id}", flavor="data-search",
                            model="claude-sonnet-5", hyperparameters={"batch_size": 64})
    self_run = api.add_agents(pe_id, flavor="generalist")
    assert len(hosted) == 2 and len(self_run) == 1
    hosted_ids = [a["agent_id"] for a in hosted]
    own_id, own_token = self_run[0]["agent_id"], self_run[0]["token"]
    assert all(a["hosted"] and a["launcher"] == LAUNCHER and "token" not in a for a in hosted), hosted
    assert [a["name"] for a in hosted] == [f"hosted-{run_id}-1", f"hosted-{run_id}-2"]
    assert not self_run[0]["hosted"] and "launcher" not in self_run[0] and own_token.startswith("hls_")
    assert len(set(hosted_ids + [own_id])) == 3 and all(i.startswith("ag-") for i in hosted_ids + [own_id])

    roster = api.agents(pe_id)
    assert [a["agent_id"] for a in roster] == hosted_ids + [own_id]
    assert all("token" not in a for a in roster), "the roster lists a token"
    for agent_id in hosted_ids + [own_id]:
        assert api.signup_role(pe_id, agent_id) == (200, "competitor")

    instances, ending = desired_for(api, pe_id)
    assert sorted(instances) == sorted(hosted_ids), f"the launcher is told to run {sorted(instances)}"
    names = {i["name"] for i in instances.values()}
    assert len(names) == 2 and not names & ending
    for agent_id, instance in instances.items():
        env = instance["env"]
        assert set(env) == {"API_URL", "AGENT_ID", "PLATFORM_EXPERIMENT_ID", "AGENT_FLAVOR",
                            "AGENT_HYPERPARAMETERS", "ANTHROPIC_MODEL", "HL_TOKEN"}, sorted(env)
        assert env["AGENT_FLAVOR"] == "data-search" and env["ANTHROPIC_MODEL"] == "claude-sonnet-5"
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
    r = as_launcher.post(f"/internal/launchers/other-{run_id}/reconcile")
    assert r.status_code == 403, f"a launcher asking for another's list: {r.status_code} {r.text}"
    r = as_launcher.get(f"/platform-experiments/{pe_id}")
    assert r.status_code == 403, f"a launcher token on the public API: {r.status_code} {r.text}"
    unknown = api.as_caller(admin.token(f"launcher:nobody-{run_id}"))
    r = unknown.post(f"/internal/launchers/nobody-{run_id}/reconcile")
    assert r.status_code == 401, f"a launcher nothing names: {r.status_code} {r.text}"
    for scope in (f"agent:{pe_id}/{hosted_ids[0]}", f"coordinator:{pe_id}"):
        r = api.as_caller(admin.token(scope)).post(f"/internal/launchers/{LAUNCHER}/reconcile")
        assert r.status_code == 403, f"{scope} reading the launcher's list: {r.status_code} {r.text}"

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

    # Closed: the instance is no longer wanted and is named as ending, so its agent's last turn is
    # not cut short and nothing is started in its place.
    kept_name = instances[kept]["name"]
    api.close_platform_experiment(pe_id)
    instances, ending = desired_for(api, pe_id)
    assert not instances, f"instances of a closed platform experiment: {sorted(instances)}"
    assert kept_name in ending, f"{kept_name} is not left to end: {sorted(ending)}"


def test_a_cut_hosted_agent_is_left_to_end_while_survivors_keep_running(api, pe, run_id, deadline):
    # Six agents and a 50% first cut: above the roster floor for a cut, see test_stage_ladder_cut.
    pe_id = pe("hosted-agents-cut", BUDGET, 6, report_interval_seconds=10, stages=STAGES)
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
