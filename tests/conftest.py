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

from support.api import API
from support.cluster import wait_scope_idle
from support.wait import Deadline

REPO_ROOT = Path(__file__).resolve().parents[1]
API_URL = os.environ.get("API_URL", "http://localhost:8081")
HL_BIN = REPO_ROOT / "bin" / "hl"

# Which accelerator the generic scenarios schedule onto.
TEST_ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-L40"
# Must match TEST_ACCELERATOR_TYPE's acch_rate in controlplane/settings/hypothesisloop.yaml.
TEST_ACCH_RATE = 0.25


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
            r = requests.get(f"{API_URL}/platform-experiments", params={"limit": 1}, timeout=5)
            if r.ok:
                return
        except requests.RequestException as exc:
            last_error = exc
        time.sleep(2)
    pytest.exit(
        f"the control plane at {API_URL} did not serve a request within 120s "
        f"(last error: {last_error}). Start it (make controlplane-up) or wait for its migrations "
        "to finish before running the suite.",
        returncode=2,
    )


@pytest.fixture(scope="session")
def api(preflight: None):
    """Closes at session end every platform experiment created through it, which catches the ones
    a test created directly and failed before closing."""
    client = API(API_URL, _build_hl_binary())
    yield client
    for pe_id in client.created_platform_experiments:
        client.close_platform_experiment(pe_id)


@pytest.fixture
def deadline(request: pytest.FixtureRequest) -> Deadline:
    """Tied to pytest-timeout's budget minus a cleanup margin, so a test's own waits give up before
    pytest-timeout kills the process mid-assertion -- an explicit failure naming the awaited
    condition, not a bare SIGALRM."""
    marker = request.node.get_closest_marker("timeout")
    budget = marker.args[0] if marker and marker.args else request.config.getini("timeout")
    margin = 15
    return Deadline.in_seconds(max(1.0, float(budget) - margin))


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
