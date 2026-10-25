"""Agents join a platform experiment through POST /platform-experiments/{id}/agents and in no
other way, each acts as itself alone with its own token, and a coordinator's token runs its
platform experiment without acting as any agent. The agents here are run by their owner; the
agents the platform runs in a sandbox it rents are covered by test_real_providers.py and by
test_the_sandboxes_of_one_platform_experiment_are_bounded below, which rent and are opt-in.
"""
from __future__ import annotations

import os
import uuid

import pytest
import requests

from support import admin
from support.api import API

pytestmark = pytest.mark.parallel


def _run(api: API, name: str, **more) -> requests.Response:
    return api.post("/platform-experiments", {
        "project_id": api.project_id, "name": name, "description": name, "budget_accelerator_hours": 1.0, "max_agents": 5,
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "duration_hours": 24, **more,
    })


@pytest.fixture
def pe_id(api: API, run_id: str):
    r = _run(api, f"agents-{uuid.uuid4().hex[:8]}-{run_id}")
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    api.record_platform_experiment(pe_id)
    yield pe_id
    api.close_platform_experiment(pe_id)


def test_agents_join_and_leave_through_the_api_and_act_only_as_themselves(api: API, pe_id: str, run_id: str):
    # An agent runs the default type on its type's model unless it names its own.
    own = api.add_agents(pe_id)[0]
    assert not own["in_agentsandbox"] and own["token"].startswith("hls_") and "instance" not in own, own
    assert own["agent_type"] == "generalist" and not own.get("model") and own["agent_id"].startswith("ag-"), own
    served = api.get_json("/models")["coordinator_model"]
    named = api.add_agents(pe_id, name=f"named-{run_id}", agent_type="data-search", model=served)[0]
    assert (named["agent_type"], named["model"]) == ("data-search", served), named

    for body, status, reason in (
        ({"agent_type": "no-such-type"}, 422, None),
        ({"hyperparameters": {"blob": "x" * 5000}}, 422, None),
        ({"count": 2}, 422, None),
        ({"name": f"named-{run_id}"}, 409, "name_taken"),
    ):
        r = api.post(f"/platform-experiments/{pe_id}/agents", body)
        assert r.status_code == status and (reason is None or r.json()["reason"] == reason), f"{body}: {r.status_code} {r.text}"
        assert r.json()["message"], r.text
    listed = api.agents(pe_id)
    assert [a["agent_id"] for a in listed] == [own["agent_id"], named["agent_id"]], listed
    assert all("token" not in a for a in listed), "the roster lists a token"
    assert api.signup_role(pe_id, own["agent_id"]) == (200, "executor")

    # An agent adds nobody, names no other agent, and a token for an agent nobody added is refused.
    as_own = api.as_caller(own["token"])
    assert as_own.post(f"/platform-experiments/{pe_id}/agents", {}).status_code == 403
    r = as_own.post_hypothesis(pe_id, "as another agent", agent_id=named["agent_id"])
    assert r.status_code == 403, f"an agent's token naming another agent: {r.status_code} {r.text}"
    stranger = api.as_caller(admin.token(f"agent:{pe_id}/ag-{uuid.uuid4().hex}"))
    assert stranger.get(f"/platform-experiments/{pe_id}").status_code == 401
    # Only the agent itself finishes.
    r = api.post(f"/platform-experiments/{pe_id}/agents/{own['agent_id']}/finish", {"reason": "said by its owner"})
    assert r.status_code == 403, f"an owner finishing an agent: {r.status_code} {r.text}"

    # Removed before the start, its token reaches nothing; it joins again by its id, with that token.
    removed = api.remove_agent(pe_id, own["agent_id"])
    assert removed.status_code == 204 and not removed.content, f"remove: {removed.status_code} {removed.text}"
    assert api.signup_role(pe_id, own["agent_id"]) == (404, None)
    assert as_own.get(f"/platform-experiments/{pe_id}").status_code == 401, "a removed agent's token still works"
    again = api.post_json(f"/platform-experiments/{pe_id}/agents", {"agent_id": own["agent_id"]})["agents"][0]
    assert again["agent_id"] == own["agent_id"] and again["token"] == own["token"], again
    assert as_own.signup_role(pe_id, own["agent_id"]) == (200, "executor")

    # Started, nobody is removed.
    api.declare_baseline_and_wait_running(pe_id)
    r = api.remove_agent(pe_id, named["agent_id"])
    assert (r.status_code, r.json()["reason"]) == (409, "experiment_started"), f"remove after start: {r.status_code} {r.text}"


