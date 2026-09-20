"""Two orgs on one control plane: org A can neither read nor write anything of org B.

Each org gets a user, an API key, a registered agent, a started platform experiment with a
hypothesis, a job and a donation request. Every read is first made by the owner, so a 404 for the
other org is a refusal and never a route that answers 404 to everyone.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass

import pytest
import requests

from support import admin
from support.api import API

pytestmark = pytest.mark.parallel

WATCH_SUBPROTOCOL = "hypothesisloop.watch"
# Priced in controlplane/settings/hypothesisloop.yaml and on none of the dev nodes.
UNAVAILABLE_ACCELERATOR = "nvidia.com/gpu.product=NVIDIA-H200"


@dataclass
class Tenant:
    api: API
    org_id: str
    project_id: str
    agent: str
    pe_id: str
    hypothesis_id: str
    job_id: str
    donation_id: str
    cluster: str
    marker: str

    def identifiers(self) -> list[str]:
        return [self.org_id, self.project_id, self.agent, self.pe_id, self.hypothesis_id, self.job_id,
                self.donation_id, self.marker]


def _tenant(api: API, run_id: str, label: str) -> Tenant:
    marker = f"tenancy{label}{uuid.uuid4().hex[:10]}"
    subject = f"user-{marker}-{run_id}"
    home = admin.add_user(subject, f"Tenant {label} {marker}")
    caller = api.as_caller(admin.add_api_key(subject, home["org_id"]), home["org_id"], home["project_id"])
    agent = f"agent-{marker}-{run_id}"
    caller.register_agent(agent)
    pe_id = caller.create_platform_experiment(f"{marker}-{run_id}", 5.0, 2)
    caller.signup_and_start(pe_id, [agent])
    hypothesis_id = caller.register_hypothesis(pe_id, agent, f"{marker} hypothesis")["id"]
    # An accelerator type no local cluster has, so the job stays QUEUED: non-terminal for the job
    # token, and holding nothing.
    job_id = caller.submit_job(pe_id, agent, hyp_id=hypothesis_id, job_overrides={"accelerator_type": UNAVAILABLE_ACCELERATOR})
    donation_id = caller.create_donation(agent, pe_id, 0.5, f"{marker} donation")
    cluster = f"cluster-{marker}-{run_id}"
    admin.add_cluster(cluster, home["org_id"])
    return Tenant(caller, home["org_id"], home["project_id"], agent, pe_id, hypothesis_id, job_id, donation_id, cluster, marker)


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
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}],
        "starts_at": "0001-01-01T00:00:00Z", "ends_at": "2099-01-01T00:00:00Z", **fields,
    }


def _reads_of_one_resource(t: Tenant) -> list[str]:
    """Every GET that names one resource of a tenant by id."""
    return [
        f"/platform-experiments/{t.pe_id}",
        f"/platform-experiments/{t.pe_id}/signups/{t.agent}",
        f"/platform-experiments/{t.pe_id}/results",
        f"/platform-experiments/{t.pe_id}/quotas",
        f"/platform-experiments/{t.pe_id}/quotas/{t.agent}",
        f"/platform-experiments/{t.pe_id}/stages",
        f"/platform-experiments/{t.pe_id}/findings",
        f"/platform-experiments/{t.pe_id}/data-usage",
        f"/platform-experiments/{t.pe_id}/metrics-timeseries?metric_name=val_accuracy",
        f"/experiments/{t.job_id}",
        f"/experiments/{t.job_id}/lineage",
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
        f"/platform-experiments?project_id={t.project_id}",
        f"/platform-experiments?q={t.marker}",
        "/agents?limit=200",
        f"/agents?platform_experiment_id={t.pe_id}",
        f"/agents?q={t.marker}",
        f"/agents?project_id={t.project_id}",
        f"/agents/stats?platform_experiment_id={t.pe_id}",
        "/experiments?limit=200",
        f"/experiments?platform_experiment_id={t.pe_id}",
        f"/experiments?agent={t.agent}",
        f"/experiments?project_id={t.project_id}",
        f"/experiments?hypothesis_id={t.hypothesis_id}",
        f"/experiments/stats?platform_experiment_id={t.pe_id}",
        f"/experiments/stats?agent={t.agent}",
        "/hypotheses?limit=200",
        f"/hypotheses?platform_experiment_id={t.pe_id}",
        f"/hypotheses?project_id={t.project_id}",
        f"/hypotheses?agent={t.agent}",
        f"/hypotheses/stats?platform_experiment_id={t.pe_id}",
        "/donations?limit=200",
        f"/donations?platform_experiment_id={t.pe_id}",
        f"/donations?project_id={t.project_id}",
        f"/watch/subscriptions?platform_experiment_id={t.pe_id}",
    ]


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
        for path in ("/platform-experiments?limit=200", "/agents?limit=200", "/experiments?limit=200",
                     "/hypotheses?limit=200", "/donations?limit=200"):
            r = t.api.get(path)
            assert r.status_code == 200 and t.marker in r.text, f"owner GET {path} does not list its own rows: {r.text}"


def test_another_org_reads_nothing_by_id(tenants):
    a, b = tenants
    for path in _reads_of_one_resource(b):
        r = a.api.get(path)
        assert r.status_code == 404, f"org A GET {path}: {r.status_code} {r.text}"


def test_another_orgs_rows_are_in_no_list(tenants):
    a, b = tenants
    for path in _list_reads(b):
        r = a.api.get(path)
        assert r.status_code in (200, 404), f"org A GET {path}: {r.status_code} {r.text}"
        if r.status_code == 404:
            continue
        assert not _leaks(r, b), f"org A GET {path} shows {_leaks(r, b)} of org B: {r.text}"
        assert r.headers.get("X-Total-Count", "0") == "0" or "?limit=200" in path, (
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
        "donations": b.api.get_json(f"/donations?platform_experiment_id={b.pe_id}"),
    }

    body_naming_b = a.api.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", a.pe_id, a.agent)
    body_naming_b["metadata"]["platform_experiment_id"] = b.pe_id
    body_as_b = a.api.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", a.pe_id, a.agent)
    body_as_b["metadata"]["agent_id"] = b.agent
    pe_update = _platform_experiment_body("taken over")
    writes = [
        ("POST", f"/platform-experiments/{b.pe_id}/signup", {"agent_id": a.agent}),
        ("PUT", f"/platform-experiments/{b.pe_id}", pe_update),
        ("POST", f"/platform-experiments/{b.pe_id}/start", None),
        ("POST", f"/platform-experiments/{b.pe_id}/close", {"top_results": []}),
        ("POST", f"/platform-experiments/{b.pe_id}/summary", {"summary": "taken over"}),
        ("POST", f"/platform-experiments/{b.pe_id}/data-key", {"agent_id": a.agent}),
        ("POST", "/platform-experiments", _platform_experiment_body("intruding", project_id=b.project_id)),
        ("POST", "/agents", {"id": f"agent-intruder-{uuid.uuid4().hex[:8]}", "kind": "agent", "org_id": b.org_id}),
        ("POST", "/experiments", body_naming_b),
        ("POST", "/experiments", body_as_b),
        ("POST", f"/experiments/{b.job_id}/cancel", None),
        ("POST", f"/experiments/{b.job_id}/summary", {"summary": "taken over"}),
        ("POST", f"/experiments/{b.job_id}/admit", {"cluster_name": a.cluster}),
        ("POST", f"/experiments/{a.job_id}/admit", {"cluster_name": b.cluster}),
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
        ("POST", "/donations", {"agent_id": a.agent, "platform_experiment_id": b.pe_id, "credits_want": 1, "reason": "intruding"}),
        ("POST", f"/donations/{b.donation_id}/fulfill", {"donor_agent_id": a.agent}),
        ("POST", f"/donations/{b.donation_id}/cancel", {}),
    ]
    for method, path, body in writes:
        r = a.api.session.request(method, a.api._url(path), json=body, timeout=30)
        assert r.status_code in (403, 404), f"org A {method} {path}: {r.status_code} {r.text}"

    after = {
        "pe": b.api.get_json(f"/platform-experiments/{b.pe_id}"),
        "job": b.api.experiment(b.job_id),
        "hypothesis": b.api.hypothesis(b.hypothesis_id),
        "hypotheses": len(b.api.hypotheses(b.pe_id)),
        "donations": b.api.get_json(f"/donations?platform_experiment_id={b.pe_id}"),
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

    watched = a.api.hl_expect("watch", "--url", a.api.base_url, "--platform-experiment", b.pe_id, "--timeout", "5")
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
        for path in _reads_of_one_resource(b):
            r = caller.get(path)
            assert r.status_code in (403, 404), f"{scope} GET {path}: {r.status_code} {r.text}"
        for path in _list_reads(b):
            r = caller.get(path)
            assert r.status_code in (200, 403, 404) and (r.status_code != 200 or not _leaks(r, b)), (
                f"{scope} GET {path}: {r.status_code} {r.text}"
            )

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
    runtime = a.api.as_caller(admin.token(f"runtime:{a.cluster}"))
    report = {"cluster_id": f"id-{a.cluster}", "capacity": {}, "workloads": []}

    assert runtime.post(f"/internal/clusters/{b.cluster}/reconcile", report).status_code == 403
    assert runtime.post(f"/internal/clusters/{b.cluster}/status", {"statuses": []}).status_code == 403
    assert runtime.get("/internal/clusters/").status_code == 403
    assert runtime.get(f"/platform-experiments/{a.pe_id}").status_code == 403

    unregistered = a.api.as_caller(admin.token(f"runtime:never-registered-{uuid.uuid4().hex[:8]}"))
    assert unregistered.post(f"/internal/clusters/{a.cluster}/reconcile", report).status_code == 401
    for caller in (a.api, b.api):
        assert caller.post(f"/internal/clusters/{a.cluster}/reconcile", report).status_code == 403
    assert requests.post(f"{api.base_url}/internal/clusters/{a.cluster}/reconcile", json=report, timeout=30).status_code == 401


def test_cluster_settings_belong_to_the_clusters_org(tenants, api):
    """The connected clusters of the local stack belong to the default org, which neither tenant
    is in."""
    a, _ = tenants
    connected = [c for c in api.list_internal_clusters() if c["cluster_id"]]
    assert connected, "no connected cluster to check settings access against"
    for cluster in connected:
        path = f"/clusters/{cluster['cluster_id']}/settings"
        before = api.get_json(path)
        assert a.api.get(path).status_code == 404
        assert a.api.put(path, {"scale_up_timeout_seconds": 61}).status_code == 404
        assert a.api.post(f"/experiments/{a.job_id}/admit", {"cluster_name": cluster["cluster_name"]}).status_code == 404
        assert api.get_json(path) == before


def test_requests_without_a_valid_token_are_refused(tenants, api):
    a, _ = tenants
    path = f"/platform-experiments/{a.pe_id}"
    assert requests.get(f"{api.base_url}{path}", timeout=30).status_code == 401
    for token in ("", "nonsense", "hls_c3RhZmY.AAAA", "hlk_000000000000_AAAA", "a.b.c", a.api.token[:-2]):
        r = requests.get(f"{api.base_url}{path}", headers={"Authorization": f"Bearer {token}"}, timeout=30)
        assert r.status_code == 401, f"token {token!r}: {r.status_code} {r.text}"
    assert api.get(path).status_code == 200, "staff reads every org"
