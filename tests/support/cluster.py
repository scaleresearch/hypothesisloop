"""The scope-drain barrier for `exclusive` tests, ported from tests/lib/scope.sh::wait_scope_idle.

Only a handful of kubectl operations belong here per tests/improve.md's target layout; none are
needed by the `parallel`-lane scenarios ported so far (Step 2 of this round), so this module is
presently just the drain barrier `idle_cluster` needs. Fault-injection kubectl helpers
(tests/lib/cluster.sh) move here in a later round together with the exclusive scenarios themselves.
"""
from __future__ import annotations

import json
import os
import subprocess
import time
from dataclasses import dataclass

from .api import API

JOB_NS = os.environ.get("JOB_NS", "hypothesisloop-jobs")
CLUSTER_NS = os.environ.get("CLUSTER_NS", "hypothesisloop")


def node_allocatable_gpu_total(label_selector: str) -> int:
    """Sum of `nvidia.com/gpu` allocatable across every live node matching `label_selector`
    (e.g. "nvidia.com/gpu.product=NVIDIA-L40") -- ground truth read straight off the vendor's own
    node labels, ported from tests/scenarios/acceptable-accelerator-types.sh."""
    out = subprocess.run(
        ["kubectl", "get", "nodes", "-l", label_selector, "-o", "json"],
        capture_output=True, text=True, timeout=15,
    ).stdout
    try:
        doc = json.loads(out)
    except Exception:
        return 0
    return sum(int(n.get("status", {}).get("allocatable", {}).get("nvidia.com/gpu", 0)) for n in doc.get("items") or [])


def pod_cpu_resources(experiment_id: str) -> tuple[str | None, str | None]:
    """(requests.cpu, limits.cpu) of the one pod labeled for `experiment_id`, or (None, None) if
    no pod is found. A bounded, UUID-namespaced lookup -- safe for `parallel` tests, unlike the
    cluster-wide kubectl helpers reserved for `exclusive` fault scenarios."""
    try:
        pod = subprocess.run(
            ["kubectl", "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
             "-o", "jsonpath={.items[0].metadata.name}"],
            capture_output=True, text=True, timeout=15,
        ).stdout.strip()
        if not pod:
            return None, None
        req = subprocess.run(
            ["kubectl", "-n", JOB_NS, "get", "pod", pod, "-o", "jsonpath={.spec.containers[0].resources.requests.cpu}"],
            capture_output=True, text=True, timeout=15,
        ).stdout.strip()
        lim = subprocess.run(
            ["kubectl", "-n", JOB_NS, "get", "pod", pod, "-o", "jsonpath={.spec.containers[0].resources.limits.cpu}"],
            capture_output=True, text=True, timeout=15,
        ).stdout.strip()
        return req or None, lim or None
    except Exception:
        return None, None


def _kubectl(*args: str) -> str:
    try:
        return subprocess.run(
            ["kubectl", *args], capture_output=True, text=True, timeout=15
        ).stdout.strip()
    except Exception:
        return ""


def job_pod(experiment_id: str) -> str | None:
    """Name of the first pod labeled for `experiment_id`, or None. Bounded/UUID-namespaced --
    ported from tests/lib/cluster.sh::job_pod."""
    pod = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "-o", "jsonpath={.items[0].metadata.name}",
    )
    return pod or None


def job_pod_count(experiment_id: str) -> int:
    """Ported from tests/lib/cluster.sh::job_pod_count."""
    out = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "--no-headers",
    )
    return len([line for line in out.splitlines() if line.strip()])


def job_distinct_node_count(experiment_id: str) -> int:
    """Ported from tests/lib/cluster.sh::job_distinct_node_count."""
    out = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "-o", 'jsonpath={range .items[*]}{.spec.nodeName}{"\\n"}{end}',
    )
    nodes = {line.strip() for line in out.splitlines() if line.strip()}
    return len(nodes)


def pod_grace_seconds(experiment_id: str) -> int | None:
    """terminationGracePeriodSeconds of the first pod labeled for `experiment_id`, or None if the
    pod is not yet readable. The window physically lives on the pod: deleting a Job hands its
    pods to the garbage collector, which honours whatever the POD declares. Ported from
    tests/scenarios/preemption-requeue.sh::pod_grace_seconds."""
    out = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "-o", "jsonpath={.items[0].spec.terminationGracePeriodSeconds}",
    )
    return int(out) if out else None


