"""End-to-end proof that the bare-node agent (runtime/bare-metal/cmd/bare-agent -- see
different-backend.md) genuinely collaborates with the control plane: it registers as a real
cluster, runs a submitted job as a real container on this host (not just that the control plane's
own bookkeeping says RUNNING), and a cancel actually stops a running one. Ported 1:1 from
tests/scenarios/bare-node-agent.sh.

Unlike every other e2e test (which submits against an already-running k3s/tenstorrent
cluster-agent), this test starts its own bare-agent process pointed at the local control plane
(assumed already up via `make controlplane-up`) under a UUID-namespaced CLUSTER_NAME, so it cannot
collide with any other cluster registered against this control plane -- safe to run concurrently
with the rest of the parallel lane (`pytest.mark.parallel`, not `exclusive`): its cluster name is
unique per test and it touches no shared k8s/daemonset state, same reasoning as the bash header
comment this test was ported from.

Requires a real podman or docker binary on this host (same requirement as
runtime/bare-metal/internal/podexec's own integration test) -- skipped, not failed, if neither is
present.

PROVEN against the live stack: 3x green standalone (`uv run pytest e2e/test_bare_node_agent.py -v`
x3), host verified clean (no stray bare-agent process, no stray container) after every run.
"""
from __future__ import annotations

import os
import shutil
import subprocess
import uuid

import pytest

from conftest import API_URL, REPO_ROOT
from support.wait import eventually

pytestmark = pytest.mark.parallel

CONTAINER_ENGINE_BIN = shutil.which("podman") or shutil.which("docker")
if CONTAINER_ENGINE_BIN is None:
    pytest.skip(
        "neither podman nor docker found on this host -- bare-node agent needs a container engine",
        allow_module_level=True,
    )

JOB_OVERRIDE_BASE: dict = {
    "image": "busybox",
    "command": ["sh", "-c"],
    "cpu": "0.2",
    "memory": "64Mi",
    "storage": "128Mi",
    "max_retries": 0,
    "accelerator_count": 0,
    "accelerator_type": None,
    "acceptable_accelerator_types": None,
    "accelerator_tolerations": None,
    "extra_resources": None,
    "accelerator_pod_resources": None,
}


def job_override(sleep_seconds: int) -> dict:
    """A CPU-only busybox job body, with every k8s-only field the bare executor rejects
    (podexec.BuildContainerSpec -- accelerator_tolerations/extra_resources/
    accelerator_pod_resources) explicitly nulled out per different-backend.md's coupling-2 table,
    rather than relying on job.yaml's defaults."""
    body = dict(JOB_OVERRIDE_BASE)
    body["args"] = [f"echo hello-from-bare-node-agent && sleep {sleep_seconds}"]
    return body


@pytest.fixture
def bare_agent(tmp_path, run_id: str):
    """Builds and launches a real bare-agent process against the local control plane under its own
    UUID-namespaced cluster name, waits for it to register, and tears it down unconditionally:
    kills the process and removes any container it left behind, labeled by this test's own agent
    id, so a failed assertion can never leave a stray process or container on the host (the same
    duty the bash scenario's `cleanup`/`trap EXIT` served)."""
    agent_bin = tmp_path / "bare-agent"
    subprocess.run(
        ["go", "build", "-o", str(agent_bin), "./runtime/bare-metal/cmd/bare-agent"],
        cwd=REPO_ROOT, check=True, capture_output=True, text=True,
    )

    cluster_name = f"bare-e2e-{uuid.uuid4().hex[:8]}-{run_id}"
    scratch_dir = tmp_path / "scratch"
    scratch_dir.mkdir()
    log_path = tmp_path / "bare-agent.log"

    env = {
        "CLUSTER_NAME": cluster_name,
        "API_URL": API_URL,
        "HYPOTHESISLOOP_CONFIG": str(REPO_ROOT / "controlplane" / "settings" / "hypothesisloop.yaml"),
        "SCRATCH_DIR": str(scratch_dir),
        "NODE_NAME": f"bare-e2e-node-{run_id}",
        "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/local/bin"),
    }
    log_file = open(log_path, "w")
    proc = subprocess.Popen([str(agent_bin)], stdout=log_file, stderr=subprocess.STDOUT, env=env)

    platform_agent = f"agent-bare-e2e-{uuid.uuid4().hex[:8]}-{run_id}"

    try:
        yield agent_bin, cluster_name, log_path, platform_agent
    finally:
        proc.kill()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            pass
        log_file.close()
        leftover = subprocess.run(
            [
                CONTAINER_ENGINE_BIN, "ps", "-aq",
                "--filter", "label=hypothesisloop.io/managed-by=hypothesisloop",
                "--filter", f"label=hypothesisloop.io/agent-id={platform_agent}",
            ],
            capture_output=True, text=True, timeout=15,
        ).stdout.split()
        if leftover:
            subprocess.run([CONTAINER_ENGINE_BIN, "rm", "-f", *leftover], capture_output=True, text=True, timeout=15)


def _job_container_name(engine: str, job_id: str) -> str:
    out = subprocess.run(
        [engine, "ps", "-a", "--filter", f"label=hypothesisloop.io/experiment-id={job_id}", "--format", "{{.Names}}"],
        capture_output=True, text=True, timeout=15,
    ).stdout.strip()
    return out.splitlines()[0] if out else ""


