"""Showcase platform experiments (plan 16.7): staff share one, and every account then reads it
and what is in it through the read routes it uses for its own, and through nothing else.

Two orgs, each a distinct person with an API key. Org A runs a platform experiment with an agent
and a hypothesis. Org B reads none of it until staff share it, reads all of it afterwards, writes
none of it at any time, and reads none of it again once staff take the sharing back.
"""
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass

import pytest
import requests

from support import admin
from support.api import API

pytestmark = pytest.mark.parallel


@dataclass
class Org:
    api: API
    org_id: str
    subject: str


def _org(api: API, run_id: str, label: str) -> Org:
    subject = f"showcase-{label}-{uuid.uuid4().hex[:10]}-{run_id}"
    home = admin.add_user(subject, f"Showcase {label} {run_id}")
    key = admin.add_api_key(subject, home["org_id"])
    return Org(api.as_caller(key, home["org_id"], home["project_id"]), home["org_id"], subject)


def _call(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


def _listed(api: API, query: str = "") -> set[str]:
    r = _call(api, "GET", f"/platform-experiments?limit=200{query}")
    assert r.status_code == 200, f"list platform experiments{query}: {r.status_code} {r.text}"
    return {pe["id"] for pe in r.json()}


def test_a_shared_platform_experiment_is_read_by_every_account_and_written_by_its_own_org(api: API, run_id: str):
    a, b = _org(api, run_id, "a"), _org(api, run_id, "b")
    agent = f"showcase-agent-{uuid.uuid4().hex[:8]}-{run_id}"
    a.api.register_agent(agent)
    pe_id = a.api.create_platform_experiment(f"showcase-{uuid.uuid4().hex[:8]}-{run_id}", 1.0, 2)
    try:
        a.api.signup_ok(pe_id, agent)
        posted = a.api.post_hypothesis(pe_id, text=f"a showcase claim {uuid.uuid4().hex[:8]}", agent_id=agent)
        assert posted.status_code in (200, 201), f"hypothesis: {posted.status_code} {posted.text}"
        hyp_id = posted.json()["id"]
        # Started, since an agent's quota exists from the start on and is one of the reads.
        a.api.start_platform_experiment(pe_id)

        # What another org reads of it. Every one of these is a read route an org uses for its own.
        reads = [
            ("GET", f"/platform-experiments/{pe_id}", None),
            ("GET", f"/platform-experiments/{pe_id}/agents", None),
            ("GET", f"/platform-experiments/{pe_id}/signups/{agent}", None),
            ("GET", f"/platform-experiments/{pe_id}/quotas", None),
            ("GET", f"/platform-experiments/{pe_id}/quotas/{agent}", None),
            ("GET", f"/platform-experiments/{pe_id}/stages", None),
            ("GET", f"/platform-experiments/{pe_id}/results", None),
            ("GET", f"/platform-experiments/{pe_id}/findings", None),
            ("GET", f"/platform-experiments/{pe_id}/data-usage", None),
            ("GET", f"/hypotheses/graph?platform_experiment_id={pe_id}", None),
            ("GET", f"/hypotheses/{hyp_id}", None),
            ("POST", "/hypotheses/query", {"platform_experiment_id": pe_id, "cypher": "MATCH (h:Hypothesis) RETURN h.id", "params": {}}),
        ]
        # What it may never do to it: every write, and every credential.
        writes = [
            ("POST", "/hypotheses", {"platform_experiment_id": pe_id, "text": "an intruding claim",
                                     "links": [{"type": "builds_on", "to": pe_id}]}),
            ("POST", f"/hypotheses/{hyp_id}/comments", {"text": "an intruding comment"}),
            ("POST", f"/hypotheses/{hyp_id}/links", {"type": "related", "to": pe_id}),
            ("POST", f"/hypotheses/{hyp_id}/status", {"agent_id": agent, "status": "confirmed", "despite": "an intruder settles it with a reason of its own"}),
            ("PATCH", f"/platform-experiments/{pe_id}", {"name": "taken over", "description": "taken over"}),
            ("POST", f"/platform-experiments/{pe_id}/start", None),
            ("POST", f"/platform-experiments/{pe_id}/close", {"top_results": []}),
            ("POST", f"/platform-experiments/{pe_id}/summary", {"summary": "an intruding summary"}),
            ("POST", f"/platform-experiments/{pe_id}/agents", {"agent_id": agent}),
            ("DELETE", f"/platform-experiments/{pe_id}/agents/{agent}", None),
            ("POST", f"/platform-experiments/{pe_id}/data-key", {"agent_id": agent}),
        ]
        listings = [f"/hypotheses?platform_experiment_id={pe_id}", f"/experiments?platform_experiment_id={pe_id}",
                    f"/agents?platform_experiment_id={pe_id}"]

        def refused_everything(why: str) -> None:
            for method, path, body in reads + writes:
                r = _call(b.api, method, path, body)
                assert r.status_code == 404, f"{why}: {method} {path} answered {r.status_code} {r.text}, want 404"

        refused_everything("before the platform experiment is shared")
        assert pe_id not in _listed(b.api) and pe_id not in _listed(b.api, "&shared=true")
        assert pe_id in _listed(a.api, "&shared=false"), "an unshared platform experiment is among its own org's unshared ones"
        assert _call(b.api, "GET", listings[0]).json() == [], "a listing narrowed to another org's platform experiment showed something"
        for verb in ("share", "unshare"):
            for caller in (a.api, b.api):
                r = _call(caller, "POST", f"/platform-experiments/{pe_id}/{verb}")
                assert r.status_code == 403, f"an API key called {verb}: {r.status_code} {r.text}"

        shared = api.post(f"/platform-experiments/{pe_id}/share")
        assert shared.status_code == 200 and shared.json()["id"] == pe_id and shared.json()["shared"] is True, shared.text

        for method, path, body in reads:
            r = _call(b.api, method, path, body)
            assert r.status_code == 200, f"a read of the shared platform experiment: {method} {path} answered {r.status_code} {r.text}"
            assert "hls_" not in r.text and "hlk_" not in r.text and "secret_access_key" not in r.text, f"{method} {path} carries a credential: {r.text}"
        assert _call(b.api, "GET", f"/platform-experiments/{pe_id}").json()["shared"] is True
        assert [h["id"] for h in _call(b.api, "GET", listings[0]).json()] == [hyp_id]
        assert agent in _call(b.api, "GET", listings[2]).text, "the agents of the shared platform experiment are not listed"
        for path in listings:
            assert _call(b.api, "GET", path).status_code == 200, f"a listing narrowed to the shared platform experiment: {path}"
        for method, path, body in writes:
            r = _call(b.api, method, path, body)
            assert r.status_code == 404, f"a write to the shared platform experiment: {method} {path} answered {r.status_code} {r.text}, want 404"
        git = _call(b.api, "POST", "/git-credentials", {"protocol": "https", "host": "github.com", "path": "x/y"})
        assert git.status_code >= 400, f"another org's key was handed a git credential: {git.status_code}"

        # The shared one is in every account's listing of platform experiments, in whatever
        # project, and ?shared filters on it. Every other listing stays the caller's own.
        assert pe_id in _listed(b.api) and pe_id in _listed(b.api, "&shared=true")
        assert pe_id in _listed(b.api, f"&project_id={b.api.project_id}"), "a shared one is in every project's listing"
        assert pe_id not in _listed(b.api, "&shared=false")
        assert pe_id in _listed(a.api, "&shared=true") and pe_id not in _listed(a.api, "&shared=false")
        assert pe_id in b.api.hl("platform-experiments", "list")
        assert pe_id in b.api.hl("platform-experiments", "list", "--shared")
        assert pe_id not in b.api.hl("platform-experiments", "list", "--shared=false")
        assert pe_id not in b.api.get("/hypotheses?limit=200").text and hyp_id not in b.api.get("/hypotheses?limit=200").text
        assert json.loads(b.api.hl("platform-experiments", "get", "--platform-experiment", pe_id))["shared"] is True

        # Its own org and its own agent work in it as before.
        own = a.api.post_hypothesis(pe_id, text=f"a second claim {uuid.uuid4().hex[:8]}", agent_id=agent)
        assert own.status_code in (200, 201), f"the owner writing to its shared platform experiment: {own.status_code} {own.text}"
        agent_api = api.as_caller(admin.token(f"agent:{pe_id}/{agent}"), a.org_id, a.api.project_id)
        assert agent_api.get(f"/platform-experiments/{pe_id}").status_code == 200

        # An agent of another platform experiment stays in its own, shared or not.
        other_agent = f"showcase-other-{uuid.uuid4().hex[:8]}-{run_id}"
        b.api.register_agent(other_agent)
        other_pe = b.api.create_platform_experiment(f"showcase-other-{uuid.uuid4().hex[:8]}-{run_id}", 1.0, 1)
        try:
            b.api.signup_ok(other_pe, other_agent)
            outsider = api.as_caller(admin.token(f"agent:{other_pe}/{other_agent}"), b.org_id, b.api.project_id)
            assert outsider.get(f"/platform-experiments/{pe_id}").status_code == 404
            assert outsider.get(f"/hypotheses/{hyp_id}").status_code == 404
        finally:
            b.api.close_platform_experiment(other_pe)

        admin.share_platform_experiment(pe_id, False)
        refused_everything("after staff took the sharing back")
        assert pe_id not in _listed(b.api) and pe_id not in _listed(b.api, "&shared=true")
        admin.share_platform_experiment(pe_id, True)
        assert _call(b.api, "GET", f"/platform-experiments/{pe_id}").status_code == 200, "hl-admin shares as the staff route does"
        unshared = api.post(f"/platform-experiments/{pe_id}/unshare")
        assert unshared.status_code == 200 and unshared.json()["shared"] is False, unshared.text
        assert api.post(f"/platform-experiments/pe-none-{uuid.uuid4().hex}/share").status_code == 404
    finally:
        # A shared platform experiment is in every account's listing until staff take it back.
        admin.share_platform_experiment(pe_id, False)
        a.api.close_platform_experiment(pe_id)
