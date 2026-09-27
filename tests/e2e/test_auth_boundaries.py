"""The boundaries inside one org and around one token, over HTTP against the real stack.

test_tenancy.py holds two orgs apart. This file holds what an org shares with itself: two
projects, two agents of one platform experiment, a person signed in with a JWT, tokens around a
close, what a /watch stream delivers and for how long, and the liveness endpoint of
metrics-service.
"""
from __future__ import annotations

import base64
import json
import os
import subprocess
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from urllib.parse import urlsplit, urlunsplit

import pytest
import requests

from support import admin
from support.api import API
from support.wait import eventually

# Exclusive: an org made here owns no cluster that runs anything, so its platform experiment is
# created and started while the default org's clusters are shared with every org
# (API.clusters_shared).
pytestmark = pytest.mark.exclusive

WATCH_SUBPROTOCOL = "hypothesisloop.watch"
# Priced in controlplane/settings/hypothesisloop.yaml and on none of the dev nodes, so a job
# asking for it stays QUEUED: its token stays valid and it holds nothing.
UNAVAILABLE_ACCELERATOR = "nvidia.com/gpu.product=NVIDIA-H200"
# domain.AgentWindDown, in seconds.
AGENT_WIND_DOWN = 120
SAMPLE = {"metric_name": "val_accuracy", "fraction_complete": 0.25, "metric_value": 0.5}


def _status(caller: API, method: str, path: str, body: dict | None = None) -> tuple[int, str]:
    r = caller.session.request(method, caller._url(path), json=body, timeout=30)
    return r.status_code, r.text


def _assert_each(caller: API, who: str, calls: list[tuple[str, str, dict | None, int]]) -> None:
    for method, path, body, want in calls:
        got, text = _status(caller, method, path, body)
        assert got == want, f"{who} {method} {path}: got {got}, want {want}: {text}"


def _platform_experiment_body(name: str, project_id: str) -> dict:
    return {
        "name": name, "description": name, "budget_accelerator_hours": 1, "max_agents": 1, "project_id": project_id,
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "duration_hours": 24,
    }


def _hypothesis_body(pe_id: str, text: str, **origin: str) -> dict:
    return {"platform_experiment_id": pe_id, "text": text, "links": [{"type": "builds_on", "to": pe_id}],
            "distinct_from": [], **origin}


def _claim(label: str) -> str:
    """A hypothesis text no other reads close to: registration holds text that shares words with
    a hypothesis of the same platform experiment, so each carries words of its own."""
    return f"{label} " + " ".join(uuid.uuid4().hex[:12] for _ in range(6))


@dataclass
class Org:
    subject: str
    key: API
    org_id: str
    project_id: str
    marker: str


def _org(api: API, run_id: str, label: str) -> Org:
    marker = f"bound{label}{uuid.uuid4().hex[:10]}"
    subject = f"user-{marker}-{run_id}"
    home = admin.add_user(subject, f"Owner {marker}")
    key = api.as_caller(admin.add_api_key(subject, home["org_id"]), home["org_id"], home["project_id"])
    return Org(subject, key, home["org_id"], home["project_id"], marker)


@dataclass
class Member:
    """One agent of a platform experiment and what is its own there."""
    agent: str
    token: API
    hypothesis_id: str
    job_id: str


def _running_experiment(org: Org, caller: API, run_id: str, agents: int) -> tuple[str, list[Member]]:
    """A started platform experiment in the caller's project with that many agents, each holding
    a hypothesis and a queued job."""
    names = [f"agent-{org.marker}-{uuid.uuid4().hex[:6]}-{run_id}" for _ in range(agents)]
    for name in names:
        caller.register_agent(name)
    with caller.clusters_shared():
        pe_id = caller.create_platform_experiment(f"{org.marker}-{uuid.uuid4().hex[:6]}-{run_id}", 5.0, agents)
        caller.signup_and_start(pe_id, names)
    members = []
    for name in names:
        hypothesis_id = caller.register_hypothesis(pe_id, name, _claim(name))["id"]
        job_id = caller.submit_job(pe_id, name, hyp_id=hypothesis_id, job_overrides={"accelerator_type": UNAVAILABLE_ACCELERATOR})
        members.append(Member(name, caller.as_caller(admin.token(f"agent:{pe_id}/{name}")), hypothesis_id, job_id))
    return pe_id, members


