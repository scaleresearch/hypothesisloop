"""HARDWARE-ONLY: requires a real NVIDIA GPU, nvidia-container-toolkit, and podman/docker on this
host. Excluded from a default pytest run -- included only with `-m hardware`. Ported 1:1 from
tests/scenarios/nvidia-hardware.sh.

Bare-node leg (always attempted when hardware is present): admission, real GPU compute, metrics, a
negative-admission case, and mid-run metrics + eviction + post-eviction reuse -- all via
runtime/bare-metal/cmd/bare-agent. k3s leg (optional): same hardware via cluster-agent's
device-plugin path -- see localdev/k3s-nvidia/install.sh, registers cluster_name "k3s-nvidia".
Skipped (not failed) if that context isn't set up on this host, same as the bash scenario.

This host has no NVIDIA GPU (`nvidia-smi` not found, no `/dev/nvidia*`), so this module cleanly
`pytest.skip`s at collection time -- identical in spirit to the bash scenario's own
"[SKIP] no nvidia-smi on this host" early-exit. A future host with real NVIDIA hardware runs this
test for real the moment nvidia-smi (and a working GPU passthrough) exist -- no code change needed.
"""
from __future__ import annotations

import os
import shutil
import subprocess

import pytest

from conftest import API_URL, REPO_ROOT
from support.wait import eventually

pytestmark = [pytest.mark.hardware, pytest.mark.exclusive("nvidia")]

JOB_FILE = REPO_ROOT / "tests" / "workloads" / "nvidia" / "job.yaml"


def _skip_reason() -> str | None:
    """Mirrors nvidia-hardware.sh's own early-exit chain, in order: no nvidia-smi -> no container
    engine -> engine can't pass a GPU through -> detected GPU has no acch_rate configured."""
    if shutil.which("nvidia-smi") is None:
        return "no nvidia-smi on this host"
    engine = shutil.which("docker") or shutil.which("podman")
    if engine is None:
        return "neither podman nor docker found on this host"
    probe = subprocess.run(
        [engine, "run", "--rm", "--gpus", "all", "nvidia/cuda:12.4.1-base-ubuntu22.04", "nvidia-smi", "-L"],
        capture_output=True, text=True, timeout=60,
    )
    if probe.returncode != 0:
        return f"{engine} cannot pass a GPU through (nvidia-container-toolkit not set up?)"

    gpu_name_raw = subprocess.run(
        ["nvidia-smi", "--query-gpu=name", "--format=csv,noheader"],
        capture_output=True, text=True, timeout=15,
    ).stdout.splitlines()
    if not gpu_name_raw:
        return "nvidia-smi reported no GPU name"
    acc_type = "nvidia.com/gpu.product=" + gpu_name_raw[0].strip().upper().replace(" ", "-")
    settings = (REPO_ROOT / "controlplane" / "settings" / "hypothesisloop.yaml").read_text()
    if acc_type not in settings:
        return f"{acc_type} has no acch_rate in controlplane/settings/hypothesisloop.yaml -- add one to admit jobs of this type"
    return None


_REASON = _skip_reason()
if _REASON is not None:
    pytest.skip(f"nvidia-hardware preconditions not met: {_REASON}", allow_module_level=True)


def _detected_accelerator_type() -> str:
    raw = subprocess.run(
        ["nvidia-smi", "--query-gpu=name", "--format=csv,noheader"],
        capture_output=True, text=True, timeout=15,
    ).stdout.splitlines()[0].strip()
    return "nvidia.com/gpu.product=" + raw.upper().replace(" ", "-")


def _engine() -> str:
    return shutil.which("docker") and "docker" or "podman"


