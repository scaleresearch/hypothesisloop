"""A project's GitHub repository: who reaches it, and who is handed a git credential.

Most of this holds whether or not the deployment has a GitHub App: every boundary around a
repository, and that nothing is handed out for a project that has none. A real attach needs a
person to authorise the App in a browser, so the one test that clones and pushes has hl-admin
record the repository instead, which asks GitHub as the App that the installation covers it. It
runs only when the environment names a sandbox repository:

    HL_E2E_GITHUB_INSTALLATION    the installation of the App that covers it
    HL_E2E_GITHUB_REPO            owner/name, a repository tests may push branches to
    HL_E2E_GITHUB_REPO_ID         its id on GitHub
"""
from __future__ import annotations

import os
import subprocess
import uuid
from dataclasses import dataclass
from urllib.parse import urlencode

import pytest
import requests

from support import admin
from support.api import API


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
    """A platform experiment of org a with one self-run agent, and that agent's own caller."""
    a, _ = orgs
    pe_id = a.api.create_platform_experiment(f"repo-{run_id}", 1, 2)
    agent = a.api.add_agents(pe_id, count=1)[0]
    yield pe_id, a.api.as_caller(agent["token"], a.org_id, a.project_id)
    a.api.close_platform_experiment(pe_id)


# What every create states itself, whatever the repository's experiment file says.
RUN = {"budget_accelerator_hours": 1, "max_agents": 2, "ends_at": "2099-01-01T00:00:00Z"}


def _call(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


ATTACH = {
    "repo": "acme/model", "code": "a-code-github-never-issued",
    "redirect_uri": "http://localhost:3000/settings/repository",
}


def _credential(path: str, host: str = "github.com", protocol: str = "https") -> str:
    """The call git's helper makes for an address."""
    return "/git-credential?" + urlencode({"protocol": protocol, "host": host, "path": path})


ANY_REPO = _credential("acme/model.git")


@pytest.mark.parallel
def test_a_project_starts_with_no_repository(orgs):
    a, _ = orgs
    read = _call(a.api, "GET", f"/projects/{a.project_id}/repo")
    detach = _call(a.api, "DELETE", f"/projects/{a.project_id}/repo")
    for refused in (read, detach):
        assert refused.status_code == 404 and refused.json()["reason"] == "no_repository", refused.text
        assert "no repository is attached" in refused.json()["message"], refused.text


@pytest.mark.parallel
def test_another_orgs_project_repo_reads_as_a_project_that_does_not_exist(orgs):
    a, b = orgs
    for method, body in (("GET", None), ("DELETE", None), ("POST", ATTACH)):
        foreign = _call(a.api, method, f"/projects/{b.project_id}/repo", body)
        absent = _call(a.api, method, "/projects/prj_does-not-exist/repo", body)
        assert foreign.status_code == absent.status_code == 404, (method, foreign.text, absent.text)
        assert foreign.json() == absent.json() == {"error": "project not found"}, (method, foreign.text)


@pytest.mark.parallel
def test_attach_is_refused_without_proof_from_github(orgs):
    """With no App the deployment says so. With one, GitHub refuses a made-up code. Either way
    nothing is attached."""
    a, _ = orgs
    app = _call(a.api, "GET", "/github/app")
    attach = _call(a.api, "POST", f"/projects/{a.project_id}/repo", ATTACH)
    if app.status_code == 503:
        for refused in (app, attach):
            assert refused.status_code == 503 and refused.json()["reason"] == "github_not_configured", refused.text
            assert "GitHub App" in refused.json()["message"], refused.text
    else:
        assert app.status_code == 200 and set(app.json()) >= {"slug", "client_id", "install_url", "authorize_url"}, app.text
        assert attach.status_code == 400 and "code" in attach.json()["error"], attach.text
        for bad in ({**ATTACH, "repo": "https://github.com/acme/model"}, {**ATTACH, "code": ""}, {**ATTACH, "redirect_uri": ""}):
            assert _call(a.api, "POST", f"/projects/{a.project_id}/repo", bad).status_code == 400, bad
    # The installation is GitHub's to name: a body that names one is refused whole.
    assert _call(a.api, "POST", f"/projects/{a.project_id}/repo", {**ATTACH, "installation_id": 1}).status_code == 422
    assert _call(a.api, "GET", f"/projects/{a.project_id}/repo").status_code == 404


@pytest.mark.parallel
def test_an_agent_token_cannot_attach_read_or_detach(orgs, run):
    a, _ = orgs
    _, agent = run
    for method, path, body in (
        ("GET", "/github/app", None), ("GET", f"/projects/{a.project_id}/repo", None),
        ("POST", f"/projects/{a.project_id}/repo", ATTACH), ("DELETE", f"/projects/{a.project_id}/repo", None),
    ):
        assert _call(agent, method, path, body).status_code == 403, (method, path)


@pytest.mark.parallel
def test_a_git_credential_goes_only_to_what_the_platform_launched(api: API, orgs, run):
    a, _ = orgs
    _, agent = run
    for caller in (a.api, api):
        refused = _call(caller, "GET", ANY_REPO)
        assert refused.status_code == 403 and "password" not in refused.text, refused.text
    assert requests.get(a.api._url(ANY_REPO), timeout=30).status_code == 401
    nothing = _call(agent, "GET", ANY_REPO)
    assert nothing.status_code == 404 and nothing.json()["reason"] == "no_repository", nothing.text
    # The address git asks about is part of the question.
    assert _call(agent, "GET", "/git-credential").status_code == 422


@pytest.mark.parallel
def test_hl_git_credential_gives_git_nothing_when_the_project_has_no_repository(run):
    _, agent = run
    asked = "protocol=https\nhost=github.com\npath=acme/model.git\n\n"
    got = agent.hl_expect("git-credential", "get", input=asked)
    assert got.returncode == 1 and got.stdout == "" and "no repository is attached" in got.stderr, got
    for operation in ("store", "erase"):
        done = agent.hl_expect("git-credential", operation, input=asked + "password=secret\n")
        assert done.returncode == 0 and done.stdout == "" and done.stderr == "", done


@pytest.mark.parallel
def test_a_platform_experiment_without_a_repository_has_no_clone_address_and_no_from_repo(orgs, run):
    a, _ = orgs
    pe_id, agent = run
    assert "code_repo_url" not in agent.get_json(f"/platform-experiments/{pe_id}")
    created = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "from_repo": True})
    assert created.status_code == 422 and "repository" in created.json()["error"], created.text