def _close(caller: API, pe_id: str, members: list[Member]) -> None:
    for member in members:
        caller.post(f"/experiments/{member.job_id}/cancel")
    caller.close_platform_experiment(pe_id)


@pytest.fixture(scope="module")
def org(api: API, run_id: str) -> Org:
    return _org(api, run_id, "a")


@pytest.fixture(scope="module")
def shared_experiment(org: Org, run_id: str):
    pe_id, members = _running_experiment(org, org.key, run_id, 2)
    yield pe_id, members
    _close(org.key, pe_id, members)


# ---- two projects of one org -----------------------------------------------------------------


def test_a_project_key_reads_and_writes_nothing_of_its_orgs_other_project(org: Org, run_id: str):
    created = org.key.post(f"/orgs/{org.org_id}/projects", {"name": f"second {org.marker}"})
    assert created.status_code == 201, f"create a second project: {created.status_code} {created.text}"
    second = created.json()["id"]
    in_second = org.key.as_caller(org.key.token, org.org_id, second)
    pe_id, members = _running_experiment(org, in_second, run_id, 1)
    other = members[0]
    bound = org.key.as_caller(admin.add_api_key(org.subject, org.org_id, org.project_id), org.org_id, org.project_id)
    try:
        reads = [
            f"/platform-experiments/{pe_id}", f"/platform-experiments/{pe_id}/quotas", f"/platform-experiments/{pe_id}/agents",
            f"/platform-experiments/{pe_id}/data-usage", f"/platform-experiments/{pe_id}/findings",
            f"/experiments/{other.job_id}", f"/experiments/{other.job_id}/metrics", f"/experiments/{other.job_id}/logs",
            f"/experiments/{other.job_id}/data", f"/hypotheses/{other.hypothesis_id}",
            f"/hypotheses/graph?platform_experiment_id={pe_id}",
        ]
        _assert_each(org.key, "the org's key", [("GET", path, None, 200) for path in reads])
        _assert_each(bound, "the first project's key", [("GET", path, None, 404) for path in reads])
        _assert_each(bound, "the first project's key", [
            ("GET", f"/platform-experiments?project_id={second}", None, 404),
            ("GET", f"/experiments?project_id={second}", None, 404),
            ("POST", "/platform-experiments", _platform_experiment_body("outside", second), 404),
            ("POST", f"/platform-experiments/{pe_id}/close", {"top_results": []}, 404),
            ("POST", f"/platform-experiments/{pe_id}/data-key", {"agent_id": other.agent}, 404),
            ("POST", f"/experiments/{other.job_id}/cancel", None, 404),
            ("POST", f"/experiments/{other.job_id}/summary", {"summary": "s"}, 404),
            ("POST", f"/experiments/{other.job_id}/metrics", SAMPLE, 404),
            ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("from outside"), agent_id=other.agent), 404),
            ("POST", f"/hypotheses/{other.hypothesis_id}/comments", {"agent_id": other.agent, "text": "t"}, 404),
            ("POST", f"/hypotheses/{other.hypothesis_id}/status", {"agent_id": other.agent, "status": "refuted"}, 404),
            ("POST", "/hypotheses/query", {"platform_experiment_id": pe_id, "cypher": "MATCH (h:Hypothesis) RETURN h.id", "params": {}}, 404),
            ("POST", f"/orgs/{org.org_id}/projects", {"name": "a third"}, 403),
            ("POST", f"/orgs/{org.org_id}/api-keys", {"name": "wider"}, 403),
        ])
        for path in ("/platform-experiments?limit=200", "/experiments?limit=200", "/hypotheses?limit=200"):
            listed = bound.get(path)
            assert listed.status_code == 200, f"the first project's key GET {path}: {listed.status_code} {listed.text}"
            for theirs in (pe_id, other.job_id, other.hypothesis_id):
                assert theirs not in listed.text, f"the first project's key GET {path} lists {theirs} of the second project"
        assert org.key.experiment(other.job_id)["status"] == "QUEUED"
        assert org.key.hypothesis(other.hypothesis_id)["status"] == "open"
    finally:
        _close(in_second, pe_id, members)