@pytest.mark.timeout(600)
def test_nvidia_bare_node_and_optional_k3s_leg(api, run_id: str, deadline, tmp_path):
    engine = _engine()
    acc_type = _detected_accelerator_type()
    cluster_name = f"bare-nvidia-e2e-{run_id}"

    agent_bin = tmp_path / "bare-agent"
    subprocess.run(
        ["go", "build", "-o", str(agent_bin), "./runtime/bare-metal/cmd/bare-agent"],
        cwd=REPO_ROOT, check=True, capture_output=True, text=True,
    )
    scratch_dir = tmp_path / "scratch"
    scratch_dir.mkdir()
    log_path = tmp_path / "bare-agent.log"
    env = {
        "CLUSTER_NAME": cluster_name,
        "API_URL": API_URL,
        "HYPOTHESISLOOP_CONFIG": str(REPO_ROOT / "controlplane" / "settings" / "hypothesisloop.yaml"),
        "SCRATCH_DIR": str(scratch_dir),
        "NODE_NAME": f"nvidia-e2e-node-{run_id}",
        "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/local/bin"),
    }
    log_file = open(log_path, "w")
    proc = subprocess.Popen([str(agent_bin)], stdout=log_file, stderr=subprocess.STDOUT, env=env)

    submitted: list[str] = []
    agent = f"agent-nvidia-e2e-{run_id}"
    pe_id = ""
    try:
        eventually(
            f"bare-agent registered cluster {cluster_name}",
            lambda: cluster_name in [c.get("cluster_name") for c in api.internal_clusters()],
            accept=lambda ok: ok,
            deadline=deadline,
        )

        subprocess.run(
            [engine, "build", "-f", str(REPO_ROOT / "tests" / "workloads" / "nvidia" / "Dockerfile.train"),
             "-t", "localhost/hypothesisloop-nvidia-workload", str(REPO_ROOT / "tests" / "workloads" / "nvidia")],
            check=True, capture_output=True, text=True,
        )

        api.register_agent(agent)
        pe_id = api.create_platform_experiment(
            f"nvidia-e2e-{run_id}", 1.0, 1,
            metrics=[
                {"key": "tflops_measured", "direction": "maximize"},
                {"key": "latency_ms", "direction": "minimize"},
            ],
        )
        api.signup_and_start(pe_id, [agent])

        _run(api, pe_id, agent, cluster_name, engine, acc_type, run_id, deadline, submitted)
    finally:
        for j in submitted:
            try:
                api.cancel_job(j)
            except Exception:
                pass
        if pe_id:
            api.close_platform_experiment(pe_id)
        proc.kill()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            pass
        log_file.close()


