"""A job whose commit carries .hypothesisloop/Dockerfile runs in the image the platform builds from
that file on the job's image, pulled with the job's own pull secret. A file that reads from a build
context is refused at submit. One whose build fails ends its job as the file's fault with the end
of the build's output, and the same file submitted again is built again.

The platform reads the file from GitHub alone, so the module needs the sandbox repository
(TEST_GIT_HOST_GITHUB_INSTALLATION, TEST_GIT_HOST_GITHUB_REPOSITORY): each test pushes its
experiment's baseline branch there and deletes it at the end. A build needs a connected cluster that builds for org
default (the chart's builder.enabled=true). The local k3s of make e2e builds nothing, so there the build
tests skip. Where another cluster builds, such as a Tenstorrent host's, localdev/e2e.sh refuses the stack,
and the module runs with what make tt-e2e gives the suite, while nothing else uses that cluster:

    bash -c '. localdev/lib/auth.sh && export HYPOTHESISLOOP_CONFIG="$PWD/controlplane/settings/.hypothesisloop.dev.yaml" \
      API_URL=http://localhost:8081 HL_TOKEN="$(lib_hl_admin token staff)" \
      TEST_GIT_HOST_GITHUB_INSTALLATION="$(lib_env_value TEST_GIT_HOST_GITHUB_INSTALLATION)" \
      TEST_GIT_HOST_GITHUB_REPOSITORY="$(lib_env_value TEST_GIT_HOST_GITHUB_REPOSITORY)" && cd tests && uv run pytest e2e/test_environment_build.py'
"""
from __future__ import annotations

import uuid
from pathlib import Path

import pytest
import requests
import yaml

from conftest import cpu_job
from support.api import API, JobRequest, baseline_reporter
from support.wait import eventually

pytestmark = pytest.mark.parallel

METRICS = [{"key": "val_accuracy", "direction": "maximize"}]
# The standard job image of hl capacity, which every cluster that builds runs.
JOB = cpu_job(1) | {"image": "python", "memory": "512Mi", "storage": "1Gi", "max_retries": 0, "accelerator_tolerations": None}
MARKER_PATH = "/opt/hl-e2e-environment"
ENDED = ("COMPLETED", "FAILED", "EVICTED", "REJECTED", "CANCELLED")


class Run:
    """A platform experiment on the sandbox repository, before its start, with its baseline agent,
    which may submit while it is open and pushes the commits its jobs name."""

    def __init__(self, api: API, pe_id: str, agent_id: str, token: str, work: Path):
        self.api, self.pe_id, self.agent_id, self.work = api, pe_id, agent_id, work
        self.agent = api.as_caller(token, api.org_id, api.project_id)
        self.read = self.agent.get_json(f"/platform-experiments/{pe_id}")
        self.repository_url = self.read["repository_url"]
        self.branch = f"hl/{pe_id}/baseline"
        self.pushed = False

    def git(self, *args: str, cwd: Path | None = None) -> str:
        done = self.agent.git(self.read, cwd or self.work, *args)
        assert done.returncode == 0, f"git {' '.join(args)}: {done.stderr}"
        return done.stdout.strip()

    def commit(self, dockerfile: str) -> str:
        """Pushes a commit whose environment file is dockerfile on the baseline branch, and
        returns the code_ref a job of it names."""
        if not (self.work / ".git").exists():
            self.git("clone", "--depth", "1", self.repository_url, str(self.work), cwd=self.work.parent)
            self.git("checkout", "-b", self.branch)
        path = self.work / ".hypothesisloop" / "Dockerfile"
        path.parent.mkdir(exist_ok=True)
        path.write_text(dockerfile)
        self.git("add", ".hypothesisloop/Dockerfile")
        self.git("commit", "-m", f"environment of {self.pe_id}")
        self.git("push", "--force", "origin", f"HEAD:{self.branch}")
        self.pushed = True
        return f"{self.repository_url}@{self.git('rev-parse', 'HEAD')}"

    def submit(self, code_ref: str, overrides: dict | None = None) -> tuple[str, requests.Response]:
        job_id = f"env-{uuid.uuid4().hex[:8]}"
        req = JobRequest(id=job_id, agent_id=self.agent_id, platform_experiment_id=self.pe_id, hypothesis_id="",
                         code_ref=code_ref, hours=0.02, job_overrides=JOB | (overrides or {}))
        return job_id, self.api.post("/experiments", yaml.safe_load(req.to_yaml()))

    def submit_to_a_builder(self, code_ref: str, overrides: dict | None = None) -> str:
        """Submits a job of code_ref, skipping the test when no cluster builds for the org."""
        job_id, r = self.submit(code_ref, overrides)
        if r.status_code == 422 and r.json().get("reason") == "no_environment_builder":
            pytest.skip("no cluster that builds environments is open to org default (the chart's builder.enabled=true)")
        assert r.status_code == 202, r.text
        return job_id


@pytest.fixture
def run(api: API, sandbox_repository: str, pe, run_id: str, tmp_path: Path):
    if api.get("/github/app").status_code == 503:
        pytest.skip("the platform reads .hypothesisloop/Dockerfile from GitHub alone, and this stack has no GitHub App")
    pe_id = pe("environment", 1.0, 2, metrics=METRICS, repository=sandbox_repository)
    added = api.add_agents(pe_id, role="baseline", quota_tier="guaranteed")[0]
    made = Run(api, pe_id, added["agent_id"], added["token"], tmp_path / "work")
    yield made
    # The branch goes before the platform experiment closes, after which the proxy serves it no more.
    if made.pushed:
        made.git("push", "origin", "--delete", made.branch)