def job_uid(experiment_id: str) -> str | None:
    """Ported from tests/lib/cluster.sh::job_uid."""
    uid = _kubectl(
        "-n", JOB_NS, "get", "jobs", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "-o", "jsonpath={.items[0].metadata.uid}",
    )
    return uid or None


def job_recreated_with_new_uid(experiment_id: str, old_uid: str) -> bool:
    """Ported from tests/lib/cluster.sh::job_recreated_with_new_uid."""
    current = job_uid(experiment_id)
    return bool(current) and current != old_uid


def job_resource_exists(experiment_id: str) -> bool:
    """True while a Kubernetes Job still exists for `experiment_id`. Ported from
    tests/lib/cluster.sh::job_resource_exists."""
    return job_uid(experiment_id) is not None


def job_resource_absent(experiment_id: str) -> bool:
    """True once no Kubernetes Job remains for `experiment_id`. Ported from
    tests/lib/cluster.sh::job_resource_absent (`! job_resource_exists`)."""
    return job_uid(experiment_id) is None


def job_node(experiment_id: str) -> str | None:
    """Node name of the first pod labeled for `experiment_id`, or None. Ported from
    tests/lib/cluster.sh::job_node."""
    node = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "-o", "jsonpath={.items[0].spec.nodeName}",
    )
    return node or None


def job_rescheduled_off(experiment_id: str, old_node: str) -> bool:
    """True once the job has a Running pod on a node other than `old_node`. Ported from
    tests/lib/cluster.sh::job_rescheduled_off."""
    new_node = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "--field-selector=status.phase=Running", "-o", "jsonpath={.items[0].spec.nodeName}",
    )
    return bool(new_node) and new_node != old_node


def delete_job_resource(experiment_id: str) -> None:
    """Deletes the Kubernetes Job for `experiment_id` while PostgreSQL still desires it. Ported
    from tests/lib/cluster.sh::delete_job_resource."""
    subprocess.run(
        ["kubectl", "-n", JOB_NS, "delete", "jobs", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
         "--wait=true"],
        capture_output=True, text=True, timeout=30,
    )


def corrupt_job_desired_hash(experiment_id: str) -> None:
    """Mutates the actual Job's desired-spec-hash annotation to force the stateless reconciler to
    detect drift and recreate it from PostgreSQL. Ported from
    tests/lib/cluster.sh::corrupt_job_desired_hash."""
    _kubectl(
        "-n", JOB_NS, "annotate", "jobs", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "hypothesisloop.io/desired-spec-hash=externally-mutated", "--overwrite",
    )


def kill_node_running_job(experiment_id: str) -> str | None:
    """Cordons the node running `experiment_id`'s Running pod and force-deletes that pod
    (simulates real infra loss). Returns the killed node's name, or None if the job currently has
    no Running pod to kill. Ported from tests/lib/cluster.sh::kill_node_running_job."""
    out = _kubectl(
        "-n", JOB_NS, "get", "pods", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "--field-selector=status.phase=Running",
        "-o", 'jsonpath={.items[0].metadata.name}{" "}{.items[0].spec.nodeName}',
    )
    parts = out.split()
    if len(parts) != 2:
        return None
    pod, node = parts
    subprocess.run(["kubectl", "cordon", node], capture_output=True, text=True, timeout=15)
    subprocess.run(
        ["kubectl", "-n", JOB_NS, "delete", "pod", pod, "--grace-period=0", "--force"],
        capture_output=True, text=True, timeout=15,
    )
    return node


def uncordon_node(node: str) -> None:
    """Ported from tests/lib/cluster.sh::uncordon_node. Best-effort -- always safe to call in a
    cleanup path even if the node is already schedulable or gone."""
    subprocess.run(["kubectl", "uncordon", node], capture_output=True, text=True, timeout=15)


def restart_node_agent_daemonset() -> None:
    """Rolling-restarts the per-node metrics DaemonSet and waits for it to come back. Ported from
    tests/lib/cluster.sh::restart_node_agent_daemonset."""
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "rollout", "restart", "daemonset/hypothesisloop-node-agent"],
        capture_output=True, text=True, timeout=30,
    )
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "rollout", "status", "daemonset/hypothesisloop-node-agent", "--timeout=60s"],
        capture_output=True, text=True, timeout=70,
    )


def all_cordoned_nodes() -> list[str]:
    """Names of every node currently cordoned (spec.unschedulable=true) -- used by a cleanup path
    to uncordon anything a fault test may have left behind, without needing to know which node it
    was."""
    out = _kubectl(
        "get", "nodes", "-o",
        'jsonpath={range .items[?(@.spec.unschedulable==true)]}{.metadata.name}{"\\n"}{end}',
    )
    return [line.strip() for line in out.splitlines() if line.strip()]


