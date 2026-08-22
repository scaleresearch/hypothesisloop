#!/usr/bin/env python3
"""
Distributed e2e workload: every rank joins one process group and contributes its index to a
single all-reduce, then reports the sum as a metric.

For num_nodes N the only correct value is N(N-1)/2. A rank that never joined makes it smaller,
and a rank that joined twice makes it larger — so the assertion is on a number the platform
cannot fake, rather than on a pod spec that merely looks right.

The rendezvous is hand-rolled over MASTER_ADDR/MASTER_PORT rather than torch.distributed. What is
under test is the platform's guarantee — that every rank resolves the same MASTER_ADDR, that rank
0 keeps that name for the job's life, and that the gang forms at all — not torch's implementation
of gloo. A pure-stdlib rendezvous proves exactly that, reads the same env://-style variables a
real torchrun job would, and keeps the portable suite's image a 60MB python-slim instead of a
multi-gigabyte torch one that every node would have to pull before any assertion could run.

Environment (injected by the runtime, see runtime/k8s/internal/k8sexec/job_build.go):
  RANK / HYPOTHESISLOOP_RANK              this node's index, 0..WORLD_SIZE-1 (UNGROUPED jobs only)
  HYPOTHESISLOOP_RANK_OFFSET              this group's first global rank    (GROUPED jobs only)
  HYPOTHESISLOOP_GROUP_RANK               this pod's index within its group (GROUPED jobs only)
  HYPOTHESISLOOP_GROUP                    which group this pod belongs to   (GROUPED jobs only)
  WORLD_SIZE / HYPOTHESISLOOP_WORLD_SIZE  the node count, job-global in both shapes
  MASTER_ADDR / MASTER_PORT               rank 0's stable DNS name and port
  HYPOTHESISLOOP_ACCELERATOR_COUNT        accelerators on THIS node (never the job total)
  HYPOTHESISLOOP_ATTEMPT                  which attempt this is, 0 on the first
"""

import json
import os
import socket
import sys
import time
import urllib.request

EXP_ID = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
API_URL = os.environ.get("HYPOTHESISLOOP_API_URL", "http://localhost:8081")
METRIC = os.environ.get("HYPOTHESISLOOP_PRIMARY_METRIC", "val_accuracy")

def resolve_rank() -> int:
    """This node's JOB-GLOBAL rank, whichever shape the job was submitted in.

    A grouped job's pods are deliberately not given RANK. A job-global rank is the group's
    offset plus the pod's index within its group, and Kubernetes cannot supply that sum: the
    downward API hands a pod its completion index and env expansion concatenates strings, but
    nothing adds them. Setting RANK to the group-local index instead would put two pods at rank 0
    for every group after the first — a hang or a silently corrupt all-reduce, never an error
    anyone sees. So the platform publishes the two terms separately and the workload adds them,
    and an absent RANK on a grouped pod fails loudly here rather than reducing the wrong number.
    """
    offset = os.environ.get("HYPOTHESISLOOP_RANK_OFFSET")
    group_rank = os.environ.get("HYPOTHESISLOOP_GROUP_RANK")
    if offset is not None and group_rank is not None:
        return int(offset) + int(group_rank)
    # Ungrouped job: every pod's index is already its global rank, so the runtime sets RANK
    # itself and there is nothing to add.
    return int(os.environ.get("RANK", os.environ.get("HYPOTHESISLOOP_RANK", "0")))


RANK = resolve_rank()
GROUP = os.environ.get("HYPOTHESISLOOP_GROUP", "")
WORLD_SIZE = int(os.environ.get("WORLD_SIZE", os.environ.get("HYPOTHESISLOOP_WORLD_SIZE", "1")))
MASTER_ADDR = os.environ.get("MASTER_ADDR", os.environ.get("HYPOTHESISLOOP_MASTER_ADDR", "127.0.0.1"))
MASTER_PORT = int(os.environ.get("MASTER_PORT", os.environ.get("HYPOTHESISLOOP_MASTER_PORT", "29500")))

ACCELERATOR_COUNT = int(os.environ.get("HYPOTHESISLOOP_ACCELERATOR_COUNT", "1"))
ATTEMPT = int(os.environ.get("HYPOTHESISLOOP_ATTEMPT", "0"))
DURATION = int(os.environ.get("HYPOTHESISLOOP_DURATION_SECONDS", "30"))
# Rank to kill after the barrier, for the gang-failure guarantee. Unset means nobody fails.
FAIL_RANK = os.environ.get("FAIL_RANK", "")

# Generous because it covers pod scheduling, image pull and container start on every other node,
# not just the socket connect. Still far below any collective-library timeout, so a scenario
# asserting "the survivors died promptly rather than blocking until the collective timed out" has
# an unambiguous window to measure in.
RENDEZVOUS_TIMEOUT = int(os.environ.get("RENDEZVOUS_TIMEOUT_SECONDS", "180"))


def log(message: str) -> None:
    print(f"[rank {RANK}/{WORLD_SIZE}] {message}", flush=True)


def post_metric(name: str, value: float, fraction: float) -> None:
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps(
        {"metric_name": name, "fraction_complete": fraction, "metric_value": value}
    ).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:  # reporting must never take down the work it reports on
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr, flush=True)


