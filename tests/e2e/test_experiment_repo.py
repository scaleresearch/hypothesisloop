"""The repository a platform experiment works on: how an org's GitHub connection is reached, what
an experiment may name, and who is handed a git credential.

Most of this holds whether or not the deployment has a GitHub App: every boundary around an org's
installations, and that nothing is handed out for an experiment that names no repository. A real
connect needs a person to authorise the App in a browser, so the one test that clones and pushes
has hl-admin connect the installation instead, which asks GitHub as the App that it exists. It
runs only when the environment names a sandbox repository:

    HL_E2E_GITHUB_INSTALLATION    the installation of the App that covers it
    HL_E2E_GITHUB_REPO            owner/name, a repository tests may push branches to
"""
from __future__ import annotations

import os
import subprocess
import uuid
from dataclasses import dataclass

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
    agent = a.api.add_agents(pe_id)[0]
    yield pe_id, a.api.as_caller(agent["token"], a.org_id, a.project_id)
    a.api.close_platform_experiment(pe_id)


# What every create states itself, whatever the repository's experiment file says.
RUN = {"budget_accelerator_hours": 1, "max_agents": 2, "ends_at": "2099-01-01T00:00:00Z"}


def _call(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


CONNECT = {"code": "a-code-github-never-issued", "redirect_uri": "http://localhost:3000/settings/github"}
# What git's helper asks a credential for.
ANY_REPO = {"protocol": "https", "host": "github.com", "path": "acme/model.git"}


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
        ("GET", "github-installations", None), ("POST", "github-installations", CONNECT),
        ("DELETE", "github-installations/1", None), ("GET", "github-repositories", None),
    ):
        foreign = _call(a.api, method, f"/orgs/{b.org_id}/{suffix}", body)
        absent = _call(a.api, method, f"/orgs/org_does-not-exist/{suffix}", body)
        assert foreign.status_code == absent.status_code == 404, (method, suffix, foreign.text, absent.text)
        assert foreign.json() == absent.json(), (method, suffix, foreign.text)


@pytest.mark.parallel
def test_connect_is_refused_without_proof_from_github(orgs):
    """With no App the deployment says so. With one, GitHub refuses a made-up code. Either way
    nothing is connected."""
    a, _ = orgs
    app = _call(a.api, "GET", "/github/app")
    connect = _call(a.api, "POST", f"/orgs/{a.org_id}/github-installations", CONNECT)
    if app.status_code == 503:
        for refused in (app, connect, _call(a.api, "GET", f"/orgs/{a.org_id}/github-repositories")):
            assert refused.status_code == 503 and refused.json()["reason"] == "github_not_configured", refused.text
            assert "GitHub App" in refused.json()["message"], refused.text
    else:
        assert app.status_code == 200 and set(app.json()) >= {"slug", "client_id", "install_url", "authorize_url"}, app.text
        assert connect.status_code == 422 and connect.json()["reason"] == "code_rejected", connect.text
        for bad in ({**CONNECT, "code": ""}, {**CONNECT, "redirect_uri": ""}, {**CONNECT, "installation_id": -1}):
            assert _call(a.api, "POST", f"/orgs/{a.org_id}/github-installations", bad).status_code == 422, bad
    assert _installations(a) == []


@pytest.mark.parallel
def test_an_agent_token_reaches_nothing_of_the_orgs_github(orgs, run):
    a, _ = orgs
    _, agent = run
    for method, path, body in (
        ("GET", "/github/app", None), ("GET", f"/orgs/{a.org_id}/github-installations", None),
        ("POST", f"/orgs/{a.org_id}/github-installations", CONNECT), ("DELETE", f"/orgs/{a.org_id}/github-installations/1", None),
        ("GET", f"/orgs/{a.org_id}/github-repositories", None),
    ):
        assert _call(agent, method, path, body).status_code == 403, (method, path)