def cluster_agent_name() -> str:
    """CLUSTER_NAME the live cluster-agent Deployment reports itself as -- read straight off its
    own env var rather than hardcoded, since it's whatever the local dev deploy was configured
    with. Ported from tests/scenarios/connectivity-loss.sh's inline jsonpath lookup."""
    return _kubectl(
        "-n", CLUSTER_NS, "get", "deployment/hypothesisloop-cluster-agent",
        "-o", '''jsonpath={.spec.template.spec.containers[?(@.name=="cluster-agent")].env[?(@.name=="CLUSTER_NAME")].value}''',
    )


def disconnect_cluster_agent() -> None:
    """Scales the cluster-agent Deployment to 0, stopping all outbound reports/reconciliation
    without touching the control plane or any already-running job pods -- the cleanest available
    proxy for "this cluster lost network reachability" in a single-cluster local dev setup (no
    separate network segment to partition). Ported from tests/lib/cluster.sh::disconnect_cluster_agent."""
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "scale", "deployment/hypothesisloop-cluster-agent", "--replicas=0"],
        capture_output=True, text=True, timeout=30,
    )
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "wait", "--for=delete", "pod", "-l", "app=hypothesisloop-cluster-agent", "--timeout=30s"],
        capture_output=True, text=True, timeout=35,
    )


def reconnect_cluster_agent() -> None:
    """Ported from tests/lib/cluster.sh::reconnect_cluster_agent. Best-effort -- callers must
    still assert reconnection via cluster_agent_connected()/eventually(), this only issues the
    scale-up and waits for the rollout to finish starting the pod."""
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "scale", "deployment/hypothesisloop-cluster-agent", "--replicas=1"],
        capture_output=True, text=True, timeout=30,
    )
    subprocess.run(
        ["kubectl", "-n", CLUSTER_NS, "rollout", "status", "deployment/hypothesisloop-cluster-agent", "--timeout=60s"],
        capture_output=True, text=True, timeout=65,
    )


def cluster_agent_connected() -> bool:
    """Ported from tests/lib/cluster.sh::cluster_agent_connected: the Deployment reports exactly
    one ready replica."""
    ready = _kubectl(
        "-n", CLUSTER_NS, "get", "deployment/hypothesisloop-cluster-agent",
        "-o", "jsonpath={.status.readyReplicas}",
    )
    return ready == "1"


def cluster_agent_disconnected() -> bool:
    return not cluster_agent_connected()


def corrupt_service_desired_hash(experiment_id: str) -> None:
    """Mutates the companion Service's desired-spec-hash annotation to force the stateless
    reconciler to repair it from PostgreSQL. Bounded to one UUID-namespaced Service, unlike the
    cluster-wide fault-injection helpers reserved for `exclusive` scenarios. Ported from
    tests/lib/cluster.sh::corrupt_service_desired_hash."""
    _kubectl(
        "-n", JOB_NS, "annotate", "service", "-l", f"hypothesisloop.io/experiment-id={experiment_id}",
        "hypothesisloop.io/desired-spec-hash=externally-mutated", "--overwrite",
    )


def _millicores(cpu: str) -> int:
    return int(float(cpu[:-1])) if cpu.endswith("m") else int(float(cpu) * 1000)


