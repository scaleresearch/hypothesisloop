"""Two orgs on one control plane: org A can neither read nor write anything of org B.

Each org gets a user, an API key, a registered agent, a started platform experiment with a
hypothesis and a job. Every read is first made by the owner, so a 404 for the
other org is a refusal and never a route that answers 404 to everyone.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass

import pytest
import requests

from conftest import cpu_job
from support import admin
from support.api import API, WATCH_SUBPROTOCOL
from support.wait import eventually

# Exclusive: an org made here owns no cluster that runs anything, so its platform experiment is
# created and started on the default org's clusters, which serve every org.
pytestmark = pytest.mark.exclusive

# A job of two cores in a platform experiment that runs one unit at a time fits the platform's
# clusters, which judge every org's submit, and is never admitted: it stays QUEUED, non-terminal
# for the job token.
HELD_JOB = cpu_job(2)
# The largest page GET /experiments serves: 422 invalid_page over it.
MAX_JOB_PAGE = 50


@dataclass
class Tenant:
    api: API
    org_id: str
    project_id: str
    agent: str
    pe_id: str
    hypothesis_id: str
    job_id: str
    cluster: str
    marker: str

    def identifiers(self) -> list[str]:
        return [self.org_id, self.project_id, self.agent, self.pe_id, self.hypothesis_id, self.job_id,
                self.marker]


def _tenant(api: API, run_id: str, label: str) -> Tenant:
    marker = f"tenancy{label}{uuid.uuid4().hex[:10]}"
    subject = f"user-{marker}-{run_id}"
    home = admin.add_user(subject, f"Tenant {label} {marker}")
    caller = api.as_caller(admin.add_api_key(subject, home["org_id"]), home["org_id"], home["project_id"])
    agent = f"agent-{marker}-{run_id}"
    caller.register_agent(agent)
    pe_id = caller.create_platform_experiment(f"{marker}-{run_id}", 5.0, 2, max_concurrent_accelerators=1)
    caller.signup_and_start(pe_id, [agent])
    hypothesis_id = caller.register_hypothesis(pe_id, agent, f"{marker} hypothesis")["id"]
    job_id = caller.submit_job(pe_id, agent, hyp_id=hypothesis_id, job_overrides=HELD_JOB)
    cluster = f"cluster-{marker}-{run_id}"
    admin.add_cluster(cluster, home["org_id"])
    return Tenant(caller, home["org_id"], home["project_id"], agent, pe_id, hypothesis_id, job_id, cluster, marker)


@pytest.fixture(scope="module")
def tenants(api: API, run_id: str):
    a, b = _tenant(api, run_id, "a"), _tenant(api, run_id, "b")
    yield a, b
    for tenant in (a, b):
        # The status is not checked: one of the jobs is cancelled by a test.
        tenant.api.post(f"/experiments/{tenant.job_id}/cancel")
        tenant.api.close_platform_experiment(tenant.pe_id)


def _platform_experiment_body(name: str, **fields) -> dict:
    return {
        "name": name, "description": name, "budget_accelerator_hours": 1, "max_agents": 1,
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "duration_hours": 24, **fields,
    }


def _reads_of_one_resource(t: Tenant) -> list[str]:
    """Every GET that names one resource of a tenant by id."""
    return [
        f"/platform-experiments/{t.pe_id}",
        f"/platform-experiments/{t.pe_id}/signups/{t.agent}",
        f"/platform-experiments/{t.pe_id}/agents",
        f"/platform-experiments/{t.pe_id}/results",
        f"/platform-experiments/{t.pe_id}/quotas",
        f"/platform-experiments/{t.pe_id}/quotas/{t.agent}",
        f"/platform-experiments/{t.pe_id}/stages",
        f"/platform-experiments/{t.pe_id}/findings",
        f"/platform-experiments/{t.pe_id}/data-usage",
        f"/platform-experiments/{t.pe_id}/metrics-timeseries?metric_name=val_accuracy",
        f"/experiments/{t.job_id}",
        f"/experiments/{t.job_id}/data",
        f"/experiments/{t.job_id}/metrics",
        f"/experiments/{t.job_id}/logs",
        f"/experiments/{t.job_id}/cost",
        f"/hypotheses/{t.hypothesis_id}",
        f"/hypotheses/graph?platform_experiment_id={t.pe_id}",
    ]


def _list_reads(t: Tenant) -> list[str]:
    """Every list and aggregate read, asked for a tenant's rows in each way its filters allow."""
    return [
        "/platform-experiments?limit=200",
        # A project's listing also holds what staff shared with every account, which is nobody's
        # leak and may be there from any run before this one.
        f"/platform-experiments?project_id={t.project_id}&shared=false",
        f"/platform-experiments?q={t.marker}",
        "/agents?limit=200",
        f"/agents?platform_experiment_id={t.pe_id}",
        f"/agents?q={t.marker}",
        f"/agents?project_id={t.project_id}",
        f"/agents/stats?platform_experiment_id={t.pe_id}",
        f"/experiments?limit={MAX_JOB_PAGE}",
        f"/experiments?platform_experiment_id={t.pe_id}",
        f"/experiments?agent_id={t.agent}",
        f"/experiments?project_id={t.project_id}",
        f"/experiments?hypothesis_id={t.hypothesis_id}",
        f"/experiments/stats?platform_experiment_id={t.pe_id}",
        f"/experiments/stats?agent_id={t.agent}",
        "/hypotheses?limit=200",
        f"/hypotheses?platform_experiment_id={t.pe_id}",
        f"/hypotheses?project_id={t.project_id}",
        f"/hypotheses?agent_id={t.agent}",
        f"/hypotheses/stats?platform_experiment_id={t.pe_id}",
    ]