@pytest.mark.parallel
def test_a_git_credential_goes_only_to_what_the_platform_launched(api: API, orgs, run):
    a, _ = orgs
    _, agent = run
    for caller in (a.api, api):
        refused = _call(caller, "POST", "/git-credentials", ANY_REPO)
        assert refused.status_code == 403 and refused.json()["reason"] == "forbidden" and "password" not in refused.text, refused.text
    assert requests.post(a.api._url("/git-credentials"), json=ANY_REPO, timeout=30).status_code == 401
    nothing = _call(agent, "POST", "/git-credentials", ANY_REPO)
    assert nothing.status_code == 404 and nothing.json()["reason"] == "no_repository", nothing.text
    # The address git asks about is part of the question.
    assert _call(agent, "POST", "/git-credentials", {}).status_code == 422


@pytest.mark.parallel
def test_hl_git_credential_gives_git_nothing_when_the_experiment_names_no_repository(run):
    _, agent = run
    asked = "protocol=https\nhost=github.com\npath=acme/model.git\n\n"
    got = agent.hl_expect("git-credential", "get", input=asked)
    assert got.returncode == 1 and got.stdout == "" and "names no repository" in got.stderr, got
    for operation in ("store", "erase"):
        done = agent.hl_expect("git-credential", operation, input=asked + "password=secret\n")
        assert done.returncode == 0 and done.stdout == "" and done.stderr == "", done


@pytest.mark.parallel
def test_a_platform_experiment_without_a_repository_has_no_clone_address_and_no_from_repo(orgs, run):
    a, _ = orgs
    pe_id, agent = run
    read = agent.get_json(f"/platform-experiments/{pe_id}")
    assert "repository" not in read and "repository_url" not in read, read
    created = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "from_repo": True})
    assert created.status_code == 422 and created.json()["reason"] == "no_repository", created.text


@pytest.mark.parallel
def test_a_create_states_everything_and_an_update_is_whole(orgs, run):
    """A create without from_repo states everything, and a partial update is refused and clears
    nothing."""
    a, _ = orgs
    pe_id, agent = run
    partial = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": a.project_id, "name": "only a name"})
    assert partial.status_code == 422 and "description" in partial.json()["error"], partial.text
    before = agent.get_json(f"/platform-experiments/{pe_id}")
    for body in ({"name": "x"}, {"from_repo": True, "name": "x"}):
        assert _call(a.api, "PUT", f"/platform-experiments/{pe_id}", body).status_code == 422, body
    after = agent.get_json(f"/platform-experiments/{pe_id}")
    assert (after["name"], after["description"]) == (before["name"], before["description"])