def single_eligible_node_cpu_fair_share(accel_type: str) -> tuple[str, int, int] | None:
    """Ground truth for domain.FairShare's denominator/numerator on a single-node cluster,
    ported from max-resource-sentinel.sh's inline Python. Returns (node_name,
    installed_accelerators, expected_fair_share_millicores), or None if the live cluster does not
    have EXACTLY one node currently reporting live capacity for `accel_type` -- a second eligible
    node makes "the minimum across eligible nodes" and "this node's own share" diverge, and which
    node the scheduler actually picks becomes unpredictable, so the caller should skip rather than
    guess (see the bash scenario's header comment for why this scenario needs single-node
    unambiguity).

    Mirrors resolveClusterLocalResources's real inputs: raw allocatable CPU from `kubectl get
    nodes`, minus the CPU requests of every resident pod NOT labeled
    hypothesisloop.io/managed-by=hypothesisloop (DaemonSets, CNI, monitoring, etc) -- see
    runtime/k8s/internal/k8sexec/job_lifecycle.go's GetNodeTotalCapacity. Raw allocatable alone
    would overstate free capacity on a cluster with real DaemonSets.
    """
    vendor = accel_type.split("/", 1)[0]
    key, _, value = accel_type.partition("=")
    nodes_json = _kubectl("get", "nodes", "-o", "json")
    try:
        nodes = json.loads(nodes_json).get("items") or []
    except Exception:
        return None

    carriers: list[tuple[str, int, int]] = []
    for node in nodes:
        if (node.get("metadata", {}).get("labels") or {}).get(key) != value:
            continue
        alloc = node.get("status", {}).get("allocatable") or {}
        installed = sum(int(v) for k, v in alloc.items() if k.startswith(f"{vendor}/") and v.lstrip("-").isdigit())
        if installed <= 0:
            continue
        cpu = alloc.get("cpu", "0")
        carriers.append((node["metadata"]["name"], installed, _millicores(cpu)))

    if len(carriers) != 1:
        return None
    node_name, installed, raw_milli = carriers[0]

    pods_json = _kubectl(
        "get", "pods", "--all-namespaces", "--field-selector", f"spec.nodeName={node_name}", "-o", "json",
    )
    free = raw_milli
    try:
        pods = json.loads(pods_json).get("items") or []
    except Exception:
        pods = []
    for pod in pods:
        if (pod.get("status") or {}).get("phase") in ("Succeeded", "Failed"):
            continue
        if (pod.get("metadata", {}).get("labels") or {}).get("hypothesisloop.io/managed-by") == "hypothesisloop":
            continue
        for ctr in pod.get("spec", {}).get("containers", []):
            cpu = (ctr.get("resources", {}).get("requests") or {}).get("cpu")
            if cpu:
                free -= _millicores(cpu)
    free = max(0, free)

    return node_name, installed, free // installed


@dataclass
class DisbalanceNodeShape:
    """Ground truth for whether a live cluster can express a CPU/accelerator disbalance for
    `accel_type`, ported from resource-disbalance-evict.sh's inline Python node-selection block.

    `node`/`installed`/`free_cpu_cores` describe the node carrying the MOST accelerators of this
    flavor (empty/0/0.0 if none carry any); `flavor_nodes` is how many live nodes carry the label
    at all, regardless of accelerator count -- the scenario only fires when exactly one does,
    otherwise the blocked job would just land on a different node and prove nothing.
    """

    node: str
    installed: int
    free_cpu_cores: float
    flavor_nodes: int


def disbalance_node_shape(accel_type: str) -> DisbalanceNodeShape:
    vendor = accel_type.split("/", 1)[0]
    key, _, value = accel_type.partition("=")
    try:
        nodes = json.loads(_kubectl("get", "nodes", "-o", "json")).get("items") or []
    except Exception:
        return DisbalanceNodeShape("", 0, 0.0, 0)

    best_node, best_count, best_cpu_milli, flavor_nodes = "", 0, 0, 0
    for node in nodes:
        if (node.get("metadata", {}).get("labels") or {}).get(key) != value:
            continue
        flavor_nodes += 1
        alloc = node.get("status", {}).get("allocatable") or {}
        count = sum(int(v) for k, v in alloc.items() if k.startswith(f"{vendor}/") and v.lstrip("-").isdigit())
        if count <= best_count:
            continue
        best_node, best_count, best_cpu_milli = node["metadata"]["name"], count, _millicores(alloc.get("cpu", "0"))

    if not best_node:
        return DisbalanceNodeShape("", 0, 0.0, flavor_nodes)

    # Same platform-local denominator as GetNodeTotalCapacity (runtime/k8s/internal/k8sexec/
    # job_lifecycle.go): raw allocatable minus the CPU requests of every resident pod NOT labeled
    # hypothesisloop.io/managed-by=hypothesisloop.
    pods_json = _kubectl(
        "get", "pods", "--all-namespaces", "--field-selector", f"spec.nodeName={best_node}", "-o", "json",
    )
    free_milli = best_cpu_milli
    try:
        pods = json.loads(pods_json).get("items") or []
    except Exception:
        pods = []
    for pod in pods:
        if (pod.get("status") or {}).get("phase") in ("Succeeded", "Failed"):
            continue
        if (pod.get("metadata", {}).get("labels") or {}).get("hypothesisloop.io/managed-by") == "hypothesisloop":
            continue
        for ctr in pod.get("spec", {}).get("containers", []):
            cpu = (ctr.get("resources", {}).get("requests") or {}).get("cpu")
            if cpu:
                free_milli -= _millicores(cpu)
    free_milli = max(0, free_milli)

    return DisbalanceNodeShape(best_node, best_count, free_milli / 1000.0, flavor_nodes)


