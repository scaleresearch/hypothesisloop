"""Accounts and cluster ownership, driven with an API key and nothing else.

A person exists once they sign in, and their first key comes from the UI; hl-admin stands in for
both. Everything after that is done with `hlk_` keys through `hl` and the API: projects, more
keys, platform experiments, clusters. A second org sees none of it, and its jobs land on none of
the first org's clusters. A cluster of the platform's org runs every org's jobs.
"""
from __future__ import annotations

import json
import re
import time
import uuid
from dataclasses import dataclass

import pytest
import requests

from conftest import DEFAULT_ORG
from support import admin
from support.api import API, HLError
from support.wait import Deadline, eventually


# Priced in controlplane/settings/hypothesisloop.yaml, installed on none of the dev nodes and asked
# for by no other test, so nothing but this module's jobs can fit the clusters it fakes.
OWN_ACCELERATOR = "nvidia.com/gpu.product=NVIDIA-GeForce-GTX-1070-Ti"
# One job's share of the faked node, small enough that no other test's job fits beside it.
JOB_SHAPE = {
    "accelerator_type": OWN_ACCELERATOR, "accelerator_count": 1,
    "cpu": "10m", "memory": "16Mi", "storage": "16Mi",
}
MI = 1 << 20


@dataclass
class Org:
    api: API
    org_id: str
    project_id: str
    subject: str
    marker: str


def _org(api: API, run_id: str, label: str) -> Org:
    marker = f"accounts{label}{uuid.uuid4().hex[:10]}"
    subject = f"user-{marker}-{run_id}"
    home = admin.add_user(subject, f"Org {label} {marker}")
    key = admin.add_api_key(subject, home["org_id"])
    return Org(api.as_caller(key, home["org_id"], home["project_id"]), home["org_id"], home["project_id"], subject, marker)


@pytest.fixture(scope="module")
def orgs(api: API, run_id: str):
    return _org(api, run_id, "a"), _org(api, run_id, "b")