# ---- two agents of one platform experiment ---------------------------------------------------


def test_an_agent_token_acts_as_its_own_agent_and_on_its_own_work_only(org: Org, shared_experiment):
    pe_id, (mine, theirs) = shared_experiment
    job_as_them = org.key.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", pe_id, theirs.agent)
    job_as_them["metadata"]["hypothesis_id"] = mine.hypothesis_id

    # The job is the agent's, and who submitted it is the key that did. So is the hypothesis.
    key_id = org.key.get_json("/me")["key_id"]
    submitted = org.key.experiment(mine.job_id)
    assert submitted["agent_id"] == mine.agent and (submitted["actor"]["kind"], submitted["actor"]["id"]) == ("key", key_id), submitted
    registered = org.key.hypothesis(mine.hypothesis_id)
    assert registered["agent_id"] == mine.agent and (registered["actor"]["kind"], registered["actor"]["id"]) == ("key", key_id), registered

    # Everything in the platform experiment is the agent's to read.
    _assert_each(mine.token, "an agent", [
        ("GET", f"/experiments/{theirs.job_id}", None, 200),
        ("GET", f"/experiments/{theirs.job_id}/metrics", None, 200),
        ("GET", f"/hypotheses/{theirs.hypothesis_id}", None, 200),
        ("GET", f"/platform-experiments/{pe_id}/quotas/{theirs.agent}", None, 200),
    ])
    _assert_each(mine.token, "an agent", [
        ("POST", f"/experiments/{theirs.job_id}/cancel", None, 403),
        ("POST", f"/experiments/{theirs.job_id}/summary", {"summary": "written by a rival"}, 403),
        ("POST", f"/experiments/{theirs.job_id}/metrics", SAMPLE, 403),
        ("POST", "/experiments", job_as_them, 403),
        ("POST", f"/platform-experiments/{pe_id}/data-key", {"agent_id": theirs.agent}, 403),
        ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("as the rival"), agent_id=theirs.agent), 403),
        ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("as a person"), author="Mallory"), 422),
        ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("as both"), agent_id=mine.agent, author="Mallory"), 422),
        ("POST", f"/hypotheses/{mine.hypothesis_id}/comments", {"author": "Mallory", "text": "as a person"}, 422),
        ("POST", f"/hypotheses/{mine.hypothesis_id}/comments", {"agent_id": theirs.agent, "text": "as the rival"}, 403),
        ("POST", f"/hypotheses/{mine.hypothesis_id}/links", {"author": "Mallory", "type": "related", "to": theirs.hypothesis_id}, 422),
        ("POST", f"/hypotheses/{theirs.hypothesis_id}/links", {"agent_id": mine.agent, "type": "related", "to": mine.hypothesis_id}, 403),
        ("POST", f"/hypotheses/{theirs.hypothesis_id}/status", {"agent_id": theirs.agent, "status": "refuted"}, 403),
        ("POST", f"/hypotheses/{theirs.hypothesis_id}/status", {"agent_id": mine.agent, "status": "refuted"}, 403),
        ("POST", "/platform-experiments", _platform_experiment_body("an agent's own", org.project_id), 403),
        ("POST", f"/platform-experiments/{pe_id}/close", {"top_results": []}, 403),
        ("POST", "/agents", {"id": f"agent-made-by-an-agent-{uuid.uuid4().hex[:6]}"}, 403),
    ])

    assert org.key.experiment(theirs.job_id)["status"] == "QUEUED"
    assert not org.key.metrics(theirs.job_id)
    assert org.key.hypothesis(theirs.hypothesis_id)["status"] == "open"
    refused = ("as the rival", "as a person", "as both")
    left = [h["text"] for h in org.key.hypotheses(pe_id) if h["text"].startswith(refused)]
    assert not left, f"a refused registration left a hypothesis behind: {left}"

    _assert_each(mine.token, "an agent on its own", [
        ("POST", f"/hypotheses/{mine.hypothesis_id}/comments", {"agent_id": mine.agent, "text": "my own note"}, 201),
        ("POST", f"/hypotheses/{mine.hypothesis_id}/comments", {"text": "my own note, naming nobody"}, 201),
        ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("naming nobody")), 201),
        ("POST", f"/experiments/{mine.job_id}/metrics", SAMPLE, 204),
    ])


