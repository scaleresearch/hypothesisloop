"""The repository a platform experiment works on: how an org's GitHub connection is reached, what
an experiment may name, who reaches it through the platform's git proxy and what they may push,
and what code a job may run.

Every boundary around an org's installations holds whichever git host the deployment has. On a
plain host one test names, clones and pushes a repository of the local git server. On GitHub a
real connect needs a person to authorise the App in a browser, so the one test that clones and
pushes has hl-admin connect the installation instead, which asks GitHub as the App that it
exists. It runs only when the environment names a sandbox repository:

    TEST_GIT_HOST_GITHUB_INSTALLATION   the installation of the App that covers it
    TEST_GIT_HOST_GITHUB_REPOSITORY     owner/name, a repository tests may push branches to
"""
from __future__ import annotations

import os
import re
import uuid
from dataclasses import dataclass

import pytest
import requests

from conftest import make_agent
from support import admin
from support.api import API, token_scope


@dataclass
class Org:
    api: API
    org_id: str
    project_id: str


def _org(api: API, run_id: str, label: str) -> Org:
    subject = f"user-repo{label}{uuid.uuid4().hex[:10]}-{run_id}"
    home = admin.add_user(subject, f"Repo org {label} {run_id}")
    key = admin.add_api_key(subject, home["org_id"])
    return Org(api.as_caller(key, home["org_id"], home["project_id"]), home["org_id"], home["project_id"])


@pytest.fixture(scope="module")
def orgs(api: API, run_id: str):
    return _org(api, run_id, "a"), _org(api, run_id, "b")


@pytest.fixture(scope="module")
def run(orgs, run_id: str):
    """A platform experiment of org a with one self-run agent, and that agent's own caller. The
    org owns no cluster, so it is created while the default org's are shared with every org, and
    a test that uses it is exclusive."""
    a, _ = orgs
    with a.api.clusters_shared():
        pe_id = a.api.create_platform_experiment(f"repo-{run_id}", 1, 2)
    agent = a.api.add_agents(pe_id)[0]
    yield pe_id, a.api.as_caller(agent["token"], a.org_id, a.project_id)
    a.api.close_platform_experiment(pe_id)


# What every create states itself, whatever the repository's experiment file says.
RUN = {"budget_accelerator_hours": 1, "max_agents": 2, "duration_hours": 24}