# Routes of the coordinator audience: an agent token is refused on them whatever they name.
COORDINATOR_ONLY = ("/agents", "/findings", "/hypotheses/graph", "/hypotheses/stats")


def _coordinator_only(path: str) -> bool:
    return any(part in path for part in COORDINATOR_ONLY)


def _leaks(response: requests.Response, t: Tenant) -> list[str]:
    return [identifier for identifier in t.identifiers() if identifier in response.text]


def _counts(body) -> list[int]:
    """Every number in an aggregate body, so a count of another org's rows shows as non-zero."""
    if isinstance(body, bool):
        return []
    if isinstance(body, (int, float)):
        return [body]
    if isinstance(body, dict):
        return [n for value in body.values() for n in _counts(value)]
    if isinstance(body, list):
        return [n for value in body for n in _counts(value)]
    return []


def test_the_owner_reads_everything_of_its_own(tenants):
    for t in tenants:
        for path in _reads_of_one_resource(t):
            r = t.api.get(path)
            assert r.status_code == 200, f"owner GET {path}: {r.status_code} {r.text}"
        for path in ("/platform-experiments?limit=200", "/agents?limit=200", f"/experiments?limit={MAX_JOB_PAGE}",
                     "/hypotheses?limit=200"):
            r = t.api.get(path)
            assert r.status_code == 200 and t.marker in r.text, f"owner GET {path} does not list its own rows: {r.text}"
        # A page over a list's maximum is refused, so a short page is never read as the whole set.
        for path in (f"/experiments?limit={MAX_JOB_PAGE + 1}", "/hypotheses?limit=201", "/agents?limit=201",
                     "/platform-experiments?limit=201", "/platform-experiments?limit=-1", "/agents?offset=-1"):
            r = t.api.get(path)
            assert r.status_code == 422 and r.json()["reason"] == "invalid_page", f"owner GET {path}: {r.status_code} {r.text}"


def test_another_org_reads_nothing_by_id(tenants):
    a, b = tenants
    for path in _reads_of_one_resource(b):
        r = a.api.get(path)
        assert r.status_code == 404, f"org A GET {path}: {r.status_code} {r.text}"