def test_a_coordinator_token_runs_its_platform_experiment_and_creates_no_other(org: Org, shared_experiment):
    pe_id, (mine, _) = shared_experiment
    coordinator = org.key.as_caller(admin.token(f"coordinator:{pe_id}"))
    _assert_each(coordinator, "a coordinator", [
        ("GET", f"/platform-experiments/{pe_id}", None, 200),
        ("GET", f"/platform-experiments/{pe_id}/agents", None, 200),
        ("POST", f"/hypotheses/{mine.hypothesis_id}/comments", {"agent_id": mine.agent, "text": "for the agent"}, 201),
        ("POST", "/platform-experiments", _platform_experiment_body("a second one", org.project_id), 403),
        ("GET", "/me", None, 403),
        ("POST", f"/orgs/{org.org_id}/api-keys", {"name": "a way out"}, 403),
    ])
    names = [pe["name"] for pe in org.key.get_json("/platform-experiments?limit=200")]
    assert "a second one" not in names


# ---- a person signed in with a JWT -----------------------------------------------------------


def _b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def _raw_signature(der: bytes) -> bytes:
    """An ECDSA signature as JWS wants it: r and s, 32 bytes each, from openssl's DER pair."""
    assert der[0] == 0x30 and der[2] == 0x02, "not a DER ECDSA signature"
    r_len = der[3]
    r = der[4:4 + r_len]
    assert der[4 + r_len] == 0x02
    s_len = der[5 + r_len]
    s = der[6 + r_len:6 + r_len + s_len]
    return b"".join(int.from_bytes(part, "big").to_bytes(32, "big") for part in (r, s))


def _jwt(key_path: str, **claims) -> str:
    """An ES256 JWT of the local identity provider, signed by openssl with the given key."""
    body = {"iss": os.environ["HL_DEV_JWT_ISSUER"], "aud": os.environ["HL_DEV_JWT_AUDIENCE"],
            "exp": int(time.time()) + 600, **claims}
    body = {k: v for k, v in body.items() if v is not None}
    header = {"alg": "ES256", "kid": os.environ["HL_DEV_JWT_KID"], "typ": "JWT"}
    signing_input = f"{_b64(json.dumps(header).encode())}.{_b64(json.dumps(body).encode())}"
    der = subprocess.run(["openssl", "dgst", "-sha256", "-sign", key_path], input=signing_input.encode(),
                         capture_output=True, check=True).stdout
    return f"{signing_input}.{_b64(_raw_signature(der))}"


def _me(api: API, token: str) -> requests.Response:
    return requests.get(f"{api.base_url}/me", headers={"Authorization": f"Bearer {token}"}, timeout=30)