def _call(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


START = {"return_to": "/settings"}
FINISH = {"state": "a-state-never-issued", "installation_id": 1}


def _installations(org: Org) -> list[dict]:
    return org.api.get_json(f"/orgs/{org.org_id}/github-installations")


@pytest.mark.parallel
def test_an_org_starts_with_no_github_connected(orgs):
    a, _ = orgs
    assert _installations(a) == []
    gone = _call(a.api, "DELETE", f"/orgs/{a.org_id}/github-installations/1")
    assert gone.status_code == 404 and gone.json()["reason"] == "not_found", gone.text


@pytest.mark.parallel
def test_another_orgs_github_reads_as_an_org_that_does_not_exist(orgs):
    a, b = orgs
    for method, suffix, body in (
        ("GET", "github-installations", None), ("POST", "github-install-states", START),
        ("DELETE", "github-installations/1", None), ("GET", "github-repositories", None),
    ):
        foreign = _call(a.api, method, f"/orgs/{b.org_id}/{suffix}", body)
        absent = _call(a.api, method, f"/orgs/org_does-not-exist/{suffix}", body)
        assert foreign.status_code == absent.status_code == 404, (method, suffix, foreign.text, absent.text)
        assert foreign.json() == absent.json(), (method, suffix, foreign.text)


@pytest.mark.parallel
def test_connect_is_refused_without_proof_from_github(orgs):
    """A deployment whose git host is not GitHub says it has no App. With one, an install state
    is issued once and only the person it was issued to finishes with it: a made-up one, or one
    used before, connects nothing."""
    a, _ = orgs
    app = _call(a.api, "GET", "/github/app")
    finish = _call(a.api, "POST", "/github/installations", FINISH)
    if app.status_code == 503:
        for refused in (app, finish, _call(a.api, "GET", f"/orgs/{a.org_id}/github-repositories"),
                        _call(a.api, "POST", f"/orgs/{a.org_id}/github-install-states", START)):
            assert refused.status_code == 503 and refused.json()["reason"] == "github_not_configured", refused.text
            assert "GitHub App" in refused.json()["message"], refused.text
    else:
        assert app.status_code == 200 and set(app.json()) >= {"slug", "install_url", "new_repository_url"}, app.text
        assert finish.status_code == 422 and finish.json()["reason"] == "install_state_invalid", finish.text
        started = _call(a.api, "POST", f"/orgs/{a.org_id}/github-install-states", START)
        assert started.status_code == 201, started.text
        state = started.json()["state"]
        assert started.json()["install_url"].endswith("?state=" + state), started.text
        # GitHub never made installation 1 for this App: the state is spent on it all the same.
        first = _call(a.api, "POST", "/github/installations", {"state": state, "installation_id": 1})
        assert first.status_code in (422, 502), first.text
        again = _call(a.api, "POST", "/github/installations", {"state": state, "installation_id": 1})
        assert again.status_code == 422 and again.json()["reason"] == "install_state_invalid", again.text
        for bad in ({**START, "return_to": "//elsewhere.example"}, {**START, "return_to": ""}):
            assert _call(a.api, "POST", f"/orgs/{a.org_id}/github-install-states", bad).status_code == 422, bad
    assert _installations(a) == []


@pytest.mark.exclusive
def test_an_agent_token_reaches_nothing_of_the_orgs_github(orgs, run):
    a, _ = orgs
    _, agent = run
    for method, path, body in (
        ("GET", "/github/app", None), ("GET", f"/orgs/{a.org_id}/github-installations", None),
        ("POST", f"/orgs/{a.org_id}/github-install-states", START), ("POST", "/github/installations", FINISH),
        ("DELETE", f"/orgs/{a.org_id}/github-installations/1", None),
        ("GET", f"/orgs/{a.org_id}/github-repositories", None),
    ):
        assert _call(agent, method, path, body).status_code == 403, (method, path)


@pytest.mark.exclusive
def test_no_credential_of_the_git_host_is_handed_out_and_the_git_proxy_serves_only_what_the_platform_launched(api: API, orgs, run):
    a, _ = orgs
    pe_id, agent = run
    gone = _call(agent, "POST", "/git-credentials", {"protocol": "https", "host": "github.com", "path": "acme/model.git"})
    assert gone.status_code in (404, 405), gone.text
    refs = f"/git/{pe_id}.git/info/refs?service=git-upload-pack"
    served = _call(agent, "GET", refs)
    assert served.status_code == 200 and served.headers["Content-Type"] == "application/x-git-upload-pack-advertisement", served.text
    for caller in (a.api, api):
        refused = _call(caller, "GET", refs)
        assert refused.status_code == 403 and "password" not in refused.text, refused.text
    unauthenticated = requests.get(a.api._url(refs), timeout=30)
    assert unauthenticated.status_code == 401 and unauthenticated.headers["WWW-Authenticate"].startswith("Basic "), unauthenticated.text
    assert _call(agent, "GET", "/git/pe_does-not-exist.git/info/refs?service=git-upload-pack").status_code == 404


@pytest.mark.parallel
def test_a_job_runs_only_a_commit_of_its_experiments_repository(api: API, experiment, run_id: str):
    agent = make_agent(api, run_id, "code-ref")
    pe_id = experiment("code-ref", [agent], budget=1.0)
    job_id = f"code-ref-{uuid.uuid4().hex[:8]}"
    body = api.submission_body_for_id(job_id, pe_id, agent)
    body["metadata"]["code_ref"] = "https://git.example.invalid/elsewhere.git@" + "a" * 40
    refused = api.post_experiment_body(body)
    assert refused.status_code == 422 and refused.json()["reason"] == "code_ref_not_in_repository", refused.text
    assert api.get_json(f"/platform-experiments/{pe_id}")["repository_url"] in refused.json()["message"], refused.text
    assert api.get(f"/experiments/{job_id}").status_code == 404, "the refused job was stored"


@pytest.mark.exclusive
def test_a_create_states_what_only_the_caller_can_and_an_update_only_what_changes(orgs, run):
    """A create states its name, description and metrics and nothing reads them from a repository,
    and an update changes the fields it names and no others."""
    a, _ = orgs
    pe_id, agent = run
    partial = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "name": "only a name"})
    assert partial.status_code == 422 and partial.json()["reason"] == "invalid_experiment" and "description" in partial.json()["message"], partial.text
    from_repo = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "from_repo": True})
    assert from_repo.status_code == 422 and "from_repo" in from_repo.text, from_repo.text
    before = agent.get_json(f"/platform-experiments/{pe_id}")
    assert _call(a.api, "PATCH", f"/platform-experiments/{pe_id}", {"from_repo": True, "name": "x"}).status_code == 422
    assert _call(a.api, "PATCH", f"/platform-experiments/{pe_id}", {}).status_code == 422
    assert _call(a.api, "PUT", f"/platform-experiments/{pe_id}", {"name": "x"}).status_code == 405
    amended = _call(a.api, "PATCH", f"/platform-experiments/{pe_id}", {"description": "amended"})
    assert amended.status_code == 200, amended.text
    after = agent.get_json(f"/platform-experiments/{pe_id}")
    assert after["description"] == "amended"
    unchanged = ("name", "budget_accelerator_hours", "max_agents", "metrics", "report_interval_seconds", "starts_at", "duration_hours")
    assert {key: after[key] for key in unchanged} == {key: before[key] for key in unchanged}