def test_creating_a_platform_experiment_adds_no_agent_and_names_no_model_without_a_coordinator(api: API, run_id: str):
    name = f"with-agents-{run_id}"
    r = _run(api, name, sandboxed_agents=3)
    assert r.status_code == 422, f"a create that asks for agents: {r.status_code} {r.text}"
    assert not [pe for pe in api.get_json("/platform-experiments", params={"q": name, "limit": 50}) if pe["name"] == name]
    r = _run(api, f"model-{run_id}", coordinator_model=api.get_json("/models")["coordinator_model"])
    assert r.status_code == 422, f"a model with no coordinator: {r.status_code} {r.text}"


def test_a_coordinator_closes_its_platform_experiment_through_hl(api: API, pe_id: str):
    as_coordinator = api.as_caller(admin.token(f"coordinator:{pe_id}"), api.org_id, api.project_id)
    executor = api.add_agents(pe_id)[0]
    api.declare_baseline_and_wait_running(pe_id)
    # Its only executor finishes, with time and quota left, so it says why it stops: nobody is
    # left to submit a job, and with no coordinator the platform runs, the run waits for its owner.
    api.as_caller(executor["token"], api.org_id, api.project_id).hl(
        "platform-experiments", "agents", "finish", "--reason", "the test has no idea left", "--because", "nothing_left")
    done = api.get_json(f"/platform-experiments/{pe_id}")
    assert (done["status"], done["status_reason"]["reason"], done["status_reason"]["waits_for"]) == ("running", "executors_done", "owner"), done
    as_coordinator.hl("platform-experiments", "summary", "--platform-experiment", pe_id, "--text", "why it is closed")
    as_coordinator.hl("platform-experiments", "close", "--platform-experiment", pe_id)
    closed = api.get_json(f"/platform-experiments/{pe_id}")
    assert closed["status"] == "closed" and closed["summary"].startswith("why it is closed"), closed


def test_a_coordinators_token_changes_the_description_only_and_acts_as_no_agent(api: API, pe_id: str):
    agent_id = api.add_agents(pe_id)[0]["agent_id"]
    before = api.get_json(f"/platform-experiments/{pe_id}")
    as_coordinator = api.as_caller(admin.token(f"coordinator:{pe_id}"), api.org_id, api.project_id)
    for field, value in (("budget_accelerator_hours", 1000.0), ("max_agents", 50), ("duration_hours", 48)):
        r = as_coordinator.patch(f"/platform-experiments/{pe_id}", {"description": "more to spend", field: value})
        assert r.status_code == 403 and field in r.text, f"a coordinator changing {field}: {r.status_code} {r.text}"
    after = api.get_json(f"/platform-experiments/{pe_id}")
    kept = ("name", "description", "budget_accelerator_hours", "max_agents", "metrics", "report_interval_seconds", "duration_hours", "stages")
    assert {k: after[k] for k in kept} == {k: before[k] for k in kept}, "a refused update changed the platform experiment"
    assert as_coordinator.patch(f"/platform-experiments/{pe_id}", {"description": "a sharper brief"}).status_code == 200

    # It spends as no agent: it submits no job.
    api.declare_baseline_and_wait_running(pe_id)
    r = as_coordinator.post_experiment_body(api.submission_body_for_id(api._job_id(), pe_id, agent_id))
    assert (r.status_code, r.json()["reason"]) == (403, "forbidden"), f"a coordinator submitting an agent's job: {r.status_code} {r.text}"