def test_a_person_signs_in_with_a_jwt_and_writes_under_the_providers_name(api: API, run_id: str, tmp_path):
    key = os.environ["HL_DEV_JWT_KEY"]
    subject = f"jwt-user-{uuid.uuid4().hex[:10]}-{run_id}"
    token = _jwt(key, sub=subject, name="Ada Lovelace")

    first = _me(api, token)
    assert first.status_code == 200, f"first request of a new person: {first.status_code} {first.text}"
    me = first.json()
    assert me["kind"] == "user" and me["user_id"] == subject and me["name"] == "Ada Lovelace"
    assert len(me["orgs"]) == 1 and len(me["projects"]) == 1, f"a new person's home: {me}"
    assert me["orgs"][0]["name"] == "Default org" and me["projects"][0]["name"] == "Default project", \
        f"a new person's home is named plainly, whatever their name is: {me}"
    assert _me(api, _jwt(key, sub=subject, name="Ada Lovelace")).json()["orgs"] == me["orgs"], "a second request made a second home"

    person = api.as_caller(token, me["orgs"][0]["id"], me["projects"][0]["id"])
    agent = f"agent-of-{subject}"
    person.register_agent(agent)
    with person.clusters_shared():
        pe_id = person.create_platform_experiment(f"jwt-{subject}", 1.0, 1)
        person.signup_and_start(pe_id, [agent])
    try:
        ada = {"kind": "user", "id": subject, "name": "Ada Lovelace"}
        as_another = person.post("/hypotheses", _hypothesis_body(pe_id, _claim("under another name"), author="Mallory"))
        assert as_another.status_code == 422, f"a person stating who they are: {as_another.status_code} {as_another.text}"
        own = person.post("/hypotheses", _hypothesis_body(pe_id, _claim("a person's idea")))
        assert own.status_code == 201, f"a person's hypothesis: {own.status_code} {own.text}"
        assert own.json()["actor"] == ada and not own.json().get("agent_id") and "author" not in own.json(), own.text
        assert own.json()["links"][0]["actor"] == ada, own.text
        for_agent = person.post("/hypotheses", _hypothesis_body(pe_id, _claim("the agent's idea"), agent_id=agent))
        assert for_agent.status_code == 201, f"a person naming their agent: {for_agent.status_code} {for_agent.text}"
        assert for_agent.json()["agent_id"] == agent and for_agent.json()["actor"] == ada, "a hypothesis registered for an agent names the person who did it"
        linked = person.post(f"/hypotheses/{own.json()['id']}/links", {"type": "related", "to": for_agent.json()["id"]})
        assert linked.status_code == 201 and all(l["actor"] == ada for l in linked.json()), linked.text
        graph = person.get_json(f"/hypotheses/graph?platform_experiment_id={pe_id}")
        assert {n["actor"]["id"] for n in graph["nodes"]} == {subject} and {e["actor"]["id"] for e in graph["edges"]} == {subject}, graph
        comment = person.post(f"/hypotheses/{own.json()['id']}/comments", {"text": "my note"})
        assert comment.status_code == 201, comment.text
        assert comment.json()["actor"] == {"kind": "user", "id": subject, "name": "Ada Lovelace"} and "author" not in comment.json(), comment.text
        claimed = person.post(f"/hypotheses/{own.json()['id']}/comments", {"author": "Mallory", "text": "under another name"})
        assert claimed.status_code == 422, f"a person stating who they are: {claimed.status_code} {claimed.text}"
        for_agent_note = person.post(f"/hypotheses/{own.json()['id']}/comments", {"agent_id": agent, "text": "for my agent"})
        assert for_agent_note.status_code == 201 and for_agent_note.json()["agent_id"] == agent, for_agent_note.text
        assert for_agent_note.json()["actor"] == {"kind": "user", "id": subject, "name": "Ada Lovelace"}, "a note written for an agent names the person who wrote it"

        nameless = api.as_caller(_jwt(key, sub=subject), me["orgs"][0]["id"], me["projects"][0]["id"])
        # A sign-in that states no name is still the same account, under the name last stated.
        unnamed = nameless.post("/hypotheses", _hypothesis_body(pe_id, _claim("stated with no name")))
        assert unnamed.status_code == 201 and unnamed.json()["actor"] == ada, f"a sign-in with no name: {unnamed.status_code} {unnamed.text}"
        assert nameless.post("/hypotheses", _hypothesis_body(pe_id, _claim("the agent's second"), agent_id=agent)).status_code == 201

        stranger = _jwt(key, sub=f"jwt-stranger-{uuid.uuid4().hex[:10]}-{run_id}", name="Eve")
        outside = requests.get(f"{api.base_url}/platform-experiments/{pe_id}", headers={"Authorization": f"Bearer {stranger}"}, timeout=30)
        assert outside.status_code == 404, f"another person reading it: {outside.status_code} {outside.text}"
    finally:
        person.close_platform_experiment(pe_id)

    other_key = tmp_path / "other.pem"
    subprocess.run(f"openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -out {other_key}",
                   shell=True, check=True, capture_output=True)
    now = int(time.time())
    for why, forged in {
        "signed by another key": _jwt(str(other_key), sub=subject, name="Ada Lovelace"),
        "issued for another service": _jwt(key, sub=subject, aud="another-service"),
        "with no audience": _jwt(key, sub=subject, aud=None),
        "from another issuer": _jwt(key, sub=subject, iss="https://issuer.elsewhere"),
        "expired": _jwt(key, sub=subject, exp=now - 5),
        "not valid yet": _jwt(key, sub=subject, nbf=now + 300),
        "of an anonymous session": _jwt(key, sub=subject, is_anonymous=True),
        "with no subject": _jwt(key, name="Ada Lovelace"),
        "with its claims edited": token.split(".")[0] + "." + _b64(json.dumps({"sub": "someone-else"}).encode()) + "." + token.split(".")[2],
    }.items():
        r = _me(api, forged)
        assert r.status_code == 401, f"a JWT {why}: {r.status_code} {r.text}"