def _status(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


def _lines(out: str) -> list[dict]:
    return [json.loads(line) for line in out.splitlines() if line.strip()]


def _key(api: API, org_id: str, name: str, project_id: str = "") -> dict:
    body = {"name": name} | ({"project_id": project_id} if project_id else {})
    return api.post_json(f"/orgs/{org_id}/api-keys", body)


@pytest.mark.exclusive
def test_an_api_key_runs_its_org_through_hl(orgs):
    """Exclusive: its platform experiment's baseline runs on the default org's clusters."""
    a, b = orgs

    me = a.api.get_json("/me")
    assert me["kind"] == "key" and [o["id"] for o in me["orgs"]] == [a.org_id]
    assert [p["id"] for p in me["projects"]] == [a.project_id]
    assert [o["id"] for o in _lines(a.api.hl("org", "list"))] == [a.org_id]

    renamed = json.loads(a.api.hl("org", "rename", f"renamed {a.marker}"))
    assert renamed == {**renamed, "id": a.org_id, "name": f"renamed {a.marker}"}
    with pytest.raises(HLError, match="the platform refuses this token"):
        a.api.hl("org", "create", f"second {a.marker}")

    project = json.loads(a.api.hl("project", "create", f"vision-{a.marker}"))
    assert project["org_id"] == a.org_id and project["id"] != a.project_id
    with pytest.raises(HLError, match="409"):
        a.api.hl("project", "create", f"vision-{a.marker}")
    with pytest.raises(HLError, match="409"):
        a.api.hl("project", "rename", "--project", project["id"], "Default project")
    assert json.loads(a.api.hl("project", "rename", "--project", project["id"], f"language-{a.marker}"))["id"] == project["id"]
    assert {p["id"]: p["name"] for p in _lines(a.api.hl("project", "list"))} == {
        a.project_id: "Default project", project["id"]: f"language-{a.marker}",
    }

    created = json.loads(a.api.hl("api-key", "create", f"ci-{a.marker}", "--project", project["id"]))
    assert created["token"].startswith(f"hlk_{created['id']}_") and created["project_id"] == project["id"]
    assert created["minted_by"] == me["key_id"], "a key made with a key names the key that made it"
    bound = a.api.as_caller(created["token"], a.org_id, project["id"])

    pe_id = bound.create_platform_experiment(f"{a.marker}-pe", 1.0, 1)
    assert bound.get_json(f"/platform-experiments/{pe_id}")["project_id"] == project["id"]
    assert pe_id in bound.hl("platform-experiments", "list")
    assert pe_id not in a.api.hl("platform-experiments", "list", "--project", a.project_id)
    outside = bound.post("/platform-experiments", {"project_id": a.project_id, "name": "outside", "description": "outside",
                                                   "budget_accelerator_hours": 1, "max_agents": 1, "report_interval_seconds": 10,
                                                   "duration_hours": 24, "metrics": [{"key": "val_accuracy", "direction": "maximize"}]})
    assert outside.status_code == 404, outside.text

    assert [p["id"] for p in bound.get_json(f"/orgs/{a.org_id}/projects")["projects"]] == [project["id"]]
    assert [p["id"] for p in bound.get_json("/me")["projects"]] == [project["id"]]
    for method, path, body in [
        ("POST", f"/orgs/{a.org_id}/projects", {"name": "escape"}),
        ("PATCH", f"/projects/{project['id']}", {"name": "escape"}),
        ("PATCH", f"/orgs/{a.org_id}", {"name": "escape"}),
        ("POST", f"/orgs/{a.org_id}/api-keys", {"name": "org-wide"}),
        ("POST", f"/orgs/{a.org_id}/api-keys", {"name": "other project", "project_id": a.project_id}),
        ("POST", f"/orgs/{a.org_id}/clusters", {"name": f"escape-{a.marker}"}),
        ("GET", f"/orgs/{a.org_id}/clusters", None),
    ]:
        r = _status(bound, method, path, body)
        assert r.status_code == 403, f"a project key {method} {path}: {r.status_code} {r.text}"

    listing = a.api.get(f"/orgs/{a.org_id}/api-keys")
    assert created["id"] in listing.text and me["key_id"] in listing.text
    secret = created["token"].removeprefix(f"hlk_{created['id']}_")
    for leak in (secret, "token", "key_hash"):
        assert leak not in listing.text, f"the key listing shows {leak!r}: {listing.text}"
    assert secret not in a.api.hl("api-key", "list")

    for method, path, body in [
        ("GET", f"/orgs/{a.org_id}/projects", None),
        ("POST", f"/orgs/{a.org_id}/projects", {"name": "intruding"}),
        ("PATCH", f"/orgs/{a.org_id}", {"name": "taken over"}),
        ("PATCH", f"/projects/{project['id']}", {"name": "taken over"}),
        ("GET", f"/orgs/{a.org_id}/api-keys", None),
        ("POST", f"/orgs/{a.org_id}/api-keys", {"name": "intruding"}),
        ("POST", f"/orgs/{b.org_id}/api-keys", {"name": "crossing", "project_id": project["id"]}),
        ("DELETE", f"/api-keys/{created['id']}", None),
        ("GET", f"/orgs/{a.org_id}/clusters", None),
        ("POST", f"/orgs/{a.org_id}/clusters", {"name": f"intruding-{b.marker}"}),
        ("GET", f"/platform-experiments/{pe_id}", None),
    ]:
        r = _status(b.api, method, path, body)
        assert r.status_code == 404, f"org B {method} {path}: {r.status_code} {r.text}"
    for path in ("/me", f"/orgs/{b.org_id}/projects", f"/orgs/{b.org_id}/api-keys", "/platform-experiments?limit=200",
                 f"/platform-experiments?project_id={project['id']}"):
        r = b.api.get(path)
        assert r.status_code == 200 and a.org_id not in r.text and project["id"] not in r.text and pe_id not in r.text, (
            f"org B GET {path} shows something of org A: {r.text}"
        )
    assert bound.get(f"/platform-experiments/{pe_id}").status_code == 200, "org B's refused writes changed org A's key"
    bound.close_platform_experiment(pe_id)


@pytest.mark.parallel
def test_a_revoked_key_stops_at_once_with_every_key_minted_from_it(orgs):
    a, b = orgs
    root_id = a.api.get_json("/me")["key_id"]
    parent = _key(a.api, a.org_id, f"parent-{a.marker}")
    assert parent["actor"] == {"kind": "user", "id": a.subject, "name": f"Org a {a.marker}"}, f"a key names the person behind it: {parent}"
    parent_api = a.api.as_caller(parent["token"], a.org_id, a.project_id)
    child = _key(parent_api, a.org_id, f"child-{a.marker}", a.project_id)
    child_api = a.api.as_caller(child["token"], a.org_id, a.project_id)
    grandchild = _key(child_api, a.org_id, f"grandchild-{a.marker}", a.project_id)
    grandchild_api = a.api.as_caller(grandchild["token"], a.org_id, a.project_id)
    sibling = _key(a.api, a.org_id, f"sibling-{a.marker}")
    sibling_api = a.api.as_caller(sibling["token"], a.org_id, a.project_id)
    lineage = {"parent": parent_api, "child": child_api, "grandchild": grandchild_api}

    for name, caller in lineage.items():
        assert caller.get("/me").status_code == 200, f"the {name} key does not work before it is revoked"
    assert {k["id"] for k in child_api.get_json(f"/orgs/{a.org_id}/api-keys")["api_keys"]} == {child["id"], grandchild["id"]}
    for caller, target in [(child_api, root_id), (child_api, parent["id"]), (child_api, sibling["id"]),
                           (parent_api, root_id), (parent_api, sibling["id"]), (b.api, parent["id"])]:
        r = _status(caller, "DELETE", f"/api-keys/{target}")
        assert r.status_code == 404, f"revoking a key outside the caller's lineage: {r.status_code} {r.text}"
    for name, caller in lineage.items():
        assert caller.get("/me").status_code == 200, f"a refused revocation ended the {name} key"

    revoked = json.loads(a.api.hl("api-key", "revoke", parent["id"]))
    assert revoked["id"] == parent["id"] and revoked["revoked_at"]
    for name, caller in lineage.items():
        for method, path in [("GET", "/me"), ("GET", "/platform-experiments"), ("POST", f"/orgs/{a.org_id}/api-keys")]:
            r = _status(caller, method, path, {"name": "after"})
            assert r.status_code == 401, f"the {name} key after its lineage was revoked, {method} {path}: {r.status_code} {r.text}"
        failed = caller.hl_expect("org", "list")
        assert failed.returncode != 0 and "401" in failed.stderr, failed.stderr
    assert sibling_api.get("/me").status_code == 200 and a.api.get("/me").status_code == 200

    by_id = {k["id"]: k for k in a.api.get_json(f"/orgs/{a.org_id}/api-keys")["api_keys"]}
    for key in (parent, child, grandchild):
        assert by_id[key["id"]].get("revoked_at"), f"key {key['name']} is listed as live"
    assert not by_id[sibling["id"]].get("revoked_at") and not by_id[root_id].get("revoked_at")
    again = json.loads(a.api.hl("api-key", "revoke", parent["id"]))
    assert again["revoked_at"] == revoked["revoked_at"], "a second revocation moved revoked_at"


def _report(cluster_id: str) -> dict:
    """What a runtime with one idle node of two accelerators reports. The node holds two jobs of
    JOB_SHAPE and nothing larger. The control plane takes the jobs it placed off these figures
    itself, so a runtime that runs nothing reports the same node every time."""
    node = {"cpu_millicores": 20, "memory_bytes": 32 * MI, "storage_bytes": 32 * MI}
    return {
        "cluster_id": cluster_id,
        "cpu_available_cores": 0.02, "cpu_total_cores": 0.02,
        "ram_available_bytes": node["memory_bytes"], "ram_total_bytes": node["memory_bytes"],
        "storage_available_bytes": node["storage_bytes"], "storage_total_bytes": node["storage_bytes"],
        "accelerator_available_by_type": {OWN_ACCELERATOR: 2},
        "accelerator_total_by_type": {OWN_ACCELERATOR: 2},
        "accelerator_available_by_node": {"node-1": {OWN_ACCELERATOR: 2}},
        "node_resources_by_node": {"node-1": node},
        "node_resources_total_by_node": {"node-1": node},
        "node_labels_by_node": {"node-1": {}},
        "multi_node_capable": False, "autoscaler_enabled": False,
    }


class FakeRuntime:
    """A cluster agent that reports capacity, reads what it is told to run and runs nothing."""

    def __init__(self, api: API, name: str, token: str):
        self.name, self.api, self.cluster_id = name, api.as_caller(token), f"id-{name}"
        self.handed: set[str] = set()

    def reconcile(self) -> list[dict]:
        r = self.api.post(f"/internal/clusters/{self.name}/reconcile", _report(self.cluster_id))
        assert r.status_code == 200, f"reconcile of {self.name}: {r.status_code} {r.text}"
        experiments = r.json()["experiments"] or []
        self.handed |= {e["id"] for e in experiments}
        return experiments


def _running_org(org: Org, run_id: str) -> tuple[str, str]:
    """An agent in a started platform experiment of the org's default project."""
    agent = f"agent-{uuid.uuid4().hex[:10]}-{run_id}"
    org.api.register_agent(agent)
    pe_id = org.api.create_platform_experiment(f"{org.marker}-{run_id}", 5.0, 2)
    org.api.signup_and_start(pe_id, [agent])
    return agent, pe_id


def _capacity_names(api: API) -> set[str]:
    return {c["cluster_name"] for c in api.capacity()["clusters"]}


@pytest.mark.timeout(240)
@pytest.mark.exclusive
def test_an_orgs_cluster_runs_only_its_jobs_and_a_platform_cluster_runs_any_orgs(orgs, api: API, run_id: str, deadline: Deadline):
    """Exclusive: the cluster of the platform's org it registers is in every org's capacity and
    cluster listing while it lasts."""
    a, b = orgs
    name, name_b, name_p = f"own-{a.marker}"[:40], f"own-{b.marker}"[:40], f"platform-{a.marker}"[:40]

    printed = a.api.hl("cluster", "add", name)
    match = re.fullmatch(
        r"""sh -c '[^']*printf %s "\$token" \| "\$@"' sh \\\n"""
        r"  helm upgrade --install hypothesisloop-cluster-agent 'oci://([^' ]+)/charts/hypothesisloop-cluster-agent' \\\n"
        r"  --version '0\.1\.0\+([^' ]+)' \\\n"
        r"  --namespace hypothesisloop --create-namespace --take-ownership \\\n"
        r"  --set-file runtimeToken=/dev/stdin \\\n"
        rf"  --set-string 'clusterName={name}' \\\n"
        r"  --set-string 'apiURL=(https?://[^' ]+)' \\\n"
        r"  --set-string 'metricsURL=(https?://[^' ]+)' \\\n"
        r"  --set-string 'images\.registry=([^' ]+)' \\\n"
        r"  --set-string 'images\.tag=([^' ]+)'\n"
        rf"\nIt asks for the runtime token of {name}:\n(hls_\S+)\n", printed)
    assert match, f"hl cluster add printed {printed!r}"
    command = printed.split("\n\n", 1)[0]
    assert "hls_" not in command and "hl cluster token" not in command, "the command holds the runtime token or needs hl on the machine"
    chart_registry, chart_tag, api_url, metrics_url, registry, image_tag, shown_token = match.groups()
    assert (chart_registry, chart_tag) == (registry, image_tag), "the chart named is the one pushed beside the images it runs"
    token = a.api.hl("cluster", "token", name).strip()
    assert token == admin.cluster_token(name), "the token printed is the one derived for the cluster's name"
    assert shown_token == token, "the token shown below the command is the cluster's"
    unregistered = a.api.hl_expect("cluster", "token", f"never-{a.marker}"[:40])
    assert unregistered.returncode != 0 and "hls_" not in unregistered.stdout, "hl cluster token registered a name to print its token"
    assert b.api.hl_expect("cluster", "token", name).returncode != 0, "org B read the runtime token of org A's cluster"
    for address in (api_url, metrics_url):
        assert "localhost" not in address and "127.0.0.1" not in address and "REPLACE" not in address, (
            f"hl cluster add printed {address}, which no runtime inside a cluster reaches"
        )
    assert a.api.hl("cluster", "add", name) == printed, "adding again prints the same"
    registered = a.api.post_json(f"/orgs/{a.org_id}/clusters", {"name": name})
    assert (registered["runtime_token"], registered["api_url"], registered["metrics_url"]) == (token, api_url, metrics_url)
    assert (registered["registry"], registered["image_tag"]) == (registry, image_tag), "the command names the images the platform answered"

    taken = b.api.post(f"/orgs/{b.org_id}/clusters", {"name": name})
    assert taken.status_code == 409 and "hls_" not in taken.text, f"org B registering org A's cluster name: {taken.status_code} {taken.text}"
    assert b.api.post(f"/orgs/{a.org_id}/clusters", {"name": f"other-{b.marker}"[:40]}).status_code == 404
    token_b = b.api.post_json(f"/orgs/{b.org_id}/clusters", {"name": name_b})["runtime_token"]
    assert token_b != token

    runtime, runtime_b = FakeRuntime(a.api, name, token), FakeRuntime(b.api, name_b, token_b)
    assert runtime.reconcile() == []
    assert runtime.api.post(f"/internal/clusters/{name_b}/reconcile", _report(runtime.cluster_id)).status_code == 403
    stolen = runtime_b.api.post(f"/internal/clusters/{name_b}/reconcile", _report(runtime.cluster_id))
    assert stolen.status_code == 409, f"org B's runtime stating org A's cluster id: {stolen.status_code} {stolen.text}"
    changed = runtime.api.post(f"/internal/clusters/{name}/reconcile", _report(f"other-{runtime.cluster_id}"))
    assert changed.status_code == 409, f"org A's runtime stating a second cluster id: {changed.status_code} {changed.text}"
    malformed = runtime.api.post(f"/internal/clusters/{name}/reconcile", _report("../" + runtime.cluster_id))
    assert malformed.status_code == 422, f"a cluster id that is a path: {malformed.status_code} {malformed.text}"

    listed = {c["name"]: c for c in _lines(a.api.hl("cluster", "list"))}
    assert listed[name] == {**listed[name], "org_id": a.org_id, "owned": True, "connected": True, "cluster_id": runtime.cluster_id}
    assert name not in b.api.get(f"/orgs/{b.org_id}/clusters").text
    assert api.get(f"/orgs/org-never-made-{uuid.uuid4().hex[:8]}/clusters").status_code == 404, "staff list the clusters of an org that does not exist"

    assert name in _capacity_names(a.api) and name in _capacity_names(api)
    assert name not in _capacity_names(b.api), "org B reads the capacity of org A's cluster"
    platform_clusters = {c["name"] for c in api.get_json(f"/orgs/{DEFAULT_ORG}/clusters")["clusters"] if c["owned"]}
    assert (_capacity_names(a.api) & {c["cluster_name"] for c in api.internal_clusters()}) - platform_clusters == {name}, (
        "org A reads the capacity of a cluster that is neither its own nor the platform's"
    )

    # A cluster's settings are fields of the named cluster: a change names the ones that change.
    def settings() -> dict:
        own = next(c for c in a.api.get_json(f"/orgs/{a.org_id}/clusters")["clusters"] if c["name"] == name)
        return {k: v for k, v in own.items() if k.endswith("_seconds") or k == "max_speculative_accelerators"}

    own_path = f"/orgs/{a.org_id}/clusters/{name}"
    assert json.loads(a.api.hl("cluster", "settings", name, "--stuck-pending-timeout", "600"))["stuck_pending_timeout_seconds"] == 600
    assert a.api.patch(own_path, {"max_speculative_accelerators": 4}).status_code == 200
    assert a.api.patch(own_path, {"max_speculative_accelerators": 0}).status_code == 200
    assert settings() == {"stuck_pending_timeout_seconds": 600}, "a change touched a setting it did not name, or 0 did not clear one"
    for r in (b.api.patch(own_path, {"stuck_pending_timeout_seconds": 1}), b.api.patch(f"/orgs/{b.org_id}/clusters/{name}", {"stuck_pending_timeout_seconds": 1})):
        assert r.status_code == 404, f"org B on the settings of org A's cluster: {r.status_code} {r.text}"
    assert a.api.patch(own_path, {"scale_up_timeout_seconds": 1800}).status_code == 422
    assert a.api.get(f"/clusters/{runtime.cluster_id}/settings").status_code == 404, "the route keyed by what a runtime reports is still served"
    assert settings() == {"stuck_pending_timeout_seconds": 600}

    agent_a, pe_a = _running_org(a, run_id)
    agent_b, pe_b = _running_org(b, run_id)
    # Org B turns off the platform's clusters it can use, so that its own, which is away, is all
    # its job may wait for beside org A's free one.
    for platform_cluster in platform_clusters:
        b.api.hl("cluster", "disable", platform_cluster)
    job_a = a.api.submit_job(pe_a, agent_a, job_overrides=JOB_SHAPE)
    job_b = b.api.submit_job(pe_b, agent_b, job_overrides=JOB_SHAPE)
    try:
        eventually("org A's job is handed to org A's cluster", runtime.reconcile, deadline=deadline, interval=2,
                   accept=lambda exps: job_a in {e["id"] for e in exps})
        assert a.api.experiment(job_a)["cluster_name"] == name

        def waiting_for_a_cluster() -> dict:
            runtime.reconcile()
            return b.api.experiment(job_b)

        # Org B's own cluster is registered and away, so its job waits for it and is told nothing
        # of a missing cluster. The reconciles give admission ticks in which it could be misplaced.
        for _ in range(4):
            waiting = waiting_for_a_cluster()
            assert waiting["status"] == "QUEUED" and not waiting.get("cluster_name"), f"org B's job beside org A's free cluster: {waiting}"
            assert waiting.get("not_admitted_reason") != "no_cluster_for_org", "an org whose cluster is away was told it has none"
            time.sleep(2)
        assert job_b not in runtime.handed, "org A's runtime was handed org B's job with its token and data key"
        handed = next(e for e in runtime.reconcile() if e["id"] == job_a)
        assert handed["token"] and handed["data"]["secret_access_key"], "the runtime is not handed what the workload runs with"
        kept = {k for k in ("hypothesis", "theory", "org_id", "platform_experiment_id", "hypothesis_id") if handed.get(k)}
        assert not kept, f"the runtime is handed {kept} of the job, which no workload is built from"

        # A cluster of the platform's org runs another org's job, and shows that org neither its
        # owner nor its settings key.
        runtime_p = FakeRuntime(api, name_p, admin.add_cluster(name_p, DEFAULT_ORG))

        def placed_on_the_platform_cluster() -> dict:
            runtime_p.reconcile()
            return b.api.experiment(job_b)

        eventually("org B's job is placed on the platform's cluster", placed_on_the_platform_cluster, deadline=deadline, interval=2,
                   accept=lambda exp: exp.get("cluster_name") == name_p)
        assert job_b in runtime_p.handed, "the platform cluster's runtime was not handed the job placed on it"
        assert job_b not in runtime.handed, "org A's runtime was handed org B's job"
        assert name_p in _capacity_names(b.api) and name_p in _capacity_names(a.api), "an org does not read the capacity of the platform's cluster"
        seen = b.api.get(f"/orgs/{b.org_id}/clusters")
        platform = {c["name"]: c for c in seen.json()["clusters"]}[name_p]
        assert platform == {**platform, "owned": False, "connected": True}, f"org B lists the platform's cluster as {platform}"
        assert "org_id" not in platform and "cluster_id" not in platform and runtime_p.cluster_id not in seen.text, (
            f"org B is shown the owner or the settings key of the platform's cluster: {seen.text}"
        )
        assert a.api.experiment(job_a)["cluster_name"] == name, "an org's own job moved"
        assert _status(api, "DELETE", f"/orgs/{DEFAULT_ORG}/clusters/{name_p}").status_code == 200

        # Org B removes its own cluster, which nothing ran on.
        assert a.api.hl_expect("cluster", "remove", name_b).returncode != 0, "org A removed org B's cluster"
        assert json.loads(b.api.hl("cluster", "remove", name_b))["requeued"] == []
        assert runtime_b.api.post(f"/internal/clusters/{name_b}/reconcile", _report(runtime_b.cluster_id)).status_code == 401, (
            "the runtime token of a removed cluster still works"
        )

        # Forgetting the identity lets the cluster bind again and keeps its settings, which are the name's.
        assert b.api.hl_expect("cluster", "forget-identity", name).returncode != 0, "org B cleared the identity of org A's cluster"
        assert json.loads(a.api.hl("cluster", "forget-identity", name)).get("cluster_id", "") == ""
        rebuilt = runtime.api.post(f"/internal/clusters/{name}/reconcile", _report(f"other-{runtime.cluster_id}"))
        assert rebuilt.status_code == 200, f"binding to a new identity after the old one was forgotten: {rebuilt.status_code} {rebuilt.text}"
        assert settings() == {"stuck_pending_timeout_seconds": 600}, "forgetting the identity lost the cluster's settings"

        removed = json.loads(a.api.hl("cluster", "remove", name))
        assert removed["requeued"] == [job_a], f"removing the cluster requeued {removed['requeued']}, want the job placed on it"
        assert a.api.experiment(job_a)["status"] == "QUEUED"
        assert runtime.api.post(f"/internal/clusters/{name}/reconcile", _report(runtime.cluster_id)).status_code == 401
        assert name not in a.api.get(f"/orgs/{a.org_id}/clusters").text
    finally:
        # Removing the clusters, already gone on the passing path but the platform's.
        _status(api, "DELETE", f"/orgs/{a.org_id}/clusters/{name}")
        _status(api, "DELETE", f"/orgs/{b.org_id}/clusters/{name_b}")
        _status(api, "DELETE", f"/orgs/{DEFAULT_ORG}/clusters/{name_p}")
        for org, job_id, pe_id in ((a, job_a, pe_a), (b, job_b, pe_b)):
            org.api.post(f"/experiments/{job_id}/cancel")
            org.api.close_platform_experiment(pe_id)


@pytest.mark.timeout(240)
@pytest.mark.exclusive
def test_an_org_turns_a_cluster_off_and_on_for_its_own_jobs(orgs, api: API, run_id: str, deadline: Deadline):
    """Exclusive: the platform's cluster it registers is in every org's capacity and cluster listing
    while it lasts."""
    a, b = orgs
    name = f"use-{a.marker}"[:40]
    runtime = FakeRuntime(api, name, admin.add_cluster(name, DEFAULT_ORG))
    assert runtime.reconcile() == []

    def clusters_of(org: Org) -> dict[str, dict]:
        return {c["name"]: c for c in org.api.get_json(f"/orgs/{org.org_id}/clusters")["clusters"]}

    def after_a_tick(org: Org, job_id: str):
        def read() -> dict:
            runtime.reconcile()
            return org.api.experiment(job_id)
        return read

    agent_a, pe_a = _running_org(a, run_id)
    agent_b, pe_b = _running_org(b, run_id)
    jobs: list[tuple[Org, str]] = []
    # Every other cluster org B can use, which are the platform's others in this stack. It turns
    # them off too, so that the one under test is all that decides where its jobs go.
    others = [c for c in clusters_of(b) if c != name]
    try:
        assert clusters_of(a)[name]["enabled"] is True and clusters_of(b)[name]["enabled"] is True, "a cluster does not start enabled"

        project_key = b.api.as_caller(_key(b.api, b.org_id, f"one-project-{b.marker}", b.project_id)["token"], b.org_id, b.project_id)
        for verb in ("disable", "enable"):
            assert _status(project_key, "POST", f"/orgs/{b.org_id}/clusters/{name}/{verb}").status_code == 403, "a key of one project decided for its org"
            assert _status(b.api, "POST", f"/orgs/{a.org_id}/clusters/{name}/{verb}").status_code == 404, "org B decided for org A"
            assert _status(b.api, "POST", f"/orgs/{DEFAULT_ORG}/clusters/{name}/{verb}").status_code in (403, 404), "org B decided for the platform"
            assert _status(a.api, "POST", f"/orgs/{a.org_id}/clusters/never-{a.marker}/{verb}").status_code == 404, "a name nobody registered was found"
        assert clusters_of(a)[name]["enabled"] is True and clusters_of(b)[name]["enabled"] is True, "a refused request turned the cluster off"

        for other in others:
            b.api.hl("cluster", "disable", other)
        running = b.api.submit_job(pe_b, agent_b, job_overrides=JOB_SHAPE)
        jobs.append((b, running))
        eventually("org B's job is placed on the platform's cluster", after_a_tick(b, running), deadline=deadline, interval=2,
                   accept=lambda exp: exp.get("cluster_name") == name)

        off = json.loads(b.api.hl("cluster", "disable", name))
        assert off == {**off, "name": name, "enabled": False}, f"hl cluster disable answered {off}"
        assert "org_id" not in off and "cluster_id" not in off, f"org B is shown the owner or the settings key of the cluster it turned off: {off}"
        assert json.loads(b.api.hl("cluster", "disable", name)) == off, "turning it off again answered something else"
        listed = {c["name"]: c for c in _lines(b.api.hl("cluster", "list"))}
        assert listed[name]["enabled"] is False, f"hl cluster list shows the cluster org B turned off as {listed[name]}"
        assert clusters_of(a)[name]["enabled"] is True, "org B turning the platform's cluster off turned it off for org A"
        assert name not in _capacity_names(b.api), "org B reads the capacity of a cluster it turned off"
        assert name in _capacity_names(a.api), "org A lost the capacity of the platform's cluster when org B turned it off"

        # What was placed before stays: the runtime is still told to run it, and it is not requeued.
        waiting = b.api.submit_job(pe_b, agent_b, job_overrides=JOB_SHAPE)
        jobs.append((b, waiting))
        eventually("org B's next job says every cluster of its org is turned off", after_a_tick(b, waiting), deadline=deadline, interval=2,
                   accept=lambda exp: exp.get("not_admitted_reason") == "clusters_disabled")
        for _ in range(4):
            queued = after_a_tick(b, waiting)()
            assert queued["status"] == "QUEUED" and not queued.get("cluster_name"), f"org B's job beside a cluster its org turned off: {queued}"
            time.sleep(2)
        assert waiting not in runtime.handed, "the runtime was handed a job of an org that turned its cluster off"
        kept = b.api.experiment(running)
        assert kept["cluster_name"] == name and kept["status"] in ("SUBMITTED", "RUNNING"), f"the job placed before the cluster was turned off: {kept}"
        assert running in {e["id"] for e in runtime.reconcile()}, "the runtime is no longer told to run the job placed before"

        # The other org is placed there as before.
        of_a = a.api.submit_job(pe_a, agent_a, job_overrides=JOB_SHAPE)
        jobs.append((a, of_a))
        eventually("org A's job is placed on the platform's cluster", after_a_tick(a, of_a), deadline=deadline, interval=2,
                   accept=lambda exp: exp.get("cluster_name") == name)

        # The node holds two jobs: the one placed before makes room, and the cluster is turned on.
        b.api.cancel_job(running)
        on = json.loads(b.api.hl("cluster", "enable", name))
        assert on["enabled"] is True and json.loads(b.api.hl("cluster", "enable", name)) == on
        assert clusters_of(b)[name]["enabled"] is True and name in _capacity_names(b.api)
        eventually("org B's waiting job is placed once the cluster is on again", after_a_tick(b, waiting), deadline=deadline, interval=2,
                   accept=lambda exp: exp.get("cluster_name") == name)
    finally:
        for other in others:
            _status(b.api, "POST", f"/orgs/{b.org_id}/clusters/{other}/enable")
        # Removing the cluster also forgets who turned it off.
        _status(api, "DELETE", f"/orgs/{DEFAULT_ORG}/clusters/{name}")
        for org, job_id in jobs:
            org.api.post(f"/experiments/{job_id}/cancel")
        for org, pe_id in ((a, pe_a), (b, pe_b)):
            org.api.close_platform_experiment(pe_id)


@pytest.mark.timeout(240)
@pytest.mark.exclusive
def test_an_org_ranks_its_clusters_and_its_jobs_follow_the_order(orgs, api: API, run_id: str, deadline: Deadline):
    """Exclusive: its jobs fit any cluster of org A that reports the accelerator, and for a moment
    the platform has a cluster of its own here."""
    a, b = orgs
    # The names sort against the priorities they are given, so placement by name fails the test.
    low, high, platform = f"a-low-{a.marker}"[:40], f"z-high-{a.marker}"[:40], f"platform-rank-{a.marker}"[:40]
    runtimes = {
        name: FakeRuntime(a.api, name, a.api.post_json(f"/orgs/{a.org_id}/clusters", {"name": name})["runtime_token"])
        for name in (low, high)
    }

    def path(name: str) -> str:
        return f"/orgs/{a.org_id}/clusters/{name}"

    order_path = f"/orgs/{a.org_id}/clusters/order"

    def ranked() -> list[str]:
        return [c["name"] for c in _lines(a.api.hl("cluster", "list")) if c["name"] in runtimes]

    def others() -> list[str]:
        """The clusters org A ranks besides the two of this test, in their order."""
        return [c["name"] for c in a.api.get_json(f"/orgs/{a.org_id}/clusters")["clusters"] if c["owned"] and c["name"] not in runtimes]

    def placed(job_id: str) -> str:
        def read() -> dict:
            for runtime in runtimes.values():
                runtime.reconcile()
            return a.api.experiment(job_id)
        return eventually(f"job {job_id} is placed on one of org A's clusters", read, deadline=deadline, interval=2,
                          accept=lambda exp: exp.get("cluster_name") in runtimes)["cluster_name"]

    agent, pe_id = _running_org(a, run_id)
    jobs: list[str] = []

    def submit() -> str:
        jobs.append(a.api.submit_job(pe_id, agent, job_overrides=JOB_SHAPE))
        return jobs[-1]

    try:
        for runtime in runtimes.values():
            assert runtime.reconcile() == []
        assert ranked() == [low, high], "clusters that were never ordered are not listed by name"
        assert "priority" not in a.api.get(f"/orgs/{a.org_id}/clusters").text, "the list answers the number the order is kept as"

        for names in ([high], [high, high, low], [high, low, "never-registered"], []):
            refused = _status(a.api, "PUT", order_path, {"names": names and [*names, *others()]})
            assert refused.status_code == 422, f"PUT order {names}: {refused.status_code} {refused.text}"
        assert _status(a.api, "PATCH", path(high), {"priority": 5}).status_code == 422, "a priority is still taken on a cluster"
        assert _status(b.api, "PUT", order_path, {"names": [high, low]}).status_code == 404, "org B ordered org A's clusters"
        assert a.api.hl_expect("cluster", "order", high).returncode != 0
        assert ranked() == [low, high], "a refused request changed the order"

        answered = [c["name"] for c in _lines(a.api.hl("cluster", "order", high, low, *others()))]
        assert answered[:2] == [high, low], f"hl cluster order answered {answered}"
        assert ranked() == [high, low], "the listing is not in placement order"

        # Each faked node holds two jobs: the higher cluster fills first, then the lower takes one.
        first, second = submit(), submit()
        assert placed(first) == high and placed(second) == high, "jobs did not go to the cluster the org ranked first"
        third = submit()
        assert placed(third) == low, "a job waited for the full higher cluster, or went nowhere, with room on the lower one"

        # The order changes for what is placed from now on. What is placed stays.
        a.api.hl("cluster", "order", low, high, *others())
        assert ranked() == [low, high]
        a.api.cancel_job(first)
        fourth = submit()
        assert placed(fourth) == low, "a job went to the cluster that is now ranked second, with room on the first"
        assert a.api.experiment(second)["cluster_name"] == high, "a job that was placed moved when the order changed"

        # A cluster of the platform's org comes after every org's own and is in no other org's order.
        FakeRuntime(api, platform, admin.add_cluster(platform, DEFAULT_ORG)).reconcile()
        seen = [c["name"] for c in a.api.get_json(f"/orgs/{a.org_id}/clusters")["clusters"]]
        assert seen.index(platform) > max(seen.index(low), seen.index(high)), f"the platform's cluster is listed before an org's own: {seen}"
        refused = _status(a.api, "PUT", order_path, {"names": [platform, low, high, *others()]})
        assert refused.status_code == 422, f"an order naming the platform's cluster: {refused.status_code} {refused.text}"
    finally:
        _status(api, "DELETE", f"/orgs/{DEFAULT_ORG}/clusters/{platform}")
        for name in runtimes:
            _status(api, "DELETE", path(name))
        for job_id in jobs:
            a.api.post(f"/experiments/{job_id}/cancel")
        a.api.close_platform_experiment(pe_id)


@pytest.mark.parallel
def test_only_staff_see_every_org(orgs, api: API):
    a, b = orgs
    me = api.get_json("/me")
    assert me["kind"] == "staff" and {a.org_id, b.org_id, "default"} <= {o["id"] for o in me["orgs"]}
    assert api.get(f"/orgs/{a.org_id}/api-keys").status_code == 200
    r = api.post(f"/orgs/{a.org_id}/api-keys", {"name": "by a staff token"})
    assert r.status_code == 403, f"a staff token that is no person created a key: {r.status_code} {r.text}"
    created = api.post("/orgs", {"name": f"by staff {a.marker}", "owner_user_id": a.subject})
    assert created.status_code == 201, created.text
    assert api.post("/orgs", {"name": "ownerless"}).status_code == 422
    assert api.post("/orgs", {"name": "x", "owner_user_id": f"user-nobody-{uuid.uuid4().hex}"}).status_code == 404
    assert created.json()["id"] not in a.api.get("/me").text, "an org-wide key reached an org made after it"


@pytest.mark.exclusive
def test_tokens_the_platform_derives_reach_no_account(orgs, api: API, run_id: str):
    """Exclusive: its platform experiment's baseline runs on the default org's clusters."""
    a, _ = orgs
    agent, pe_id = _running_org(a, run_id)
    try:
        for scope in (f"agent:{pe_id}/{agent}", f"coordinator:{pe_id}"):
            caller = api.as_caller(admin.token(scope))
            for method, path, body in [
                ("GET", "/me", None), ("POST", "/orgs", {"name": "x"}), ("PATCH", f"/orgs/{a.org_id}", {"name": "x"}),
                ("GET", f"/orgs/{a.org_id}/projects", None), ("POST", f"/orgs/{a.org_id}/projects", {"name": "x"}),
                ("PATCH", f"/projects/{a.project_id}", {"name": "x"}), ("GET", f"/orgs/{a.org_id}/api-keys", None),
                ("POST", f"/orgs/{a.org_id}/api-keys", {"name": "x"}), ("GET", f"/orgs/{a.org_id}/clusters", None),
                ("POST", f"/orgs/{a.org_id}/clusters", {"name": f"x-{a.marker}"[:40]}),
            ]:
                r = _status(caller, method, path, body)
                assert r.status_code == 403, f"{scope} {method} {path}: {r.status_code} {r.text}"
        assert requests.get(f"{api.base_url}/me", timeout=30).status_code == 401
    finally:
        a.api.close_platform_experiment(pe_id)