def test_another_orgs_rows_are_in_no_list(tenants):
    a, b = tenants
    for path in _list_reads(b):
        r = a.api.get(path)
        assert r.status_code == 200, f"org A GET {path}: {r.status_code} {r.text}"
        assert not _leaks(r, b), f"org A GET {path} shows {_leaks(r, b)} of org B: {r.text}"
        assert r.headers.get("X-Total-Count", "0") == "0" or "?limit=" in path, (
            f"org A GET {path} counts {r.headers['X-Total-Count']} rows of org B"
        )
        if "/stats" in path:
            assert not any(_counts(r.json())), f"org A GET {path} counts org B's rows: {r.text}"


def test_another_org_writes_nothing(tenants):
    a, b = tenants
    before = {
        "pe": b.api.get_json(f"/platform-experiments/{b.pe_id}"),
        "job": b.api.experiment(b.job_id),
        "hypothesis": b.api.hypothesis(b.hypothesis_id),
        "hypotheses": len(b.api.hypotheses(b.pe_id)),
    }

    body_naming_b = a.api.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", a.pe_id, a.agent)
    body_naming_b["metadata"]["platform_experiment_id"] = b.pe_id
    body_as_b = a.api.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", a.pe_id, a.agent)
    body_as_b["metadata"]["agent_id"] = b.agent
    writes = [
        ("POST", f"/platform-experiments/{b.pe_id}/agents", {"agent_id": a.agent}),
        ("POST", f"/platform-experiments/{b.pe_id}/agents", {"in_agentsandbox": True}),
        ("DELETE", f"/platform-experiments/{b.pe_id}/agents/{b.agent}", None),
        ("PATCH", f"/platform-experiments/{b.pe_id}", {"name": "taken over"}),
        ("POST", f"/platform-experiments/{b.pe_id}/close", {"top_results": []}),
        ("PUT", f"/platform-experiments/{b.pe_id}/summary", {"summary": "taken over"}),
        ("POST", "/platform-experiments", _platform_experiment_body("intruding", project_id=b.project_id)),
        ("POST", "/agents", {"id": f"agent-intruder-{uuid.uuid4().hex[:8]}", "name": "Intruder", "org_id": b.org_id}),
        ("POST", "/experiments", body_naming_b),
        ("POST", "/experiments", body_as_b),
        ("POST", f"/experiments/{b.job_id}/cancel", None),
        ("POST", f"/experiments/{b.job_id}/summary", {"summary": "taken over"}),
        ("POST", f"/experiments/{b.job_id}/metrics", {"metric_name": "val_accuracy", "fraction_complete": 0.5, "metric_value": 1}),
        ("POST", "/hypotheses", {"agent_id": a.agent, "platform_experiment_id": b.pe_id, "text": "intruding",
                                 "links": [{"type": "builds_on", "to": b.pe_id}], "distinct_from": []}),
        ("POST", f"/hypotheses/{b.hypothesis_id}/links", {"agent_id": a.agent, "type": "builds_on", "to": b.pe_id}),
        ("POST", f"/hypotheses/{a.hypothesis_id}/links", {"agent_id": a.agent, "type": "builds_on", "to": b.hypothesis_id}),
        ("POST", f"/hypotheses/{b.hypothesis_id}/comments", {"agent_id": a.agent, "text": "intruding"}),
        ("POST", f"/hypotheses/{b.hypothesis_id}/status", {"agent_id": a.agent, "status": "refuted"}),
        ("POST", "/hypotheses", {"agent_id": a.agent, "platform_experiment_id": a.pe_id, "text": "linking out",
                                 "links": [{"type": "builds_on", "to": b.hypothesis_id}], "distinct_from": []}),
        ("POST", "/hypotheses", {"agent_id": a.agent, "platform_experiment_id": a.pe_id, "text": "told apart from another org",
                                 "links": [{"type": "builds_on", "to": a.pe_id}], "distinct_from": [b.hypothesis_id]}),
        ("POST", "/hypotheses/query", {"platform_experiment_id": b.pe_id, "cypher": "MATCH (h:Hypothesis) RETURN h.id", "params": {}}),
    ]
    for method, path, body in writes:
        r = a.api.session.request(method, a.api._url(path), json=body, timeout=30)
        assert r.status_code == 404, f"org A {method} {path}: {r.status_code} {r.text}"

    after = {
        "pe": b.api.get_json(f"/platform-experiments/{b.pe_id}"),
        "job": b.api.experiment(b.job_id),
        "hypothesis": b.api.hypothesis(b.hypothesis_id),
        "hypotheses": len(b.api.hypotheses(b.pe_id)),
    }
    assert after == before
    assert not b.api.metrics(b.job_id)
    assert b.api.signup_role(b.pe_id, a.agent) == (404, None)