# ---- tokens around a close -------------------------------------------------------------------


def test_tokens_after_a_close(org: Org, run_id: str):
    """Both tokens live the wind-down past the close and then die: the agent's and the
    coordinator's, which reads and files the summary meanwhile. Nothing that spends or starts
    work gets through either."""
    pe_id, (member,) = _running_experiment(org, org.key, run_id, 1)
    coordinator = org.key.as_caller(admin.token(f"coordinator:{pe_id}"))
    org.key.post(f"/experiments/{member.job_id}/cancel")
    closed_at = time.monotonic()
    closed = org.key.post(f"/platform-experiments/{pe_id}/close", {"top_results": []})
    assert closed.status_code == 200, f"close: {closed.status_code} {closed.text}"

    new_job = org.key.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", pe_id, member.agent)
    new_job["metadata"]["hypothesis_id"] = member.hypothesis_id
    in_the_window = [
        ("GET", f"/platform-experiments/{pe_id}", None, 200),
        ("GET", f"/experiments/{member.job_id}", None, 200),
        ("POST", "/experiments", new_job, 422),
        ("POST", f"/platform-experiments/{pe_id}/data-key", {"agent_id": member.agent}, 409),
    ]
    _assert_each(member.token, "the agent just after the close", in_the_window)
    submitted, text = _status(member.token, "POST", "/experiments", new_job)
    assert "experiment_closed" in text, f"a submission after the close: {submitted} {text}"

    _assert_each(coordinator, "the coordinator after the close", [
        ("GET", f"/platform-experiments/{pe_id}", None, 200),
        ("GET", f"/platform-experiments/{pe_id}/results", None, 200),
        ("POST", f"/platform-experiments/{pe_id}/summary", {"summary": "what the run showed"}, 200),
        ("POST", "/hypotheses/query", {"platform_experiment_id": pe_id, "cypher": "MATCH (h:Hypothesis) RETURN count(h)", "params": {}}, 200),
        ("POST", f"/platform-experiments/{pe_id}/start", None, 403),
        ("PATCH", f"/platform-experiments/{pe_id}", {"description": "reopened"}, 403),
        ("POST", f"/platform-experiments/{pe_id}/agents", {}, 403),
        ("POST", f"/platform-experiments/{pe_id}/data-key", {"agent_id": member.agent}, 403),
        ("POST", "/experiments", new_job, 403),
        ("POST", "/hypotheses", _hypothesis_body(pe_id, _claim("after the close"), agent_id=member.agent), 403),
        ("POST", f"/hypotheses/{member.hypothesis_id}/comments", {"agent_id": member.agent, "text": "late"}, 403),
    ])
    assert org.key.get_json(f"/platform-experiments/{pe_id}")["summary"] == "what the run showed"

    time.sleep(max(0.0, AGENT_WIND_DOWN + 5 - (time.monotonic() - closed_at)))
    _assert_each(member.token, "the agent after the wind-down", [
        ("GET", f"/platform-experiments/{pe_id}", None, 401),
        ("POST", f"/hypotheses/{member.hypothesis_id}/comments", {"agent_id": member.agent, "text": "too late"}, 401),
    ])
    _assert_each(coordinator, "the coordinator after the wind-down", [
        ("GET", f"/platform-experiments/{pe_id}", None, 401),
        ("POST", f"/platform-experiments/{pe_id}/summary", {"summary": "amended"}, 401),
    ])
    amended = org.key.post(f"/platform-experiments/{pe_id}/summary", {"summary": "amended"})
    assert amended.status_code == 200, f"the owner amending the summary after the wind-down: {amended.status_code} {amended.text}"