def pod_resource(experiment_id: str, resource: str) -> tuple[str | None, str | None]:
    """(requests.<resource>, limits.<resource>) of the one pod labeled for `experiment_id`, or
    (None, None) if no pod is found. Generalizes pod_cpu_resources to memory/accelerator
    resource names (e.g. "memory", "nvidia.com/gpu")."""
    pod = job_pod(experiment_id)
    if not pod:
        return None, None
    escaped = resource.replace(".", "\\.")
    req = _kubectl(
        "-n", JOB_NS, "get", "pod", pod, "-o", f"jsonpath={{.spec.containers[0].resources.requests.{escaped}}}",
    )
    lim = _kubectl(
        "-n", JOB_NS, "get", "pod", pod, "-o", f"jsonpath={{.spec.containers[0].resources.limits.{escaped}}}",
    )
    return req or None, lim or None


# SCOPE_TOKENS mirrors lib/scope.sh's per-token accelerator-type expansion (never actually wired
# up in the bash suite -- lib/scope.sh was written but never sourced, per tests/improve.md's
# header table. This pytest port is the first real user). Extend this mapping (not the caller)
# when a new exclusive scenario introduces a new scope token.
L40 = "nvidia.com/gpu.product=NVIDIA-L40"
A100 = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
H100 = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
SCOPE_TOKENS: dict[str, list[str]] = {
    "l40": [L40],
    "a100": [A100],
    "h100": [H100],
    "cluster": [L40, A100, H100],
}


class ScopeNotIdle(AssertionError):
    pass


@dataclass
class _Reading:
    busy: int  # -1 means "unreadable", never treated as idle or as progress
    present: int


def _read_busy(api: API, want_types: set[str]) -> _Reading:
    # THE FIX (tests/improve.md §3b, run.sh:283): a failed/unreachable capacity read must NOT read
    # as "0 busy". The old bash barrier piped a failed curl into `awk '{print $1+0}'`, and empty
    # input to that awk program prints "0" -- which then passed the `^[0-9]+$` check, so an
    # unreachable control plane was silently treated as an idle cluster and handed to an exclusive
    # scenario that does not actually own it. -1 is not a valid busy count and is refused by every
    # comparison below, so an unreadable capacity response can never look like progress or like an
    # idle scope.
    try:
        doc = api.capacity()
    except Exception:
        return _Reading(busy=-1, present=0)
    if not isinstance(doc, dict) or "clusters" not in doc:
        return _Reading(busy=-1, present=0)
    busy = 0
    found: set[str] = set()
    for cluster in doc.get("clusters") or []:
        for a in cluster.get("accelerators") or []:
            t = a.get("accelerator_type")
            if t in want_types:
                found.add(t)
                busy += int(a.get("total") or 0) - int(a.get("available") or 0)
    return _Reading(busy=busy, present=len(found))


def wait_scope_idle(api: API, scope_tokens: list[str], *, stall_seconds: int = 180, poll_interval: float = 5.0) -> None:
    """Blocks until every accelerator type named by `scope_tokens` reports 0 busy, resetting the
    stall budget on any drop in the busy count (progress-based, not a flat ceiling -- see
    lib/scope.sh's header comment for why a flat ceiling that "continues anyway" on expiry is worse
    than failing loudly). Raises ScopeNotIdle if the busy count stops falling for `stall_seconds`
    with no further release, or if capacity never becomes readable.
    """
    want_types: set[str] = set()
    for tok in scope_tokens:
        want_types.update(SCOPE_TOKENS.get(tok, []))
    if not want_types:
        return

    seen_types = 0
    last_busy = -1
    deadline = time.monotonic() + stall_seconds
    last_reading = _Reading(busy=-1, present=0)
    while time.monotonic() < deadline:
        reading = _read_busy(api, want_types)
        last_reading = reading
        busy, present = reading.busy, reading.present
        # A type that WAS reported and then disappears is a cluster that dropped out while still
        # holding accelerators, not proof of idleness -- treat a shrinking response as unreadable.
        if present < seen_types:
            busy = -1
        elif present > seen_types:
            seen_types = present
        if busy == 0:
            return
        if busy >= 0 and (last_busy < 0 or busy < last_busy):
            last_busy = busy
            deadline = time.monotonic() + stall_seconds
        time.sleep(poll_interval)

    raise ScopeNotIdle(
        f"scope {sorted(want_types)} still reports {last_reading.busy} accelerator(s) busy after "
        f"{stall_seconds}s with no further release"
    )