def _run(api, pe_id, agent, cluster_name, engine, acc_type, run_id, deadline, submitted: list[str]) -> None:
    def force_admit(job_id: str, cluster: str) -> bool:
        for _ in range(5):
            r = api.admit(job_id, cluster)
            if r.status_code == 200:
                return True
        return False

    # -- real GPU job to completion --
    job = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": acc_type, "accelerator_count": 1})
    submitted.append(job)
    force_admit(job, cluster_name)

    final = eventually(
        f"{job} to reach a terminal status",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", (
        f"job did not reach COMPLETED (status={final['status']}, eviction_reason={final.get('eviction_reason', 'n/a')})"
    )
    assert final.get("cluster_name") == cluster_name, (
        f"job landed on cluster_name={final.get('cluster_name')!r}, expected {cluster_name}"
    )
    metrics = api.metrics(job)
    assert len(metrics) >= 1, "0 metric points recorded -- container may not have actually run/reported"
    api.file_finding(job, "NVIDIA e2e: real GPU matmuls via bare-node agent confirmed.")

    # -- negative case: hardware this cluster does not have must never be admitted onto it --
    other_type = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
    if acc_type == other_type:
        other_type = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
    job2 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": other_type, "accelerator_count": 1})
    submitted.append(job2)
    import time
    time.sleep(5)
    exp2 = api.experiment(job2)
    assert exp2["status"] in ("SUBMITTED", "QUEUED") and not exp2.get("cluster_name"), (
        f"job requesting hardware this cluster lacks should stay SUBMITTED/QUEUED with no cluster, "
        f"got status={exp2['status']} cluster_name={exp2.get('cluster_name')!r}"
    )
    api.cancel_job(job2)

    # -- mid-run metrics + eviction + post-eviction reuse --
    job3 = api.submit_job(
        pe_id, agent, hours=0.05,
        job_overrides={"accelerator_type": acc_type, "accelerator_count": 1},
        metadata_overrides={},
    )
    submitted.append(job3)
    force_admit(job3, cluster_name)

    eventually(
        f"{job3} reaches RUNNING",
        lambda: api.experiment(job3),
        accept=lambda e: e["status"] == "RUNNING",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    eventually(
        f"{job3} reports >=2 metric samples while RUNNING",
        lambda: api.metrics(job3),
        accept=lambda m: len(m) >= 2,
        deadline=deadline,
    )
    container_name = subprocess.run(
        [engine, "ps", "--filter", f"label=hypothesisloop.io/experiment-id={job3}", "--format", "{{.Names}}"],
        capture_output=True, text=True, timeout=15,
    ).stdout.strip().splitlines()
    assert container_name, f"no local {engine} container found for {job3}"

    api.cancel_job(job3)
    eventually(
        f"{job3} reaches EVICTED",
        lambda: api.experiment(job3),
        accept=lambda e: e["status"] == "EVICTED",
        deadline=deadline,
    )

    def container_stopped() -> bool:
        out = subprocess.run(
            [engine, "ps", "--filter", f"label=hypothesisloop.io/experiment-id={job3}", "--filter", "status=running", "--format", "{{.Names}}"],
            capture_output=True, text=True, timeout=15,
        ).stdout.strip()
        return out == ""

    eventually(f"{job3}'s container actually stops", container_stopped, accept=lambda ok: ok, deadline=deadline)

    job4 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": acc_type, "accelerator_count": 1})
    submitted.append(job4)
    force_admit(job4, cluster_name)
    final4 = eventually(
        f"{job4} to reach a terminal status after eviction",
        lambda: api.experiment(job4),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final4["status"] == "COMPLETED", f"post-eviction job did not COMPLETE (status={final4['status']})"
    api.file_finding(job4, "NVIDIA e2e: post-eviction reuse confirmed.")

    # -- optional k3s/device-plugin leg --
    k3s_context = os.environ.get("NVIDIA_K3S_CONTEXT", "k3s-nvidia")
    if shutil.which("kubectl") is None:
        return
    reachable = subprocess.run(
        ["kubectl", "--context", k3s_context, "get", "nodes"], capture_output=True, timeout=15,
    ).returncode == 0
    if not reachable:
        return

    proc_kill_deadline = deadline
    # Bare-node leg is done -- stop it and wait for its heartbeat to go stale so its cluster
    # (which sorts before "k3s-nvidia") can't also win this leg's admission tiebreak.
    eventually(
        f"bare-node cluster {cluster_name} drops out of /internal/clusters",
        lambda: cluster_name not in [c.get("cluster_name") for c in api.internal_clusters()],
        accept=lambda ok: ok,
        deadline=proc_kill_deadline,
    )

    node_json = subprocess.run(
        ["kubectl", "--context", k3s_context, "get", "nodes", "-o", "json"],
        capture_output=True, text=True, timeout=15,
    ).stdout
    import json as _json
    k3s_product = None
    for node in _json.loads(node_json).get("items", []):
        v = (node.get("metadata", {}).get("labels") or {}).get("nvidia.com/gpu.product")
        if v:
            k3s_product = v
            break
    if not k3s_product:
        return

    k3s_acc_type = f"nvidia.com/gpu.product={k3s_product}"
    job5 = api.submit_job(pe_id, agent, hours=0.02, job_overrides={"accelerator_type": k3s_acc_type, "accelerator_count": 1})
    submitted.append(job5)
    force_admit(job5, "k3s-nvidia")
    final5 = eventually(
        f"{job5} to reach a terminal status via k3s/device-plugin",
        lambda: api.experiment(job5),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final5["status"] == "COMPLETED", f"k3s leg: job did not reach COMPLETED (status={final5['status']})"
    assert final5.get("cluster_name") == "k3s-nvidia"
    m5 = api.metrics(job5)
    assert len(m5) >= 1, "k3s leg: 0 metric points recorded"
    api.file_finding(job5, f"NVIDIA e2e: real GPU matmuls via k3s/device-plugin confirmed ({k3s_product}).")