# ---- what a /watch stream delivers, and for how long ------------------------------------------


def _watch(caller: API, *args: str) -> subprocess.Popen:
    return subprocess.Popen([str(caller.hl_bin), "watch", *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=caller.hl_env())


def _await_connected(proc: subprocess.Popen) -> None:
    seen = []
    for line in proc.stderr:
        seen.append(line)
        if "connected" in line:
            return
    raise AssertionError(f"hl watch ended before connecting: {''.join(seen)}")


def test_a_watch_stream_carries_its_own_platform_experiment_and_no_other(api: API, org: Org, shared_experiment, run_id: str):
    pe_id, (mine, theirs) = shared_experiment
    elsewhere = _org(api, run_id, "b")
    other_pe, (outsider,) = _running_experiment(elsewhere, elsewhere.key, run_id, 1)
    whole = _watch(org.key, "--platform-experiment", pe_id, "--kinds", "comment.new", "--timeout", "12")
    as_agent = _watch(mine.token, "--platform-experiment", pe_id, "--agent", theirs.agent, "--kinds", "comment.new", "--timeout", "12")
    try:
        _await_connected(whole)
        _await_connected(as_agent)
        elsewhere.key.post_hypothesis_comment(outsider.hypothesis_id, "a note in another org", agent_id=outsider.agent).raise_for_status()
        org.key.post_hypothesis_comment(theirs.hypothesis_id, "a note on the rival's", agent_id=theirs.agent).raise_for_status()
        org.key.post_hypothesis_comment(mine.hypothesis_id, "a note on mine", agent_id=mine.agent).raise_for_status()
        seen, _ = whole.communicate(timeout=40)
        narrowed, _ = as_agent.communicate(timeout=40)
    finally:
        for proc in (whole, as_agent):
            if proc.poll() is None:
                proc.kill()
        _close(elsewhere.key, other_pe, [outsider])

    assert mine.hypothesis_id in seen and theirs.hypothesis_id in seen, f"the org's stream missed its own events: {seen}"
    assert outsider.hypothesis_id not in seen and other_pe not in seen, f"the org's stream carried another org's event: {seen}"
    # An agent token's stream is its own agent's whatever --agent asks for.
    assert outsider.hypothesis_id not in narrowed and other_pe not in narrowed, f"an agent's stream carried another org's event: {narrowed}"


def test_an_open_watch_stream_ends_once_its_key_is_revoked(org: Org, shared_experiment):
    pe_id, _ = shared_experiment
    token = admin.add_api_key(org.subject, org.org_id)
    revocable = org.key.as_caller(token, org.org_id, org.project_id)
    key_id = revocable.get_json("/me")["key_id"]
    request = requests.Request("GET", f"{org.key.base_url}/watch?platform_experiment_id={pe_id}", headers={
        "Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
        "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Protocol": f"{WATCH_SUBPROTOCOL}, {token}",
    }).prepare()
    with requests.Session() as session:
        stream = session.send(request, stream=True, timeout=60)
        assert stream.status_code == 101, f"upgrade: {stream.status_code} {stream.text}"
        opened = time.monotonic()
        # Revoking is a member's to do, so the org's owner signs in for it.
        owner = _jwt(os.environ["HL_DEV_JWT_KEY"], sub=org.subject, name="Owner")
        revoked = requests.delete(org.key._url(f"/api-keys/{key_id}"), headers={"Authorization": f"Bearer {owner}"}, timeout=30)
        assert revoked.status_code == 200, f"revoke: {revoked.status_code} {revoked.text}"
        # The server asks the token again on its ping, every twenty seconds, and closes.
        try:
            while stream.raw.read(4096):
                assert time.monotonic() - opened < 50, "the stream outlived its revoked key by more than two pings"
        except (requests.exceptions.RequestException, OSError):
            pass
        assert time.monotonic() - opened < 50, "the stream outlived its revoked key by more than two pings"
    assert revocable.get(f"/platform-experiments/{pe_id}").status_code == 401


# ---- liveness on metrics-service -------------------------------------------------------------


def _metrics_service(api: API) -> str:
    """metrics-service of the local stack, beside the API on its own port."""
    if os.environ.get("METRICS_URL"):
        return os.environ["METRICS_URL"].rstrip("/")
    parts = urlsplit(api.base_url)
    return urlunsplit((parts.scheme, f"{parts.hostname}:8084", "", "", ""))


def _push(url: str, token: str | None, job_id: str, at: datetime) -> int:
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    payload = {"node": "a-node", "timestamp": at.strftime("%Y-%m-%dT%H:%M:%SZ"), "pods": [{"pod_uid": f"uid-{job_id}", "experiment_id": job_id}]}
    return requests.post(f"{url}/internal/node-metrics", json=payload, headers=headers, timeout=30).status_code


def test_liveness_is_taken_only_from_the_cluster_a_job_is_placed_on(api: API, org: Org, shared_experiment, run_id: str):
    pe_id, (mine, _) = shared_experiment
    url = _metrics_service(api)
    cluster = f"cluster-{org.marker}-{run_id}"
    runtime = admin.add_cluster(cluster, org.org_id)
    now = datetime.now(timezone.utc)

    assert _push(url, None, mine.job_id, now) == 401
    assert _push(url, "nonsense", mine.job_id, now) == 401
    assert _push(url, mine.token.token, mine.job_id, now) == 403
    assert _push(url, org.key.token, mine.job_id, now) == 403
    assert _push(url, admin.token(f"job:{mine.job_id}"), mine.job_id, now) == 403
    assert _push(url, admin.token(f"runtime:never-registered-{uuid.uuid4().hex[:8]}"), mine.job_id, now) == 401
    assert requests.get(f"{url}/metrics", timeout=30).status_code == 401
    assert requests.get(f"{url}/metrics", headers={"Authorization": f"Bearer {runtime}"}, timeout=30).status_code == 403
    assert requests.get(f"{url}/metrics", headers={"Authorization": f"Bearer {api.token}"}, timeout=30).status_code == 200

    def observed() -> dict:
        return org.key.get_json(f"/experiments/{mine.job_id}/cost")

    before = observed()
    # A registered runtime, of the job's own org, for a job that is placed on no cluster of its:
    # accepted as a push and counted for nothing.
    for seconds_ago in (90, 60, 30):
        assert _push(url, runtime, mine.job_id, now - timedelta(seconds=seconds_ago)) == 204
    time.sleep(3)
    assert observed() == before, "a runtime wrote liveness for a job that is not placed on its cluster"