@pytest.mark.parallel
def test_a_run_states_its_own_cost_and_an_update_is_whole(orgs, run):
    """What a run costs and how long it lasts is never read from the repository, a create without
    from_repo states everything, and a partial update is refused and clears nothing."""
    a, _ = orgs
    pe_id, agent = run
    for field in RUN:
        body = {**RUN, "project_id": a.project_id, "from_repo": True}
        del body[field]
        assert _call(a.api, "POST", "/platform-experiments", body).status_code == 422, field
    partial = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "name": "only a name"})
    assert partial.status_code == 422 and "description" in partial.json()["error"], partial.text
    before = agent.get_json(f"/platform-experiments/{pe_id}")
    for body in ({"name": "x"}, {"from_repo": True, "name": "x"}):
        assert _call(a.api, "PUT", f"/platform-experiments/{pe_id}", body).status_code == 422, body
    after = agent.get_json(f"/platform-experiments/{pe_id}")
    assert (after["name"], after["description"]) == (before["name"], before["description"])


@pytest.mark.parallel
def test_from_repo_checks_the_project_before_its_repository(orgs):
    a, b = orgs
    foreign = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": b.project_id, "from_repo": True})
    assert foreign.status_code == 404 and foreign.json() == {"error": "project not found"}, foreign.text


SANDBOX_ENV = ("HL_E2E_GITHUB_INSTALLATION", "HL_E2E_GITHUB_REPO", "HL_E2E_GITHUB_REPO_ID")


def _git(agent: API, cwd, *args: str) -> subprocess.CompletedProcess:
    """git as an agent's image configures it: hl is the credential helper and nothing else is."""
    env = agent.hl_env() | {
        "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
        "GIT_AUTHOR_NAME": "hl e2e", "GIT_AUTHOR_EMAIL": "e2e@hypothesisloop.local",
        "GIT_COMMITTER_NAME": "hl e2e", "GIT_COMMITTER_EMAIL": "e2e@hypothesisloop.local",
    }
    cmd = ["git", "-c", "credential.helper=", "-c", f"credential.https://github.com.helper=!{agent.hl_bin} git-credential",
           "-c", "credential.https://github.com.useHttpPath=true", *args]
    return subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True, timeout=120)


