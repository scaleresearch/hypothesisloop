"""Configuration, run identity, cleanup, markers, preflight (tests/improve.md target layout).

Fresh tenant per test: every fixture that creates a named resource suffixes it with a UUID (not
timestamp/PID -- collision-free either way, but a UUID needs no shared clock assumption), so
xdist-parallel tests never collide on agent ids, job ids or platform-experiment names.
"""
from __future__ import annotations

import os
import subprocess
import time
import uuid
from pathlib import Path

import pytest
import requests

from support import admin
from support.api import API
from support.cluster import wait_scope_idle
from support.wait import Deadline

REPO_ROOT = Path(__file__).resolve().parents[1]
API_URL = os.environ.get("API_URL")
if not API_URL:
    raise pytest.UsageError(
        "API_URL is required: set it to the control plane's base URL, "
        "e.g. API_URL=http://localhost:8081 for the local stack (make e2e passes it)."
    )
HL_TOKEN = os.environ.get("HL_TOKEN")
if not HL_TOKEN:
    raise pytest.UsageError(
        "HL_TOKEN is required: set it to a staff token of the control plane at API_URL "
        "(make e2e mints one for the local stack)."
    )
HL_BIN = REPO_ROOT / "bin" / "hl"
# The org and project every deployment's schema creates, which the suite's own resources live in.
DEFAULT_ORG = "default"
DEFAULT_PROJECT = "default"

# Which accelerator the generic scenarios schedule onto.
TEST_ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-L40"
# Must match TEST_ACCELERATOR_TYPE's acch_rate in controlplane/settings/hypothesisloop.yaml.
TEST_ACCH_RATE = 0.25
# The type every cluster offers its cores as. A job that states cpu and no accelerator holds that
# many units of it. A job that holds no priced unit is refused.
TEST_CPU_TYPE = "hypothesisloop.dev/cpu=core"
# Must match TEST_CPU_TYPE's acch_rate in controlplane/settings/hypothesisloop.yaml: a budget
# spent on cores reads as CPU core-hours.
TEST_CPU_ACCH_RATE = 1.0


def cpu_job(cores: int = 1) -> dict:
    """Job overrides for `cores` cores and no accelerator, asked for the plain way: cpu alone.
    Memory and storage stay those of the default job."""
    return {"accelerator_type": None, "accelerator_count": None, "acceptable_accelerator_types": None, "cpu": str(cores)}


@pytest.fixture(scope="session")
def run_id() -> str:
    return uuid.uuid4().hex[:8]


def _build_hl_binary() -> Path:
    HL_BIN.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["go", "build", "-o", str(HL_BIN), "./cli"], cwd=REPO_ROOT, check=True)
    return HL_BIN


@pytest.fixture(scope="session")
def preflight() -> None:
    """Fail-fast cluster preconditions, ported from tests/lib/preflight.sh. Every scenario's first
    HTTP call fails fast anyway if the control plane isn't up; checking once here turns dozens of
    unrelated-looking per-test failures into one clear message naming the actual cause."""
    _build_hl_binary()

    deadline = Deadline.in_seconds(120)
    last_error: Exception | None = None
    while not deadline.expired():
        try:
            r = requests.get(
                f"{API_URL}/platform-experiments", params={"limit": 1}, timeout=5,
                headers={"Authorization": f"Bearer {HL_TOKEN}"},
            )
            if r.ok:
                return
            if r.status_code in (401, 403):
                pytest.exit(f"the control plane at {API_URL} refused HL_TOKEN ({r.status_code}): {r.text}", returncode=2)
        except requests.RequestException as exc:
            last_error = exc
        time.sleep(2)
    pytest.exit(
        f"the control plane at {API_URL} did not serve a request within 120s "
        f"(last error: {last_error}). Start it (make dev) or wait for its migrations "
        "to finish before running the suite.",
        returncode=2,
    )