@pytest.mark.parallel
def test_a_repository_is_checked_after_the_project_and_must_be_one_the_org_reaches(orgs, run_id: str):
    a, b = orgs
    whole = {**RUN, "name": f"repo-named-{run_id}", "description": "names a repository", "report_interval_seconds": 10,
             "metrics": [{"key": "val_accuracy", "direction": "maximize"}]}
    foreign = _call(a.api, "POST", "/platform-experiments", {**whole, "project_id": b.project_id, "repository": "acme/model"})
    assert foreign.status_code == 404 and foreign.json() == {"message": "project not found"}, foreign.text

    named = _call(a.api, "POST", "/platform-experiments", {**whole, "project_id": a.project_id, "repository": "acme/model"})
    if _call(a.api, "GET", "/github/app").status_code == 503:
        assert named.status_code == 422 and named.json()["reason"] == "repository_not_reachable", named.text
    else:
        assert named.status_code == 422 and named.json()["reason"] == "installation_not_connected", named.text
        address = _call(a.api, "POST", "/platform-experiments", {**whole, "project_id": a.project_id, "repository": "https://github.com/acme/model"})
        assert address.status_code == 422 and address.json()["reason"] == "invalid_repository", address.text
    listed = a.api.get_json("/platform-experiments", params={"project_id": a.project_id, "limit": 200})
    assert all(pe["name"] != whole["name"] for pe in listed), "a refused create left a platform experiment behind"


SANDBOX_ENV = ("TEST_GIT_HOST_GITHUB_INSTALLATION", "TEST_GIT_HOST_GITHUB_REPOSITORY")


def _own_branch(agent: API, pe_id: str, run_id: str) -> str:
    """A branch of the agent's own, as hl hypothesis checkout names it: under its name."""
    signup = agent.get_json(f"/platform-experiments/{pe_id}/signups/{token_scope(agent.token)[2]}")
    return f"hl/{pe_id}/{re.sub(r'[^a-z0-9]+', '-', signup['name'].lower()).strip('-')}/e2e-{run_id}"