@pytest.mark.parallel
def test_a_repository_is_checked_after_the_project_and_must_be_one_the_org_reaches(orgs, run_id: str):
    a, b = orgs
    whole = {**RUN, "name": f"repo-named-{run_id}", "description": "names a repository", "report_interval_seconds": 10,
             "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "starts_at": "0001-01-01T00:00:00Z"}
    foreign = _call(a.api, "POST", "/platform-experiments", {**RUN, "project_id": b.project_id, "from_repo": True, "repository": "acme/model"})
    assert foreign.status_code == 404 and foreign.json() == {"error": "project not found"}, foreign.text

    named = _call(a.api, "POST", "/platform-experiments", {**whole, "project_id": a.project_id, "repository": "acme/model"})
    if _call(a.api, "GET", "/github/app").status_code == 503:
        assert named.status_code == 503 and named.json()["reason"] == "github_not_configured", named.text
    else:
        assert named.status_code == 422 and named.json()["reason"] == "installation_not_connected", named.text
        address = _call(a.api, "POST", "/platform-experiments", {**whole, "project_id": a.project_id, "repository": "https://github.com/acme/model"})
        assert address.status_code == 422 and address.json()["reason"] == "invalid_repository", address.text
    listed = a.api.get_json("/platform-experiments", params={"project_id": a.project_id, "limit": 200})
    assert all(pe["name"] != whole["name"] for pe in listed), "a refused create left a platform experiment behind"


SANDBOX_ENV = ("HL_E2E_GITHUB_INSTALLATION", "HL_E2E_GITHUB_REPO")


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
    pe_id = other_pe = branch = ""
    agent = None
    try:
        assert [i["installation_id"] for i in _installations(org)] == [int(installation)]
        covered = org.api.get_json(f"/orgs/{org.org_id}/github-repositories", params={"q": repo.split("/")[1]})
        assert {"repository": repo, "installation_id": int(installation)}.items() <= next(r for r in covered if r["repository"].lower() == repo.lower()).items(), covered
        # Another org reaches neither the connection nor, through it, the repository.
        assert _call(other.api, "GET", f"/orgs/{org.org_id}/github-installations").status_code == 404
        assert _installations(other) == []

        pe_id = org.api.create_platform_experiment(f"repo-live-{run_id}", 1, 2, repository=repo)
        other_pe = other.api.create_platform_experiment(f"repo-bystander-{run_id}", 1, 2)
        clone_url = f"https://github.com/{repo}.git"
        read = org.api.get_json(f"/platform-experiments/{pe_id}")
        assert (read["repository"], read["repository_url"]) == (repo, clone_url), read

        added = org.api.add_agents(pe_id)[0]
        agent = org.api.as_caller(added["token"], org.org_id, org.project_id)
        bystander = other.api.as_caller(other.api.add_agents(other_pe)[0]["token"], other.org_id, other.project_id)

        mine = {"protocol": "https", "host": "github.com", "path": f"{repo}.git"}
        minted = _call(agent, "POST", "/git-credentials", mine)
        assert minted.status_code == 201 and minted.headers["Cache-Control"] == "no-store", minted.status_code
        credential = minted.json()
        assert credential["host"] == "github.com" and credential["path"] == f"{repo}.git", {k: v for k, v in credential.items() if k != "password"}
        assert agent.post_json("/git-credentials", mine)["password"] != credential["password"], "the same token was handed out twice"
        assert "password" in agent.post_json("/git-credentials", {**mine, "path": f"{repo}.git/info/lfs"})
        # Nothing is minted for an address that is not the repository's.
        for elsewhere in ({**mine, "path": f"{repo}-other.git"}, {**mine, "host": "example.com"}, {**mine, "protocol": "http"}):
            refused = _call(agent, "POST", "/git-credentials", elsewhere)
            assert refused.status_code == 404 and refused.json()["reason"] == "not_found", (elsewhere, refused.text)
        # An agent of an experiment that names no repository is handed nothing, and cannot clone.
        assert _call(bystander, "POST", "/git-credentials", mine).status_code == 404
        refused = _git(bystander, tmp_path, "clone", "--depth", "1", clone_url, "bystander")
        assert refused.returncode != 0, "an agent of another org cloned the repository"
        # And its org cannot name the repository: it connected nothing that covers it.
        named = _call(other.api, "POST", "/platform-experiments", {
            **RUN, "project_id": other.project_id, "name": f"repo-taken-{run_id}", "description": "another org's repository",
            "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}],
            "starts_at": "0001-01-01T00:00:00Z", "repository": repo,
        })
        assert named.status_code == 422 and named.json()["reason"] == "installation_not_connected", named.text

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

        # Disconnected: the experiment still names the repository, and no credential is minted for it.
        assert _call(org.api, "DELETE", f"/orgs/{org.org_id}/github-installations/{installation}").status_code == 204
        cut = _call(agent, "POST", "/git-credentials", mine)
        assert cut.status_code == 409 and cut.json()["reason"] == "installation_not_connected", cut.text
        after = _git(agent, work, "fetch", "origin")
        assert after.returncode != 0, "git still fetched after GitHub was disconnected"
    finally:
        if branch:
            admin.connect_github(org.org_id, installation)
            _git(agent, tmp_path / "work", "push", "origin", "--delete", branch)
        _call(org.api, "DELETE", f"/orgs/{org.org_id}/github-installations/{installation}")
        for api_, pe in ((org.api, pe_id), (other.api, other_pe)):
            if pe:
                api_.close_platform_experiment(pe)
