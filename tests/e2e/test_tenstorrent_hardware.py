"""HARDWARE-ONLY: requires real Tenstorrent Blackhole hardware (box-a's `make tt-up`).
Excluded from a default pytest run -- included only with `-m hardware`. Proves the platform's own
submission path reaches real hardware: submit a job, verify a genuine DRA ResourceClaim gets
allocated a real /dev/tenstorrent device, not just that the platform reports success. Then proves
the same physical device is independently reachable via the bare-node agent (no k3s/DRA involved).

Ported 1:1 from tests/scenarios/tenstorrent-hardware.sh -- attaches/detaches box-a from its
k3s context via localdev/lib/node.sh's lib_attach_node/lib_detach_node for the duration of the
test only, same care as the k8s-mutating scenarios (test_node_and_daemonset_faults.py): detach
happens in a `finally` so it runs on pass, fail, or timeout.

The k3s leg's job runs the image `make tt-hardware-image` pushed, as the cluster's nodes pull it:
TT_WORKLOAD_IMAGE names it when the registry is not the local one (a control plane on another
machine). The bare-node leg builds the same image on this host and runs that.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from conftest import API_URL, DEFAULT_ORG, REPO_ROOT, make_agent
from support import admin
from support.wait import eventually

pytestmark = [pytest.mark.hardware, pytest.mark.exclusive("tenstorrent")]

TT_CONTEXT = os.environ.get("TT_CONTEXT", "k3s-tt")
JOB_NS = "hypothesisloop-jobs"
ACCEL_TYPE = "tenstorrent.com/chipArch=blackhole"

# Overrides tests/workloads/tenstorrent/job.yaml's own fields on top of the generic base spec
# api.submit_job() loads by default (workloads/generic/job.yaml): a real tt-metal workload needs
# its own image/cpu/memory/hugepages, and must drop the generic spec's nvidia
# accelerator_tolerations/acceptable_accelerator_types -- see that job.yaml's own comments for why
# (256Mi OOMKilled every run; 4Gi clears it).
BARE_IMAGE = "localhost/hypothesisloop-tenstorrent-workload:latest"


def _k3s_image() -> str:
    named = os.environ.get("TT_WORKLOAD_IMAGE")
    if named:
        return named
    host = (REPO_ROOT / "localdev" / ".host-address").read_text().strip()
    tag = (REPO_ROOT / "localdev" / ".image-tag").read_text().strip()
    return f"{host}:5000/hypothesisloop-tenstorrent-workload:{tag}"


TT_JOB_OVERRIDES = {
    "image": BARE_IMAGE,
    "cpu": "2",
    "memory": "4Gi",
    "storage": "1Gi",
    "max_retries": 3,
    "accelerator_count": 1,
    "accelerator_type": ACCEL_TYPE,
    "accelerator_pod_resources": {"hugepages-1Gi": "4Gi"},
    "acceptable_accelerator_types": None,
    "accelerator_tolerations": None,
}


def _kubectl(*args: str, timeout: float = 15) -> str:
    return subprocess.run(
        ["kubectl", "--context", TT_CONTEXT, *args],
        capture_output=True, text=True, timeout=timeout,
    ).stdout.strip()


def _preconditions_met() -> str | None:
    """Returns a skip reason if any real-hardware precondition is missing, else None."""
    if sys.platform != "linux":
        return "this scenario only runs on the Linux box-a host"
    if subprocess.run(
        ["kubectl", "--context", TT_CONTEXT, "get", "node", "box-a"],
        capture_output=True, timeout=15,
    ).returncode != 0:
        return f"context {TT_CONTEXT} is not the box-a cluster"
    if subprocess.run(
        ["kubectl", "--context", TT_CONTEXT, "get", "deviceclass", "tenstorrent.com"],
        capture_output=True, timeout=15,
    ).returncode != 0:
        return f"context {TT_CONTEXT} has no Tenstorrent DeviceClass"
    out = _kubectl("get", "resourceslices", "-o", "json", timeout=30)
    try:
        doc = json.loads(out)
    except Exception:
        return f"could not read resourceslices on {TT_CONTEXT}"
    device_count = sum(
        len((item.get("spec") or {}).get("devices") or [])
        for item in doc.get("items") or []
        if (item.get("spec") or {}).get("driver") == "tenstorrent.com"
    )
    if device_count <= 0:
        return "tenstorrent.com publishes no actual devices in ResourceSlices"
    return None


_SKIP_REASON = _preconditions_met()
if _SKIP_REASON is not None:
    pytest.skip(f"tenstorrent-hardware preconditions not met: {_SKIP_REASON}", allow_module_level=True)


def _lib_attach_node() -> None:
    subprocess.run(
        ["kubectl", "--context", TT_CONTEXT, "taint", "node", "box-a",
         "hypothesisloop.io/no-workload:NoSchedule-"],
        capture_output=True, text=True, timeout=15,
    )


def _lib_detach_node() -> None:
    subprocess.run(
        ["kubectl", "--context", TT_CONTEXT, "taint", "node", "box-a",
         "hypothesisloop.io/no-workload:NoSchedule", "--overwrite"],
        capture_output=True, text=True, timeout=15,
    )


def _pod_for_experiment(experiment_id: str) -> str:
    out = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}", "-o", "json",
    )
    try:
        items = json.loads(out).get("items") or []
    except Exception:
        return ""
    return items[0]["metadata"]["name"] if items else ""


def _resourceclaim_device_results(experiment_id: str) -> list:
    out = _kubectl(
        "-n", JOB_NS, "get", "resourceclaim", "-l", f"hypothesisloop.io/experiment-id={experiment_id}", "-o", "json",
    )
    try:
        items = json.loads(out).get("items") or []
    except Exception:
        return []
    if not items:
        return []
    return ((items[0].get("status") or {}).get("allocation") or {}).get("devices", {}).get("results", [])


@pytest.fixture(autouse=True)
def _attach_tt_quietbox():
    _lib_attach_node()
    try:
        yield
    finally:
        _lib_detach_node()


@pytest.mark.timeout(480)
def test_tenstorrent_real_dra_allocation_and_bare_node(api, run_id: str, deadline, tmp_path: Path):
    agent = make_agent(api, run_id, "tt-e2e-agent")
    # tflops_measured/latency_ms are what tests/workloads/tenstorrent/train.py actually pushes --
    # raw measured TFLOPS (a running max, unbounded), not the generic val_accuracy stub.
    pe_id = api.create_platform_experiment(
        f"tt-e2e-{run_id}", 1.0, 1,
        metrics=[
            {"key": "tflops_measured", "direction": "maximize"},
            {"key": "latency_ms", "direction": "minimize"},
        ],
    )
    # An autonomous agent defaults to the burst_only tier; this scenario submits tier="guaranteed"
    # jobs (matching the bash scenario's submit_job calls) and needs the reserved quota that tier
    # draws from (see test_node_and_daemonset_faults.py for the same override).
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    # The baseline is this experiment's own workload, on the hardware the run is about.
    api.declare_baseline_and_wait_running(pe_id, baseline_job={**TT_JOB_OVERRIDES, "image": _k3s_image(), "command": None})

    try:
        _run(api, pe_id, agent, run_id, deadline, tmp_path)
    finally:
        api.close_platform_experiment(pe_id)


def _run(api, pe_id: str, agent: str, run_id: str, deadline, tmp_path: Path) -> None:
    job = api.submit_job(
        pe_id, agent, hours=0.02,
        job_overrides={**TT_JOB_OVERRIDES, "image": _k3s_image()},
    )

    exp = eventually(
        f"{job} admitted onto a real cluster",
        lambda: api.experiment(job),
        accept=lambda e: bool(e.get("cluster_name")),
        deadline=deadline,
    )
    assert exp.get("cluster_name") == "box-a", (
        f"job admitted onto cluster_name={exp.get('cluster_name')!r}, expected box-a "
        "(real hardware, not a fake/local node)"
    )

    # The ResourceClaim is owned by the pod (ownerReference + delete-protection finalizer) and is
    # garbage-collected the moment the pod terminates -- must be checked while the pod is still
    # up, not after waiting out a terminal job status.
    pod_name = ""
    claim_results: list = []

    def _probe_dra():
        nonlocal pod_name, claim_results
        pod_name = _pod_for_experiment(job)
        if pod_name:
            claim_results = _resourceclaim_device_results(job)
        return bool(pod_name) and bool(claim_results)

    eventually(
        "real pod + ResourceClaim device allocation on the DRA path",
        _probe_dra,
        accept=lambda ok: ok,
        deadline=deadline,
    )
    assert pod_name, f"no pod found on {TT_CONTEXT} for experiment {job} -- DRA/scheduling path likely did not run"
    assert any("tenstorrent.com" in str(r) for r in claim_results), (
        f"ResourceClaim did not show a tenstorrent.com device allocation: {claim_results!r}"
    )

    final = eventually(
        f"{job} to reach a terminal status",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", (
        f"job did not reach COMPLETED (status={final['status']}, "
        f"eviction_reason={final.get('eviction_reason', 'n/a')}) -- see: "
        f"kubectl --context {TT_CONTEXT} -n {JOB_NS} get pods,resourceclaims"
    )

    metrics = api.metrics(job)
    assert len(metrics) >= 1, "0 metric points recorded -- pod may not have actually run/reported"
    api.file_finding(job, "Tenstorrent e2e: real DRA allocation on box-a confirmed.")

    # -- same physical hardware, reached via this host's bare-node agent (no k3s/DRA at all) --
    # Detach box-a from k3s now: it stays a schedulable, guaranteed-capacity node until the
    # fixture's own teardown, and the scheduler would place the next job there. Detached, the
    # bare-node cluster is the only one that reports the device.
    _lib_detach_node()
    bare_cluster = f"tt-bare-e2e-{run_id}"
    agent_bin = tmp_path / "bare-agent"
    subprocess.run(
        ["go", "build", "-o", str(agent_bin), "./runtime/bare-metal/cmd/bare-agent"],
        cwd=REPO_ROOT, check=True, capture_output=True, text=True,
    )
    scratch_dir = tmp_path / "scratch"
    scratch_dir.mkdir()
    log_path = tmp_path / "bare-agent.log"
    env = {
        "CLUSTER_NAME": bare_cluster,
        "API_URL": API_URL,
        "RUNTIME_TOKEN": admin.add_cluster(bare_cluster, DEFAULT_ORG),
        "SCRATCH_DIR": str(scratch_dir),
        "NODE_NAME": f"tt-bare-e2e-node-{run_id}",
        "PATH": os.environ.get("PATH", "/usr/bin:/bin:/usr/local/bin"),
    }
    log_file = open(log_path, "w")
    proc = subprocess.Popen([str(agent_bin)], stdout=log_file, stderr=subprocess.STDOUT, env=env)

    try:
        eventually(
            f"bare-agent registered cluster {bare_cluster}",
            lambda: bare_cluster in [c.get("cluster_name") for c in api.internal_clusters()],
            accept=lambda ok: ok,
            deadline=deadline,
        )

        log_text = log_path.read_text()
        engine = None
        for token in ("engine=podman", "engine=docker"):
            if token in log_text:
                engine = token.split("=", 1)[1]
                break
        assert engine is not None, f"could not determine which container engine the bare-agent resolved to (see {log_path})"

        subprocess.run(
            [engine, "build", "-f", str(REPO_ROOT / "tests" / "workloads" / "tenstorrent" / "Dockerfile.train"),
             "-t", "localhost/hypothesisloop-tenstorrent-workload", str(REPO_ROOT / "tests" / "workloads" / "tenstorrent")],
            check=True, capture_output=True, text=True,
        )

        bare_job = api.submit_job(
            pe_id, agent, hours=0.02,
            job_overrides=dict(TT_JOB_OVERRIDES),
            job_id=f"tt-bare-{run_id[:8]}",
        )

        bare_final = eventually(
            f"{bare_job} to reach a terminal status on the bare-node agent",
            lambda: api.experiment(bare_job),
            accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
            deadline=deadline,
        )
        assert bare_final.get("cluster_name") == bare_cluster, (
            f"bare-node job landed on cluster_name={bare_final.get('cluster_name')!r}, expected {bare_cluster}"
        )
        assert bare_final["status"] == "COMPLETED", (
            f"bare-node job did not reach COMPLETED (status={bare_final['status']}, "
            f"eviction_reason={bare_final.get('eviction_reason', 'n/a')}) -- see {log_path}"
        )
        bare_metrics = api.metrics(bare_job)
        assert len(bare_metrics) >= 1, "0 metric points recorded from the bare-node job -- container may not have actually run/reported"
        api.file_finding(bare_job, "Tenstorrent e2e: real device access via bare-node agent confirmed.")
    finally:
        proc.kill()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            pass
        log_file.close()
