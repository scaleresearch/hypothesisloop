from __future__ import annotations

import os
import subprocess
import time
from pathlib import Path

import pytest

from support import admin
from support.api import API
from support.wait import eventually

# Exclusive: this process asks as the one launcher the local control plane gives hosted agents to
# and reports what it holds. A machine another asker reported and this one does not is an exit,
# and a question with another launch digest renames every instance.
pytestmark = [pytest.mark.exclusive, pytest.mark.slow, pytest.mark.timeout(420)]

REPO_ROOT = Path(__file__).resolve().parents[2]
LAUNCHER = "main"
WIND_DOWN_SECONDS = 120  # domain.AgentWindDown


@pytest.fixture(scope="module")
def launcher_binary(tmp_path_factory) -> Path:
    """Built with -tags e2efake, which adds the in-memory provider to its registry: nothing is
    rented and no image is built."""
    binary = tmp_path_factory.mktemp("agent-launcher") / "agent-launcher"
    subprocess.run(["go", "build", "-tags", "e2efake", "-o", str(binary), "./runtime/cloud/cmd/agent-launcher"],
                   cwd=REPO_ROOT, check=True)
    return binary


@pytest.fixture
def project_with_repo(api: API, sandbox_repository: str) -> API:
    caller = api.as_caller(api.token, api.org_id, api.project_id)
    caller.repository = sandbox_repository
    return caller


class Launcher:
    """The process and what it has logged so far."""

    def __init__(self, binary: Path, workdir: Path, api_url: str):
        self.prefix = f"hl-agent-{LAUNCHER}-"
        self.log_path = workdir / "launcher.log"
        config = workdir / "agent-launcher.yaml"
        config.write_text(f"""
launcher_name: {LAUNCHER}
api_url: {api_url}
reconcile_interval_seconds: 2
create_pause_milliseconds: 0
provider:
  name: fake
  settings: {{}}
instance:
  command: ["run-agent"]
  cpu: "1"
  memory: 1Gi
  storage: 1Gi
""")
        self.secrets = {"LAUNCHER_TOKEN": admin.add_launcher(LAUNCHER)}
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
    launcher = Launcher(launcher_binary, tmp_path, api.base_url)
    try:
        measurer = api.add_agents(pe_id, hosted=True, role="baseline")[0]["agent_id"]
        researcher = api.add_agents(pe_id, hosted=True)[0]["agent_id"]

        def name_of(agent: str) -> str:
            return next(a["instance"].get("name", "") for a in api.agents(pe_id) if a["agent_id"] == agent)

        def created(agent: str) -> str:
            name = eventually(f"the launcher asking for {agent}", lambda: name_of(agent), accept=bool, deadline=deadline)
            eventually(f"the instance of {agent}", lambda: launcher.said(f"created {launcher.prefix}{name}"),
                       accept=lambda count: count == 1, deadline=deadline)
            return name

        # Open: a machine for the agent that measures the baseline, and none for the researcher.
        # Passes with nothing to do rent nothing more.
        measurer_name = created(measurer)
        time.sleep(6)
        assert launcher.said("created ") == 1, launcher.lines()
        assert launcher.said("destroyed ") == 0, launcher.lines()

        # Removed before the start: its machine goes on the next pass.
        assert api.remove_agent(pe_id, measurer).status_code == 204
        eventually("the removed agent's instance going",
                   lambda: launcher.said(f"destroyed {launcher.prefix}{measurer_name} (no longer desired)"),
                   accept=lambda count: count == 1, deadline=deadline)

        # Started: the researcher's machine is rented.
        api.declare_baseline_and_wait_running(pe_id)
        researcher_name = created(researcher)

        # Closed: it is told to stop once (again only every two minutes while it runs), kept for
        # its wind-down, then taken down.
        api.close_platform_experiment(pe_id)
        closed_at = time.monotonic()
        stop_line = f"told {launcher.prefix}{researcher_name} to stop (ending)"
        eventually("the stop that starts the last turn", lambda: launcher.said(stop_line),
                   accept=lambda count: count == 1, deadline=deadline)
        gone_line = f"destroyed {launcher.prefix}{researcher_name} (no longer desired)"
        assert launcher.said(gone_line) == 0, "an ending instance was destroyed at once"
        eventually("the instance going after its wind-down", lambda: launcher.said(gone_line),
                   accept=lambda count: count == 1, deadline=deadline, interval=5)
        assert time.monotonic() - closed_at >= WIND_DOWN_SECONDS - 10, "the instance did not get its wind-down"
        assert launcher.said(stop_line) <= 2, "an ending instance was told to stop on every pass"
        assert launcher.said("created ") == 2, "an instance was rented for an agent whose run was over"

        # Nothing it logged holds a secret, and no pass failed.
        log = "\n".join(launcher.lines())
        for secret in launcher.secrets.values():
            assert secret not in log, "the launcher logged a secret"
        assert "hls_" not in log and "pass:" not in log, log
    finally:
        launcher.stop()
        api.close_platform_experiment(pe_id)