@pytest.mark.exclusive
def test_on_a_plain_git_host_a_repository_is_named_and_an_agent_pushes_its_branch_to_it(orgs, run, run_id: str, tmp_path):
    a, _ = orgs
    if _call(a.api, "GET", "/github/app").status_code != 503:
        pytest.skip("this stack keeps its code on GitHub, which the sandbox test below covers")
    pe_id, agent = run
    read = agent.get_json(f"/platform-experiments/{pe_id}")
    repository, address = read["repository"], read["repository_url"]
    assert address.endswith("/" + repository), read

    # The platform creates no repository on such a host and reads no file from one.
    whole = {**RUN, "project_id": a.project_id, "name": f"repo-plain-{run_id}", "description": "names a repository",
             "metrics": [{"key": "val_accuracy", "direction": "maximize"}]}
    # A repository is made for a platform experiment that could run, so the org is given a cluster.
    with a.api.clusters_shared():
        for body, reason in (
            (whole, "repository_required"),
            ({**whole, "repository": f"no-such-{run_id}.git"}, "repository_not_reachable"),
            ({**whole, "repository": address}, "invalid_repository"),
        ):
            refused = _call(a.api, "POST", "/platform-experiments", body)
            assert refused.status_code == 422 and refused.json()["reason"] == reason, (reason, refused.text)
    unread = _call(a.api, "GET", f"/projects/{a.project_id}/experiment-file?repository={repository}")
    assert unread.status_code == 422 and unread.json()["reason"] == "experiment_file_unsupported", unread.text

    # git reaches the repository through the proxy, and the clone's origin stays its address.
    cloned = agent.git(read, tmp_path, "clone", address, "work")
    assert cloned.returncode == 0, cloned.stderr
    work = tmp_path / "work"
    assert agent.git(read, work, "config", "--get", "remote.origin.url").stdout.strip() == address
    branch = _own_branch(agent, pe_id, run_id)
    assert agent.git(read, work, "checkout", "-b", branch).returncode == 0
    (work / "e2e.txt").write_text(f"pushed by {run_id}\n")
    assert agent.git(read, work, "add", "e2e.txt").returncode == 0
    assert agent.git(read, work, "commit", "-m", f"e2e {run_id}").returncode == 0
    pushed = agent.git(read, work, "push", "origin", branch)
    assert pushed.returncode == 0, pushed.stderr
    listed = agent.git(read, work, "ls-remote", "--heads", "origin", branch)
    assert listed.returncode == 0 and branch in listed.stdout, listed.stderr
    # Nothing but its own branches: not the default branch, not another agent's, no tag.
    for refused in ("HEAD:refs/heads/main", f"HEAD:refs/heads/hl/{pe_id}/someone-else/x", f"HEAD:refs/tags/e2e-{run_id}"):
        denied = agent.git(read, work, "push", "origin", refused)
        assert denied.returncode != 0 and "[remote rejected]" in denied.stderr, (refused, denied.stderr)
    deleted = agent.git(read, work, "push", "origin", "--delete", branch)
    assert deleted.returncode == 0, deleted.stderr