@pytest.fixture(scope="session")
def api(preflight: None):
    """The staff caller. At session end it closes every platform experiment created through it
    or through a caller made from it, and removes every cluster the tests registered: what a
    test left behind when it failed, on a database that is never emptied between runs."""
    client = API(API_URL, _build_hl_binary(), HL_TOKEN, DEFAULT_ORG, DEFAULT_PROJECT)
    yield client
    for pe_id in client.created_platform_experiments:
        client.close_platform_experiment(pe_id)
    for name, org_id in admin.added_clusters:
        client.session.delete(client._url(f"/orgs/{org_id}/clusters/{name}"), timeout=30)


@pytest.fixture
def deadline(request: pytest.FixtureRequest) -> Deadline:
    """Tied to pytest-timeout's budget minus a cleanup margin, so a test's own waits give up before
    pytest-timeout kills the process mid-assertion -- an explicit failure naming the awaited
    condition, not a bare SIGALRM."""
    marker = request.node.get_closest_marker("timeout")
    budget = marker.args[0] if marker and marker.args else request.config.getini("timeout")
    margin = 15
    return Deadline.in_seconds(max(1.0, float(budget) - margin))


@pytest.fixture(scope="session")
def sandbox_repository(api: API):
    """owner/name of the GitHub repository tests may work in, reachable by the default org: an
    agent the platform hosts works in its experiment's repository, so hosting needs one. The
    installation that covers it is connected to the default org for the session unless it
    already was, and what was found connected is left connected."""
    installation, repository = os.environ.get("HL_E2E_GITHUB_INSTALLATION"), os.environ.get("HL_E2E_GITHUB_REPO")
    if not installation or not repository:
        pytest.skip("needs a repository: set HL_E2E_GITHUB_INSTALLATION and HL_E2E_GITHUB_REPO")
    if api.get("/github/app").status_code == 503:
        pytest.skip("this deployment has no GitHub App configured")
    installations = f"/orgs/{api.org_id}/github-installations"
    found = any(i["installation_id"] == int(installation) for i in api.get_json(installations, params={"limit": 200}))
    if not found:
        admin.connect_github(api.org_id, installation)
    yield repository
    if not found:
        api.session.delete(api._url(f"{installations}/{installation}"), timeout=30)


def make_agent(api: API, run_id: str, label: str = "agent") -> str:
    agent = f"{label}-{uuid.uuid4().hex[:8]}-{run_id}"
    api.register_agent(agent)
    return agent


@pytest.fixture
def pe(api: API, run_id: str):
    """create+yield+close a platform experiment; a test names its own budget/agents/metrics via
    api.create_platform_experiment and this fixture just guarantees cleanup runs on pass, fail, or
    timeout. Most tests want the richer `experiment` fixture below instead."""
    created: list[str] = []

    def _create(name: str, budget: float, max_agents: int, **kw) -> str:
        pe_id = api.create_platform_experiment(f"{name}-{run_id}", budget, max_agents, **kw)
        created.append(pe_id)
        return pe_id

    yield _create

    for pe_id in created:
        api.close_platform_experiment(pe_id)


@pytest.fixture
def experiment(api: API, run_id: str):
    """The common case: one platform experiment, a roster of freshly-registered agents signed up
    and started. Returns (pe_id, agents) after signup+start. Closes the PE on teardown regardless
    of outcome."""
    created: list[str] = []

    def _make(
        name: str,
        agents: list[str],
        budget: float,
        max_agents: int | None = None,
        **kw,
    ) -> str:
        pe_id = api.create_platform_experiment(f"{name}-{run_id}", budget, max_agents or len(agents), **kw)
        created.append(pe_id)
        api.signup_and_start(pe_id, agents)
        return pe_id

    yield _make

    for pe_id in created:
        api.close_platform_experiment(pe_id)


@pytest.fixture(autouse=True)
def idle_cluster(request: pytest.FixtureRequest, api: API):
    """Autouse on `exclusive` tests only: a scenario only owns its scope if that scope is actually
    idle when it starts (tests/improve.md #2.2). A failure to reach idle is a test/setup failure,
    not a warning followed by execution -- never "continue anyway" (that turns one contamination
    problem into unrelated failures downstream, see lib/scope.sh's header comment).
    """
    if "exclusive" not in {m.name for m in request.node.iter_markers()}:
        yield
        return
    wait_scope_idle(api, list(request.node.get_closest_marker("exclusive").args))
    yield