@pytest.mark.parallel
def test_an_agent_clones_and_pushes_its_branch_and_loses_access_on_detach(api: API, run_id: str, tmp_path):
    sandbox = {name: os.environ.get(name, "") for name in SANDBOX_ENV}
    if not all(sandbox.values()):
        pytest.skip("no sandbox repository: set " + ", ".join(SANDBOX_ENV))
    installation, repo, repo_id = (sandbox[name] for name in SANDBOX_ENV)
    org = _org(api, run_id, "live")
    if _call(org.api, "GET", "/github/app").status_code == 503:
        pytest.skip("this deployment has no GitHub App configured")
    other = _org(api, run_id, "bystander")

    admin.attach_repo(org.project_id, installation, repo_id)
    pe_id = org.api.create_platform_experiment(f"repo-live-{run_id}", 1, 2)
    other_pe = other.api.create_platform_experiment(f"repo-bystander-{run_id}", 1, 2)
    branch = ""
    try:
        attached = org.api.get_json(f"/projects/{org.project_id}/repo")
        clone_url = f"https://github.com/{repo}.git"
        assert attached["repo"] == repo and attached["clone_url"] == clone_url, attached
        assert _call(other.api, "GET", f"/projects/{org.project_id}/repo").status_code == 404

        added = org.api.add_agents(pe_id, count=1)[0]
        agent = org.api.as_caller(added["token"], org.org_id, org.project_id)
        bystander = other.api.as_caller(other.api.add_agents(other_pe, count=1)[0]["token"], other.org_id, other.project_id)
        assert agent.get_json(f"/platform-experiments/{pe_id}")["code_repo_url"] == clone_url

        mine = _credential(f"{repo}.git")
        credential = agent.get_json(mine)
        assert credential["host"] == "github.com" and credential["path"] == f"{repo}.git", {k: v for k, v in credential.items() if k != "password"}
        assert agent.get_json(mine)["password"] != credential["password"], "the same token was handed out twice"
        assert "password" in agent.get_json(_credential(f"{repo}.git/info/lfs"))
        # Nothing is minted for an address that is not the repository's.
        for elsewhere in (_credential(f"{repo}-other.git"), _credential(f"{repo}.git", host="example.com"), _credential(f"{repo}.git", protocol="http")):
            assert _call(agent, "GET", elsewhere).status_code == 404, elsewhere
        # An agent of an org with nothing attached is handed nothing, and cannot clone.
        assert _call(bystander, "GET", mine).status_code == 404
        refused = _git(bystander, tmp_path, "clone", "--depth", "1", clone_url, "bystander")
        assert refused.returncode != 0, "an agent of another org cloned the repository"

        cloned = _git(agent, tmp_path, "clone", "--depth", "1", clone_url, "work")
        assert cloned.returncode == 0, cloned.stderr
        work = tmp_path / "work"
        branch = f"hl/{pe_id}/{added['agent_id']}"
        assert _git(agent, work, "checkout", "-b", branch).returncode == 0
        (work / "e2e.txt").write_text(f"pushed by {run_id}\n")
        assert _git(agent, work, "add", "e2e.txt").returncode == 0
        assert _git(agent, work, "commit", "-m", f"e2e {run_id}").returncode == 0
        pushed = _git(agent, work, "push", "origin", branch)
        assert pushed.returncode == 0, pushed.stderr
        listed = _git(agent, work, "ls-remote", "--heads", "origin", branch)
        assert listed.returncode == 0 and branch in listed.stdout, listed.stderr

        # A coordinator reads and does not push.
        coordinator = org.api.as_caller(admin.token(f"coordinator:{pe_id}"), org.org_id, org.project_id)
        assert _git(coordinator, work, "fetch", "origin").returncode == 0
        denied = _git(coordinator, work, "push", "origin", f"{branch}:{branch}-coordinator")
        assert denied.returncode != 0, "a coordinator's read credential pushed a branch"

        deleted = _git(agent, work, "push", "origin", "--delete", branch)
        assert deleted.returncode == 0, deleted.stderr
        branch = ""

        assert _call(org.api, "DELETE", f"/projects/{org.project_id}/repo").status_code == 204
        assert _call(agent, "GET", mine).status_code == 404
        after = _git(agent, work, "fetch", "origin")
        assert after.returncode != 0, "git still fetched after the repository was detached"
    finally:
        if branch:
            admin.attach_repo(org.project_id, installation, repo_id)
            _git(agent, tmp_path / "work", "push", "origin", "--delete", branch)
        _call(org.api, "DELETE", f"/projects/{org.project_id}/repo")
        org.api.close_platform_experiment(pe_id)
        other.api.close_platform_experiment(other_pe)