def test_an_agent_and_a_coordinator_keep_their_own_session_and_only_their_org_reads_it(api: API, pe_id: str):
    first, second = api.add_agents(pe_id, 2)
    tokens = {first["agent_id"]: first["token"], second["agent_id"]: second["token"], "coordinator": admin.token(f"coordinator:{pe_id}")}
    paths = {first["agent_id"]: f"/platform-experiments/{pe_id}/agents/{first['agent_id']}/session",
             second["agent_id"]: f"/platform-experiments/{pe_id}/agents/{second['agent_id']}/session",
             "coordinator": f"/platform-experiments/{pe_id}/coordinator/session"}

    def call(method: str, who: str, whose: str, body: bytes | None = None) -> requests.Response:
        headers = {"Authorization": f"Bearer {tokens[who]}", "Content-Type": "application/octet-stream"}
        return requests.request(method, api._url(paths[whose]), headers=headers, data=body, timeout=30)

    for who in tokens:
        assert call("GET", who, who).status_code == 404, f"{who} has a session before it stored one"
        snapshot = b"\x1f\x8b\x00" + who.encode() + os.urandom(64)
        stored = call("PUT", who, who, snapshot)
        assert stored.status_code == 200 and stored.json()["size_bytes"] == len(snapshot), f"{who}: {stored.status_code} {stored.text}"
        assert call("GET", who, who).content == snapshot, f"{who} read back something else"
        assert call("PUT", who, who, snapshot + b"a second turn").status_code == 200
        assert call("GET", who, who).content == snapshot + b"a second turn"

    for who in tokens:
        for whose in tokens:
            if who != whose:
                for method, body in (("GET", None), ("PUT", b"overwritten")):
                    r = call(method, who, whose, body)
                    assert r.status_code == 403, f"{who} {method} the session of {whose}: {r.status_code} {r.text}"
    for whose in tokens:
        r = api.session.put(api._url(paths[whose]), data=b"overwritten", timeout=30)
        assert r.status_code == 403, f"the org's own caller PUT the session of {whose}: {r.status_code} {r.text}"
        stored = call("GET", whose, whose).content
        assert b"overwritten" not in stored
        r = api.session.get(api._url(paths[whose]), timeout=30)
        assert r.status_code == 200 and r.content == stored, f"the org's own caller GET the session of {whose}: {r.status_code}"

    empty = call("PUT", first["agent_id"], first["agent_id"], b"")
    assert (empty.status_code, empty.json()["reason"]) == (422, "invalid_request"), f"an empty session: {empty.status_code} {empty.text}"
    assert call("PUT", first["agent_id"], first["agent_id"], b"x" * ((32 << 20) + 1)).status_code == 413


@pytest.mark.real_providers
@pytest.mark.exclusive
def test_the_sandboxes_of_one_platform_experiment_are_bounded_whatever_their_role(api: API, sandbox_repository: str, run_id: str):
    """Each sandboxed agent added here is rented at the sandbox provider, so this runs only with
    make e2e RENT=1."""
    r = _run(api, f"sandboxed-bound-{run_id}", coordinator_in_agentsandbox=True, max_agents=1, repository=sandbox_repository, branch="main")
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id, bound = r.json()["id"], r.json()["max_agentsandboxes"]
    api.record_platform_experiment(pe_id)
    try:
        assert bound >= 3, f"max_agentsandboxes = {bound}: no room for an agent beside the coordinator and the baseline's sandbox"
        # max_agents counts executors, so reviewers are added until the sandboxes run out. One is
        # the coordinator's and one is the baseline agent's the create made.
        for _ in range(bound - 2):
            r = api.post(f"/platform-experiments/{pe_id}/agents", {"in_agentsandbox": True, "role": "reviewer"})
            assert r.status_code == 201, f"a sandboxed reviewer below the bound: {r.status_code} {r.text}"
        for body in ({"in_agentsandbox": True, "role": "reviewer"}, {"in_agentsandbox": True}, {"in_agentsandbox": True, "role": "baseline"}):
            r = api.post(f"/platform-experiments/{pe_id}/agents", body)
            assert (r.status_code, r.json()["reason"]) == (409, "max_agentsandboxes_reached"), f"{body} at the bound: {r.status_code} {r.text}"
        r = api.post(f"/platform-experiments/{pe_id}/agents", {"role": "reviewer"})
        assert r.status_code == 201, f"a self-run reviewer at the bound: {r.status_code} {r.text}"
        assert len([a for a in api.agents(pe_id) if a["in_agentsandbox"]]) == bound - 1
    finally:
        api.close_platform_experiment(pe_id)