def test_bare_node_agent_runs_and_cancels_real_containers(api, run_id, deadline, bare_agent):
    """One sequential flow, ported as a single test since every step consumes state (cluster
    registration, resolved engine, platform experiment, agent) produced by the one before it --
    the same reason the bash scenario runs it as a single script."""
    agent_bin, cluster_name, log_path, agent = bare_agent

    # -- bare-agent process started, waiting for its first reconcile --
    eventually(
        f"bare-agent registered cluster {cluster_name}",
        lambda: cluster_name in [c.get("cluster_name") for c in api.internal_clusters()],
        accept=lambda ok: ok,
        deadline=deadline,
    )

    # Ask the agent itself which container engine it resolved to (runtime/bare-metal/internal/
    # podexec/util.go's resolveEngineClient logs this at startup) rather than re-guessing with a
    # `command -v` check -- the two can legitimately disagree (e.g. podman installed but its API
    # socket not enabled), and re-guessing would just reproduce that mismatch here.
    log_text = log_path.read_text()
    engine = None
    for token in ("engine=podman", "engine=docker"):
        if token in log_text:
            engine = token.split("=", 1)[1]
            break
    assert engine is not None, f"could not determine which container engine the bare-agent resolved to (see {log_path})"

    api.register_agent(agent)
    pe_id = api.create_platform_experiment(f"bare-e2e-{run_id}", 0.001, 1, report_interval_seconds=10)
    api.signup_and_start(pe_id, [agent])

    # A job left RUNNING forever (e.g. this test asserting out early) would outlive its bare-agent
    # process once the fixture kills it -- its cluster never reports again, so every future poll of
    # it errors, and JobWatcher.scanAndWatch (controlplane/services/scheduler/job_watcher_scan.go)
    # aborts its *entire* pass on the first error, starving every other experiment's status updates
    # too. Cancel unconditionally on exit, same duty the bash scenario's `cleanup`/`trap EXIT` served.
    submitted_jobs: list[str] = []
    try:
        _run_bare_node_flow(api, pe_id, agent, cluster_name, engine, deadline, submitted_jobs)
    finally:
        for j in submitted_jobs:
            try:
                api.cancel_job(j)
            except Exception:
                pass
        api.close_platform_experiment(pe_id)


def _run_bare_node_flow(api, pe_id, agent, cluster_name, engine, deadline, submitted_jobs: list[str]) -> None:
    def container_exists(job_id: str) -> bool:
        return _job_container_name(engine, job_id) != ""

    def container_absent(job_id: str) -> bool:
        return _job_container_name(engine, job_id) == ""

    # ==> run a job to completion, verify a real container backed it --
    job = api.submit_job(pe_id, agent, hours=0.01, job_overrides=job_override(2))
    submitted_jobs.append(job)
    api.admit(job, cluster_name).raise_for_status()

    exp = eventually(
        f"{job} to reach RUNNING/COMPLETED on the bare-node cluster",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert exp["status"] in ("RUNNING", "COMPLETED")
    assert exp.get("cluster_name") == cluster_name, (
        f"job landed on cluster_name={exp.get('cluster_name')!r}, expected {cluster_name}"
    )

    eventually(
        f"a real {engine} container for {job} exists on this host",
        lambda: container_exists(job),
        accept=lambda ok: ok,
        deadline=deadline,
    )

    # The bare-agent's own FetchLogTail (runtime/bare-metal/internal/podexec) reads this same
    # container's stdout live and reports it alongside job phase on its next status push (see
    # runtime/shared/agentloop.reportChangedStatuses + controlplane/services/clusteragentapi) --
    # the control plane never reaches into the container engine itself. Confirms the whole chain:
    # real container -> bare-agent -> clusteragentapi -> GreptimeDB -> registry GET, no job-side
    # involvement at all (the job here is a bare `sh -c echo ...`, nothing pushes its own logs).
    eventually(
        "registry's log-tail endpoint reports this container's own stdout",
        lambda: api.logs(job),
        accept=lambda text: "hello-from-bare-node-agent" in text,
        deadline=deadline,
    )

    # Read now, straight after the log-tail wait confirms the output exists -- not after the
    # COMPLETED wait below. The bare-agent can reap a finished container as soon as the job
    # terminates, and every extra wait between "the output exists" and "read the engine's own copy
    # of it" is time the reaper gets to run first: capturing only a name here and reading it later
    # still raced that reap, because the container itself, not merely its identifier, has to
    # survive to be read.
    container_name = _job_container_name(engine, job)
    assert container_name, f"container for {job} was already reaped right after producing output the platform confirmed seeing"
    container_logs = subprocess.run(
        [engine, "logs", container_name], capture_output=True, text=True, timeout=15
    ).stdout
    assert "hello-from-bare-node-agent" in container_logs, f"container logs missing expected output: {container_logs!r}"

    exp = eventually(
        f"{job} to COMPLETE end-to-end via the bare-node agent",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert exp["status"] == "COMPLETED", f"expected job to COMPLETE, got {exp['status']}"
    api.file_finding(job)

    # ==> cancel a running job, verify it actually stops --
    job2 = api.submit_job(pe_id, agent, hours=0.01, job_overrides=job_override(60))
    submitted_jobs.append(job2)
    api.admit(job2, cluster_name).raise_for_status()

    exp2 = eventually(
        f"{job2} to reach RUNNING on the bare-node cluster",
        lambda: api.experiment(job2),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED", "FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert exp2["status"] == "RUNNING", f"second job must be RUNNING before cancel, got {exp2['status']}"

    eventually(
        f"a real {engine} container for {job2} exists on this host",
        lambda: container_exists(job2),
        accept=lambda ok: ok,
        deadline=deadline,
    )

    api.cancel_job(job2)

    exp2 = eventually(
        f"{job2} to reach EVICTED after cancel",
        lambda: api.experiment(job2),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert exp2["status"] == "EVICTED", f"expected EVICTED after cancelling a RUNNING job, got {exp2['status']}"

    eventually(
        f"the {engine} container for {job2} is removed after cancel",
        lambda: container_absent(job2),
        accept=lambda ok: ok,
        deadline=deadline,
    )