def _renting(api: API) -> list[str]:
    """The clusters that rent compute and take org default's jobs now."""
    on_demand = {c["cluster_name"] for c in api.capacity(api.org_id)["clusters"] if c.get("on_demand")}
    return [c["name"] for c in api.get_json(f"/orgs/{api.org_id}/clusters")["clusters"]
            if c["connected"] and c["enabled"] and c["name"] in on_demand]


def _never_rents(api: API) -> None:
    if renting := _renting(api):
        pytest.skip(f"clusters that rent take org default's jobs ({', '.join(renting)}): turn them off for the org first")


def _standing(api: API, job_id: str) -> dict:
    e = api.experiment(job_id)
    return {"status": e["status"], "environment_key": e.get("environment_key", ""),
            "waits": f"{e.get('not_admitted_reason') or ''}: {e.get('not_admitted_message') or ''}",
            "eviction_reason": e.get("eviction_reason") or ""}


def _until_ended(api: API, job_id: str, deadline, seen: list[dict]) -> dict:
    """Waits for the job to end, keeping every standing seen on the way. A build that no connected
    cluster takes skips the test."""

    def probe() -> dict:
        now = _standing(api, job_id)
        seen.append(now)
        if now["status"] == "QUEUED" and "once a live cluster that builds for its org has room" in now["waits"] and len(seen) > 120:
            pytest.skip(f"no connected cluster built the environment of {job_id} in two minutes: {now['waits']}")
        return now

    return eventually(f"job {job_id} to end", probe, accept=lambda s: s["status"] in ENDED, deadline=deadline, interval=1)


@pytest.mark.slow
@pytest.mark.timeout(900)
def test_a_job_runs_in_the_environment_its_commit_declares(api: API, run: Run, deadline):
    _never_rents(api)
    marker = uuid.uuid4().hex
    code_ref = run.commit(f"ARG BASE\nFROM ${{BASE}}\nRUN echo {marker} > {MARKER_PATH}\n")
    reporter = baseline_reporter(METRICS)
    check = (f"import os, pathlib\nfound = pathlib.Path({MARKER_PATH!r}).read_text().strip()\n"
             "assert found == os.environ['ENVIRONMENT_MARKER'], f'the job ran in an image its environment did not build: {found!r}'\n")
    job_id = run.submit_to_a_builder(code_ref, {
        "command": ["python", "-c", check + reporter["command"][2]],
        "env": reporter["env"] | {"ENVIRONMENT_MARKER": marker},
    })

    seen: list[dict] = []
    ended = _until_ended(api, job_id, deadline, seen)
    assert ended["environment_key"], ended
    assert any(s["waits"].startswith("environment_building") for s in seen), f"the job never waited for its environment: {seen[:3]}"
    # The image is private: a pod that could not pull it with the job's pull secret never runs.
    assert ended["status"] == "COMPLETED", f"{ended}\n{api.logs(job_id)}"
    assert api.metric_last(job_id, "val_accuracy") is not None, "the job reported no val_accuracy"


def test_a_file_that_copies_from_a_build_context_is_refused_at_submit(api: API, run: Run):
    code_ref = run.commit("ARG BASE\nFROM ${BASE}\nCOPY requirements.txt /tmp/\nRUN true\n")
    job_id, r = run.submit(code_ref)
    if r.status_code == 202:
        api.cancel_job(job_id)
    assert r.status_code == 422 and r.json()["reason"] == "invalid_environment_file", r.text
    assert "COPY requirements.txt /tmp/" in r.json()["message"] and "build context" in r.json()["message"], r.text
    assert api.get(f"/experiments/{job_id}").status_code == 404, "the refused job was stored"


@pytest.mark.slow
@pytest.mark.timeout(900)
def test_a_file_that_fails_to_build_rejects_its_job_and_is_built_again_when_submitted_again(api: API, run: Run, deadline):
    _never_rents(api)
    marker = uuid.uuid4().hex
    # The shell computes what the build prints, so the output in the reason is the build's own and
    # not the file's line.
    code_ref = run.commit(f"ARG BASE\nFROM ${{BASE}}\nRUN echo \"broken-$((6*7))-{marker}\" && exit 3\n")
    printed = f"broken-42-{marker}"

    first = run.submit_to_a_builder(code_ref)
    ended = _until_ended(api, first, deadline, [])
    assert ended["status"] == "REJECTED" and ended["eviction_reason"].startswith("environment_failed: "), ended
    assert ".hypothesisloop/Dockerfile of its commit did not build" in ended["eviction_reason"] and printed in ended["eviction_reason"], ended

    again = run.submit_to_a_builder(code_ref)
    seen: list[dict] = []
    ended = _until_ended(api, again, deadline, seen)
    rebuilt = [s for s in seen if s["waits"].startswith("environment_building")]
    assert rebuilt, f"the file was not built again: {seen[:3]}"
    assert any("This file failed to build before" in s["waits"] and printed in s["waits"] for s in rebuilt), rebuilt[-1]
    assert ended["status"] == "REJECTED" and printed in ended["eviction_reason"], ended