def _watch_status(api: API, token: str | None, query: str) -> int:
    """The answer to a /watch request that is not an upgrade: 400 once access is granted."""
    headers = {"Authorization": ""}
    if token is not None:
        headers["Sec-WebSocket-Protocol"] = f"{WATCH_SUBPROTOCOL}, {token}"
    return requests.get(f"{api.base_url}/watch?{query}", headers=headers, timeout=30).status_code


def test_watch_stays_inside_the_callers_org(tenants):
    a, b = tenants
    for query in (f"platform_experiment_id={b.pe_id}", f"experiment_id={b.job_id}"):
        assert _watch_status(b.api, b.api.token, query) == 400, "the owner's watch was refused before the upgrade"
        assert _watch_status(a.api, a.api.token, query) == 404
        assert _watch_status(a.api, None, query) == 401
        assert _watch_status(a.api, a.api.token + "x", query) == 401

    watched = a.api.hl_expect("watch", "--platform-experiment", b.pe_id, "--timeout", "5")
    assert watched.returncode != 0 and "404" in watched.stderr, watched.stderr
    assert not watched.stdout.strip()


def test_a_job_token_makes_one_call_for_one_job(tenants):
    a, b = tenants
    job = b.api.as_caller(admin.token(f"job:{b.job_id}"))
    sample = {"metric_name": "val_accuracy", "fraction_complete": 0.25, "metric_value": 0.5}

    refused = [
        job.get(f"/experiments/{b.job_id}"),
        job.get(f"/experiments/{b.job_id}/metrics"),
        job.get(f"/platform-experiments/{b.pe_id}"),
        job.get("/experiments"),
        job.post(f"/experiments/{b.job_id}/cancel"),
        job.post(f"/experiments/{a.job_id}/metrics", sample),
        job.post(f"/experiments/job-missing-{uuid.uuid4().hex[:8]}/metrics", sample),
    ]
    for r in refused:
        assert r.status_code == 403, f"job token {r.request.method} {r.request.path_url}: {r.status_code} {r.text}"
    assert _watch_status(job, job.token, f"experiment_id={b.job_id}") == 403

    accepted = job.post(f"/experiments/{b.job_id}/metrics", sample)
    assert accepted.status_code < 300, f"job token on its own job: {accepted.status_code} {accepted.text}"
    assert not a.api.metrics(a.job_id)
    assert b.api.metric_values(b.job_id, "val_accuracy") == [0.5]

    cancelled = b.api.as_caller(admin.token(f"job:{a.job_id}"))
    a.api.cancel_job(a.job_id)
    assert cancelled.post(f"/experiments/{a.job_id}/metrics", sample).status_code == 401, "an ended job's token still works"


def test_derived_tokens_stay_inside_their_platform_experiment(tenants):
    a, b = tenants
    for scope in (f"agent:{a.pe_id}/{a.agent}", f"coordinator:{a.pe_id}"):
        caller = a.api.as_caller(admin.token(scope))
        assert caller.get(f"/platform-experiments/{a.pe_id}").status_code == 200, scope
        is_agent = scope.startswith("agent:")
        for path in _reads_of_one_resource(b):
            want = 403 if is_agent and _coordinator_only(path) else 404
            r = caller.get(path)
            assert r.status_code == want, f"{scope} GET {path}: got {r.status_code}, want {want}: {r.text}"
        for path in _list_reads(b):
            # A token bound to one platform experiment cannot ask for another project's listing.
            want = 403 if is_agent and _coordinator_only(path) else 404 if "project_id=" in path else 200
            r = caller.get(path)
            assert r.status_code == want, f"{scope} GET {path}: got {r.status_code}, want {want}: {r.text}"
            assert r.status_code != 200 or not _leaks(r, b), f"{scope} GET {path} shows {_leaks(r, b)} of org B: {r.text}"

    agent = a.api.as_caller(admin.token(f"agent:{a.pe_id}/{a.agent}"))
    r = agent.post("/hypotheses", {"agent_id": b.agent, "platform_experiment_id": a.pe_id, "text": "as another agent",
                                   "links": [{"type": "builds_on", "to": a.pe_id}], "distinct_from": []})
    assert r.status_code == 403, f"an agent token naming another agent: {r.status_code} {r.text}"