def all_reduce_rank0(deadline: float) -> int:
    """Rank 0: collect every other rank's contribution, sum, broadcast the total back."""
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("0.0.0.0", MASTER_PORT))
    server.listen(WORLD_SIZE)

    total = RANK
    seen = {RANK}
    peers = []
    log(f"listening on 0.0.0.0:{MASTER_PORT} for {WORLD_SIZE - 1} peer(s)")
    while len(seen) < WORLD_SIZE:
        remaining = deadline - time.time()
        if remaining <= 0:
            raise TimeoutError(
                f"only {len(seen)}/{WORLD_SIZE} ranks joined before the rendezvous deadline; "
                f"joined = {sorted(seen)}"
            )
        server.settimeout(remaining)
        try:
            conn, _ = server.accept()
        except TimeoutError:
            # accept() raises its own bare TimeoutError, which says nothing about which ranks are
            # missing — and "which ranks never joined" is the entire diagnostic value of this
            # failure to whoever reads the log.
            raise TimeoutError(
                f"only {len(seen)}/{WORLD_SIZE} ranks joined before the rendezvous deadline; "
                f"joined = {sorted(seen)}, missing = {sorted(set(range(WORLD_SIZE)) - seen)}"
            ) from None
        peer_rank = int(conn.recv(64).decode().strip())
        if peer_rank in seen:
            raise ValueError(f"rank {peer_rank} joined twice — the gang is not what it claims")
        seen.add(peer_rank)
        total += peer_rank
        peers.append(conn)
        log(f"rank {peer_rank} joined ({len(seen)}/{WORLD_SIZE})")

    for conn in peers:
        conn.sendall(f"{total}\n".encode())
        conn.close()
    server.close()
    return total


def all_reduce_worker(deadline: float) -> int:
    """Every other rank: contribute this rank's index to rank 0, read the total back."""
    last_error = None
    while time.time() < deadline:
        try:
            conn = socket.create_connection((MASTER_ADDR, MASTER_PORT), timeout=10)
            conn.sendall(f"{RANK}\n".encode())
            conn.settimeout(max(1.0, deadline - time.time()))
            total = int(conn.recv(64).decode().strip())
            conn.close()
            return total
        except OSError as e:
            # Rank 0's pod may not be up yet. Retrying the connect is the rendezvous, not a
            # fallback: there is one master and one address, and this is how a rank waits for it.
            last_error = e
            time.sleep(1)
    raise TimeoutError(f"could not reach MASTER_ADDR {MASTER_ADDR}:{MASTER_PORT}: {last_error}")


def main() -> None:
    log(
        f"starting — master={MASTER_ADDR}:{MASTER_PORT} accelerators_on_this_node={ACCELERATOR_COUNT} "
        f"attempt={ATTEMPT} group={GROUP or '<ungrouped>'}"
    )
    # Asserted here as well as in the scenario: a pod told it holds more accelerators than it does
    # is the exact failure that made the per-node count wrong, and the workload is the only place
    # that can see what it was actually handed.
    if ACCELERATOR_COUNT < 1:
        raise SystemExit(f"HYPOTHESISLOOP_ACCELERATOR_COUNT={ACCELERATOR_COUNT}, want at least 1")

    deadline = time.time() + RENDEZVOUS_TIMEOUT
    started = time.time()
    total = all_reduce_rank0(deadline) if RANK == 0 else all_reduce_worker(deadline)
    expected = WORLD_SIZE * (WORLD_SIZE - 1) // 2
    log(f"all_reduce complete in {time.time() - started:.1f}s: total={total} expected={expected}")

    if total != expected:
        raise SystemExit(f"all_reduce total {total} != expected {expected} — the gang did not form")

    # Reported by every rank, with the same value, so the scenario reads one number regardless of
    # which rank's report the metrics store happened to keep.
    post_metric("world_reduced_sum", float(total), 1.0)
    post_metric("world_size_observed", float(WORLD_SIZE), 1.0)
    post_metric("accelerators_per_node", float(ACCELERATOR_COUNT), 1.0)
    # Which attempt this is. A gang retry reuses the experiment id, so the only way a scenario can
    # tell "one attempt" from "two" is a value the workload itself reports per attempt — the
    # platform's own record shows a single experiment either way.
    post_metric("gang_attempt", float(ATTEMPT), 1.0)

    if FAIL_RANK != "" and RANK == int(FAIL_RANK):
        # After the barrier, so every rank is provably running when this one dies — which is what
        # makes "the survivors were torn down" a statement about the gang policy rather than about
        # a rank that never started.
        log("failing deliberately after the barrier (FAIL_RANK)")
        sys.exit(3)

    # Hold the gang up for the rest of the declared duration, reporting as it goes, so the job
    # looks like a real run to the silence checks rather than exiting the instant it starts.
    steps = max(1, DURATION // 5)
    for step in range(steps):
        time.sleep(5)
        post_metric(METRIC, 0.5 + 0.4 * (step + 1) / steps, (step + 1) / steps)

    log("done")


if __name__ == "__main__":
    main()