@pytest.mark.exclusive
def test_an_agent_clones_and_pushes_its_branch_and_loses_access_when_github_is_disconnected(api: API, run_id: str, tmp_path):
    sandbox = {name: os.environ.get(name, "") for name in SANDBOX_ENV}
    if not all(sandbox.values()):
        pytest.skip("no sandbox repository: set " + ", ".join(SANDBOX_ENV))
    installation, repo = (sandbox[name] for name in SANDBOX_ENV)
    org = _org(api, run_id, "live")
    if _call(org.api, "GET", "/github/app").status_code == 503:
        pytest.skip("this deployment has no GitHub App configured")
    other = _org(api, run_id, "bystander")

    connected = admin.connect_github(org.org_id, installation)
    assert connected["installation_id"] == int(installation) and connected["account_login"].lower() == repo.split("/")[0].lower(), connected
    pe_id = branch = ""
    agent = None
    try:
        assert [i["installation_id"] for i in _installations(org)] == [int(installation)]
        covered = org.api.get_json(f"/orgs/{org.org_id}/github-repositories", params={"q": repo.split("/")[1]})
        assert {"repository": repo, "installation_id": int(installation)}.items() <= next(r for r in covered if r["repository"].lower() == repo.lower()).items(), covered
        # Another org reaches neither the connection nor, through it, the repository.
        assert _call(other.api, "GET", f"/orgs/{org.org_id}/github-installations").status_code == 404
        assert _installations(other) == []

        with org.api.clusters_shared():
            pe_id = org.api.create_platform_experiment(f"repo-live-{run_id}", 1, 2, repository=repo)
        clone_url = f"https://github.com/{repo}.git"
        read = org.api.get_json(f"/platform-experiments/{pe_id}")
        assert (read["repository"], read["repository_url"]) == (repo, clone_url), read

        added = org.api.add_agents(pe_id)[0]
        agent = org.api.as_caller(added["token"], org.org_id, org.project_id)

        read = agent.get_json(f"/platform-experiments/{pe_id}")
        # Another org cannot name the repository: it connected nothing that covers it.
        named = _call(other.api, "POST", "/platform-experiments", {
            **RUN, "project_id": other.project_id, "name": f"repo-taken-{run_id}", "description": "another org's repository",
            "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "repository": repo,
        })
        assert named.status_code == 422 and named.json()["reason"] == "installation_not_connected", named.text

        cloned = agent.git(read, tmp_path, "clone", "--depth", "1", clone_url, "work")
        assert cloned.returncode == 0, cloned.stderr
        work = tmp_path / "work"
        branch = _own_branch(agent, pe_id, run_id)
        assert agent.git(read, work, "checkout", "-b", branch).returncode == 0
        (work / "e2e.txt").write_text(f"pushed by {run_id}\n")
        assert agent.git(read, work, "add", "e2e.txt").returncode == 0
        assert agent.git(read, work, "commit", "-m", f"e2e {run_id}").returncode == 0
        pushed = agent.git(read, work, "push", "origin", branch)
        assert pushed.returncode == 0, pushed.stderr
        listed = agent.git(read, work, "ls-remote", "--heads", "origin", branch)
        assert listed.returncode == 0 and branch in listed.stdout, listed.stderr
        # The default branch is no agent's.
        denied = agent.git(read, work, "push", "origin", "HEAD:refs/heads/main")
        assert denied.returncode != 0 and "[remote rejected]" in denied.stderr, denied.stderr

        # A coordinator reads and does not push.
        coordinator = org.api.as_caller(admin.token(f"coordinator:{pe_id}"), org.org_id, org.project_id)
        assert coordinator.git(read, work, "fetch", "origin").returncode == 0
        denied = coordinator.git(read, work, "push", "origin", f"{branch}:{branch}-coordinator")
        assert denied.returncode != 0 and "read-only" in denied.stderr, "a coordinator pushed a branch"

        deleted = agent.git(read, work, "push", "origin", "--delete", branch)
        assert deleted.returncode == 0, deleted.stderr
        branch = ""

        # Disconnected: the experiment still names the repository, and the proxy reaches it no more.
        assert _call(org.api, "DELETE", f"/orgs/{org.org_id}/github-installations/{installation}").status_code == 204
        after = agent.git(read, work, "fetch", "origin")
        assert after.returncode != 0 and "GitHub installation" in after.stderr, after.stderr
    finally:
        if branch:
            admin.connect_github(org.org_id, installation)
            agent.git(read, tmp_path / "work", "push", "origin", "--delete", branch)
        _call(org.api, "DELETE", f"/orgs/{org.org_id}/github-installations/{installation}")
        if pe_id:
            org.api.close_platform_experiment(pe_id)
