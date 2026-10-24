#!/usr/bin/env python3
"""
A torch.distributed gang on gloo: every rank joins one process group through env:// (the
MASTER_ADDR, MASTER_PORT, RANK and WORLD_SIZE the runtime sets) and all-reduces its rank. The only
correct sum is N(N-1)/2, and gloo opens a connection between every pair of ranks, so the sum also
says that every rank reached every other one, beyond rank 0's rendezvous port.

Only rank 0 posts metrics, as a real job does: every rank's post lands on the same job.
"""

import json
import os
import urllib.request

import torch
import torch.distributed as dist

EXP_ID = os.environ["HYPOTHESISLOOP_EXPERIMENT_ID"]
API_URL = os.environ["HYPOTHESISLOOP_API_URL"]
METRIC = os.environ.get("HYPOTHESISLOOP_PRIMARY_METRIC", "val_accuracy")


def post_metric(name: str, value: float) -> None:
    payload = json.dumps({"metric_name": name, "fraction_complete": 1.0, "metric_value": value}).encode()
    req = urllib.request.Request(f"{API_URL}/experiments/{EXP_ID}/metrics", data=payload,
                                 headers={"Content-Type": "application/json", "Authorization": f"Bearer {os.environ['HYPOTHESISLOOP_TOKEN']}"})
    urllib.request.urlopen(req, timeout=10)


def main() -> None:
    dist.init_process_group("gloo", init_method="env://")
    rank, world = dist.get_rank(), dist.get_world_size()
    print(f"[rank {rank}/{world}] joined the gloo process group", flush=True)
    total = torch.tensor([float(rank)])
    dist.all_reduce(total, op=dist.ReduceOp.SUM)
    expected = world * (world - 1) / 2
    print(f"[rank {rank}/{world}] all_reduce: total={total.item()} expected={expected}", flush=True)
    if total.item() != expected:
        raise SystemExit(f"all_reduce total {total.item()} != expected {expected}: the gang did not form")
    dist.barrier()
    if rank == 0:
        post_metric("world_reduced_sum", total.item())
        post_metric(METRIC, 0.5)
    dist.destroy_process_group()


if __name__ == "__main__":
    main()