def test_a_project_key_reaches_only_its_project(tenants, run_id):
    a, _ = tenants
    subject = f"user-{a.marker}-{run_id}"
    bound = a.api.as_caller(admin.add_api_key(subject, a.org_id, a.project_id), a.org_id, a.project_id)
    assert bound.get(f"/platform-experiments/{a.pe_id}").status_code == 200

    other_project = "default"
    r = bound.post("/platform-experiments", _platform_experiment_body("outside", project_id=other_project))
    assert r.status_code == 404, f"a project key creating in another project: {r.status_code} {r.text}"
    assert bound.get(f"/platform-experiments?project_id={other_project}").status_code == 404


def test_a_runtime_token_serves_its_own_registered_cluster_only(tenants, api):
    a, b = tenants
    runtime = a.api.as_caller(admin.cluster_token(a.cluster))
    report = {"cluster_id": f"id-{a.cluster}", "capacity": {}, "workloads": []}

    assert runtime.post(f"/internal/clusters/{b.cluster}/reconcile", report).status_code == 403
    assert runtime.post(f"/internal/clusters/{b.cluster}/status", {"statuses": []}).status_code == 403
    assert runtime.get("/internal/clusters/").status_code == 403
    assert runtime.get(f"/platform-experiments/{a.pe_id}").status_code == 403

    unregistered = a.api.as_caller(admin.token(f"runtime:never-registered-{uuid.uuid4().hex[:8]}/1"))
    assert unregistered.post(f"/internal/clusters/{a.cluster}/reconcile", report).status_code == 401
    for caller in (a.api, b.api):
        assert caller.post(f"/internal/clusters/{a.cluster}/reconcile", report).status_code == 403
    assert requests.post(f"{api.base_url}/internal/clusters/{a.cluster}/reconcile", json=report, timeout=30).status_code == 401


def _default_orgs_clusters(api: API) -> list[dict]:
    """The connected clusters registered for the default org. A name another test registered and
    removed still heartbeats in the listing of everything that reports, and is no cluster."""
    registered = {c["name"] for c in api.get_json("/orgs/default/clusters")["clusters"] if c["owned"]}
    return [c for c in api.internal_clusters() if c["connected"] and c["cluster_name"] in registered]


def test_cluster_settings_belong_to_the_clusters_org(tenants, api):
    """The connected clusters of the local stack belong to the default org, which neither tenant
    is in."""
    a, _ = tenants
    connected = _default_orgs_clusters(api)
    assert connected, "no connected cluster to check settings access against"

    def listed() -> list:
        return api.get_json("/orgs/default/clusters")["clusters"]

    before = listed()
    for cluster in connected:
        name = cluster["cluster_name"]
        for path in (f"/orgs/default/clusters/{name}", f"/orgs/{a.org_id}/clusters/{name}"):
            assert a.api.patch(path, {"scale_up_timeout_seconds": 61}).status_code == 404
    assert listed() == before


def test_requests_without_a_valid_token_are_refused(tenants, api):
    a, _ = tenants
    path = f"/platform-experiments/{a.pe_id}"
    assert requests.get(f"{api.base_url}{path}", timeout=30).status_code == 401
    for token in ("", "nonsense", "hls_c3RhZmY.AAAA", "hlk_000000000000_AAAA", "a.b.c", a.api.token[:-2]):
        r = requests.get(f"{api.base_url}{path}", headers={"Authorization": f"Bearer {token}"}, timeout=30)
        assert r.status_code == 401, f"token {token!r}: {r.status_code} {r.text}"
    assert api.get(path).status_code == 200, "staff reads every org"


