"""A real agent-launcher process against the control plane, renting nothing: the binary is built
with -tags e2efake, which adds the in-memory provider to its registry (no image is built that
way), and what it does is read from its own log.

Exclusive: the local control plane gives every hosted agent to the one launcher its config names,
and a launcher's question carries a digest of its own launch inputs that is part of every instance
name. A second asker of that launcher with another digest, which is what this process is beside
the tests that ask by hand, renames the instances of every hosted agent on each call.

Needs a repository to attach, as every test that hosts an agent does (see test_hosted_agents.py).
"""
from __future__ import annotations

import os
import subprocess
import time
import uuid
from pathlib import Path

import pytest

from support import admin
from support.api import API
from support.wait import eventually

pytestmark = [pytest.mark.exclusive, pytest.mark.slow, pytest.mark.timeout(420)]

REPO_ROOT = Path(__file__).resolve().parents[2]
LAUNCHER = "main"
WIND_DOWN_SECONDS = 120  # domain.AgentWindDown


@pytest.fixture(scope="module")
def launcher_binary(tmp_path_factory) -> Path:
    binary = tmp_path_factory.mktemp("agent-launcher") / "agent-launcher"
    subprocess.run(["go", "build", "-tags", "e2efake", "-o", str(binary), "./runtime/cloud/cmd/agent-launcher"],
                   cwd=REPO_ROOT, check=True)
    return binary


@pytest.fixture
def project_with_repo(api: API) -> API:
    installation, repo_id = os.environ.get("HL_E2E_GITHUB_INSTALLATION"), os.environ.get("HL_E2E_GITHUB_REPO_ID")
    if not installation or not repo_id:
        pytest.skip("hosting an agent needs a repository: set HL_E2E_GITHUB_INSTALLATION and HL_E2E_GITHUB_REPO_ID")
    if api.get("/github/app").status_code == 503:
        pytest.skip("this deployment has no GitHub App configured")
    created = api.post(f"/orgs/{api.org_id}/projects", {"name": f"launcher process {uuid.uuid4().hex[:8]}"})
    assert created.status_code == 201, f"create a project: {created.status_code} {created.text}"
    project = api.as_caller(api.token, api.org_id, created.json()["id"])
    admin.attach_repo(project.project_id, installation, repo_id)
    yield project
    project.session.delete(project._url(f"/projects/{project.project_id}/repo"), timeout=30)


class Launcher:
    """The process and what it has logged so far."""

    def __init__(self, binary: Path, workdir: Path, api_url: str, namespace: str):
        self.prefix = f"hl-agent-{namespace}-"
        self.log_path = workdir / "launcher.log"
        config = workdir / "agent-launcher.yaml"
        config.write_text(f"""
launcher_name: {LAUNCHER}
api_url: {api_url}
namespace: {namespace}
reconcile_interval_seconds: 2
boot_limit_seconds: 3600
create_pause_milliseconds: 0
provider:
  name: fake
  settings: {{}}
instance:
  command: ["run-agent"]
  cpu: "1"
  memory: 1Gi
  storage: 1Gi
model_provider:
  name: example
  env:
    - name: MODEL_CREDENTIAL
      value_env: E2E_MODEL_CREDENTIAL
""")
        self.secrets = {"LAUNCHER_TOKEN": admin.token(f"launcher:{LAUNCHER}"), "E2E_MODEL_CREDENTIAL": f"credential-{uuid.uuid4().hex}"}
        env = {"PATH": os.environ["PATH"], "AGENT_LAUNCHER_CONFIG": str(config), **self.secrets}
        self.log_file = self.log_path.open("w")
        self.process = subprocess.Popen([str(binary)], env=env, stdout=self.log_file, stderr=subprocess.STDOUT)

    def lines(self) -> list[str]:
        assert self.process.poll() is None, f"the launcher exited {self.process.returncode}:\n{self.log_path.read_text()}"
        return self.log_path.read_text().splitlines()

    def said(self, text: str) -> int:
        return sum(text in line for line in self.lines())

    def stop(self) -> None:
        self.process.terminate()
        try:
            self.process.wait(timeout=20)
        finally:
            self.log_file.close()


def test_a_launcher_process_follows_its_agents_from_joining_to_the_end_of_their_run(
        project_with_repo: API, launcher_binary: Path, tmp_path: Path, run_id: str, deadline):
    api = project_with_repo
    pe_id = api.create_platform_experiment(f"launcher-process-{run_id}", 1.0, 5)
    launcher = Launcher(launcher_binary, tmp_path, api.base_url, f"e2e{uuid.uuid4().hex[:8]}")
    try:
        first, second = (a["agent_id"] for a in api.add_agents(pe_id, count=2, hosted=True))

        def names() -> dict[str, str]:
            return {a["agent_id"]: a["instance"].get("name", "") for a in api.agents(pe_id) if a["hosted"]}

        asked = eventually("the launcher asking for both agents", names,
                           accept=lambda got: all(got.get(a) for a in (first, second)), deadline=deadline)
        for agent in (first, second):
            eventually(f"the instance of {agent}", lambda: launcher.said(f"created {launcher.prefix}{asked[agent]}"),
                       accept=lambda count: count == 1, deadline=deadline)

        # Passes with nothing to do rent nothing more.
        time.sleep(6)
        assert launcher.said("created ") == 2, launcher.lines()
        assert launcher.said("destroyed ") == 0, launcher.lines()

        # Removed before the start: its machine goes on the next pass.
        assert api.remove_agent(pe_id, first).status_code == 200
        eventually("the removed agent's instance going",
                   lambda: launcher.said(f"destroyed {launcher.prefix}{asked[first]} (no longer desired)"),
                   accept=lambda count: count == 1, deadline=deadline)

        # Closed: the survivor is told to stop on every pass, kept for its wind-down, then taken down.
        api.start_platform_experiment(pe_id)
        api.close_platform_experiment(pe_id)
        closed_at = time.monotonic()
        stop_line = f"told {launcher.prefix}{asked[second]} to stop (ending)"
        eventually("the stop that starts the last turn", lambda: launcher.said(stop_line),
                   accept=lambda count: count >= 2, deadline=deadline)
        gone_line = f"destroyed {launcher.prefix}{asked[second]} (no longer desired)"
        assert launcher.said(gone_line) == 0, "an ending instance was destroyed at once"
        eventually("the instance going after its wind-down", lambda: launcher.said(gone_line),
                   accept=lambda count: count == 1, deadline=deadline, interval=5)
        assert time.monotonic() - closed_at >= WIND_DOWN_SECONDS - 10, "the instance did not get its wind-down"
        assert launcher.said("created ") == 2, "an instance was rented for an agent whose run was over"

        # Nothing it logged holds a secret, and no pass failed.
        log = "\n".join(launcher.lines())
        for secret in launcher.secrets.values():
            assert secret not in log, "the launcher logged a secret"
        assert "hls_" not in log and "pass:" not in log, log
    finally:
        launcher.stop()
        api.close_platform_experiment(pe_id)