def _accounts_of(t: Tenant) -> list[tuple[str, str, dict | None]]:
    """Every accounts call that names something of one tenant's org."""
    return [
        ("PATCH", f"/orgs/{t.org_id}", {"name": "taken over"}),
        ("GET", f"/orgs/{t.org_id}/projects", None),
        ("POST", f"/orgs/{t.org_id}/projects", {"name": "intruding"}),
        ("PATCH", f"/projects/{t.project_id}", {"name": "taken over"}),
        ("GET", f"/orgs/{t.org_id}/api-keys", None),
        ("POST", f"/orgs/{t.org_id}/api-keys", {"name": "intruding"}),
        ("POST", f"/orgs/{t.org_id}/api-keys", {"name": "intruding", "project_id": t.project_id}),
        ("DELETE", f"/api-keys/{t.api.get_json('/me')['key_id']}", None),
        ("GET", f"/orgs/{t.org_id}/clusters", None),
        ("POST", f"/orgs/{t.org_id}/clusters", {"name": f"intruding-{uuid.uuid4().hex[:8]}"}),
    ]


def test_another_orgs_accounts_are_out_of_reach(tenants):
    a, b = tenants
    before = {
        "me": b.api.get_json("/me"),
        "keys": b.api.get_json(f"/orgs/{b.org_id}/api-keys"),
        "clusters": [c["name"] for c in b.api.get_json(f"/orgs/{b.org_id}/clusters")["clusters"]],
    }
    owned = [c["name"] for c in b.api.get_json(f"/orgs/{b.org_id}/clusters")["clusters"] if c["owned"]]
    assert owned == [b.cluster] and a.cluster not in before["clusters"], f"org B lists {before['clusters']}, owning {owned}"
    for method, path, body in _accounts_of(b):
        r = a.api.session.request(method, a.api._url(path), json=body, timeout=30)
        assert r.status_code == 404, f"org A {method} {path}: {r.status_code} {r.text}"
    taken = a.api.post(f"/orgs/{a.org_id}/clusters", {"name": b.cluster})
    assert taken.status_code == 409 and "hls_" not in taken.text, f"org A registering org B's cluster: {taken.status_code} {taken.text}"
    crossing = a.api.post(f"/orgs/{a.org_id}/api-keys", {"name": "crossing", "project_id": b.project_id})
    assert crossing.status_code == 404, f"a key of org A bound to a project of org B: {crossing.status_code} {crossing.text}"

    me = a.api.get_json("/me")
    assert [o["id"] for o in me["orgs"]] == [a.org_id] and {p["org_id"] for p in me["projects"]} == {a.org_id}
    assert before == {
        "me": b.api.get_json("/me"),
        "keys": b.api.get_json(f"/orgs/{b.org_id}/api-keys"),
        "clusters": [c["name"] for c in b.api.get_json(f"/orgs/{b.org_id}/clusters")["clusters"]],
    }
    assert admin.add_cluster(b.cluster, b.org_id) == admin.cluster_token(b.cluster), "org B's runtime token changed"


def test_capacity_stays_inside_the_org(tenants, api):
    """The connected clusters of the local stack belong to the default org, the platform's, whose
    clusters serve every org: a tenant reads their capacity and that of no other org's."""
    a, b = tenants
    connected = {c["cluster_name"] for c in _default_orgs_clusters(api)}
    assert connected, "no connected cluster to check capacity against"
    assert connected <= {c["cluster_name"] for c in api.capacity()["clusters"]}, "staff do not read every cluster's capacity"
    shown = {c["cluster_name"] for c in a.api.capacity()["clusters"]}
    assert shown == connected, f"a tenant reads the capacity of {shown}, want the platform's clusters {connected}"
    capacity = a.api.hl("capacity")
    assert all(name in capacity for name in connected), f"hl capacity of a tenant names none of {connected}: {capacity}"

    # Capacity is read for one org. A caller names its own or none, and no other.
    path = "/resource-catalog/capacity"
    assert a.api.capacity(a.org_id) == a.api.capacity(), "naming the token's own org changes what it reads"
    agent, coordinator = (a.api.as_caller(admin.token(scope)) for scope in (f"agent:{a.pe_id}/{a.agent}", f"coordinator:{a.pe_id}"))
    for caller, who in ((a.api, "an API key"), (agent, "an agent"), (coordinator, "a coordinator")):
        assert {c["cluster_name"] for c in caller.capacity()["clusters"]} == shown, f"{who} reads other clusters than its org's"
        for org_id in (b.org_id, "default", f"org-never-made-{uuid.uuid4().hex[:8]}"):
            r = caller.get(f"{path}?org_id={org_id}")
            assert r.status_code == 404, f"{who} reading the capacity of {org_id}: {r.status_code} {r.text}"
    assert connected <= {c["cluster_name"] for c in api.capacity("default")["clusters"]}, "staff do not read the default org's clusters by naming it"
    assert {c["cluster_name"] for c in api.capacity(a.org_id)["clusters"]} == shown, "staff reading one org are shown what the org does not read"
    assert api.get(f"{path}?org_id=org-never-made-{uuid.uuid4().hex[:8]}").status_code == 404


def test_a_job_of_an_org_with_no_cluster_runs_on_the_platforms_and_one_that_turned_them_off_waits_for_its_own(tenants, api, run_id, deadline):
    """A job any local cluster could run. A cluster of the platform's org serves every org, so the
    job of an org that owns none is placed on one. An org that turned the platform's clusters off
    and whose own registered cluster is away waits for it, and is told nothing about a cluster
    being missing."""
    a, _ = tenants
    connected = {c["cluster_name"] for c in _default_orgs_clusters(api)}
    marker = f"tenancylone{uuid.uuid4().hex[:10]}"
    home = admin.add_user(f"user-{marker}-{run_id}", f"Tenant lone {marker}")
    lone = api.as_caller(admin.add_api_key(f"user-{marker}-{run_id}", home["org_id"]), home["org_id"], home["project_id"])
    lone_agent = f"agent-{marker}-{run_id}"
    lone.register_agent(lone_agent)
    lone_pe = lone.create_platform_experiment(f"{marker}-{run_id}", 5.0, 2)
    lone.signup_and_start(lone_pe, [lone_agent])
    away_agent = f"agent-away-{marker}-{run_id}"
    a.api.register_agent(away_agent)
    away_pe = a.api.create_platform_experiment(f"away-{marker}-{run_id}", 5.0, 2)
    a.api.signup_and_start(away_pe, [away_agent])
    platforms = [c["name"] for c in a.api.get_json(f"/orgs/{a.org_id}/clusters")["clusters"] if not c["owned"]]
    for name in platforms:
        a.api.hl("cluster", "disable", name)
    lone_job = lone.submit_job(lone_pe, lone_agent)
    away_job = a.api.submit_job(away_pe, away_agent)
    try:
        placed = eventually("the job of an org with no cluster is placed on the platform's", lambda: lone.experiment(lone_job), deadline=deadline, interval=2,
                            accept=lambda exp: exp.get("cluster_name") in connected)
        assert placed["org_id"] == home["org_id"]

        waiting = eventually("org A's job says why it waits", lambda: a.api.experiment(away_job), deadline=deadline, interval=2,
                             accept=lambda exp: exp.get("not_admitted_reason") not in (None, "", "awaiting_admission"))
        assert waiting["status"] == "QUEUED" and not waiting.get("cluster_name"), f"placed beside its org's own cluster: {waiting}"
        assert waiting["not_admitted_reason"] not in ("no_cluster_for_org", "clusters_disabled"), f"an org whose cluster is away was told {waiting['not_admitted_reason']}"
    finally:
        for name in platforms:
            a.api.hl("cluster", "enable", name)
        a.api.post(f"/experiments/{away_job}/cancel")
        lone.post(f"/experiments/{lone_job}/cancel")
        a.api.close_platform_experiment(away_pe)
        lone.close_platform_experiment(lone_pe)
