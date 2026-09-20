"""An agent's own key to its platform experiment's data, from `hl data-key`: it reads what any
agent's job wrote, writes under its own agent prefix, and is refused under another agent's."""
from __future__ import annotations

import shlex

import pytest

from conftest import make_agent
from support import s3
from support.wait import eventually

ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
CHECKPOINT_VALUE = "4242"
METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "stage_rank", "direction": "maximize"},
    {"key": "checkpoint_write_status", "direction": "maximize"},
]


def _data_key(api, pe_id: str, agent: str) -> dict[str, str]:
    """`hl data-key` output, parsed the way `eval` would read it."""
    env = {}
    for line in api.hl("data-key", "--platform-experiment", pe_id, "--agent", agent).splitlines():
        name, _, value = line.removeprefix("export ").partition("=")
        env[name] = shlex.split(value)[0]
    return env


@pytest.mark.parallel
def test_agent_key_reads_the_experiment_and_writes_only_its_own_prefix(api, experiment, run_id, deadline):
    agent_a, agent_b = (make_agent(api, run_id, f"datakey-{role}") for role in ("a", "b"))
    outsider = make_agent(api, run_id, "datakey-outsider")
    pe_id = experiment("agent-data-key", [agent_a, agent_b], budget=1.0, metrics=METRICS)

    job_a = api.submit_job(
        pe_id, agent_a, hours=0.02,
        job_overrides={
            "command": ["python", "data_stage.py"],
            "max_retries": 0,
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": "10", "STAGE_MODE": "write", "CHECKPOINT_VALUE": CHECKPOINT_VALUE},
        },
    )
    eventually(
        "agent A's job to write its checkpoint and complete",
        lambda: api.experiment(job_a),
        accept=lambda e: e["status"] == "COMPLETED",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    api.file_finding(job_a, "agent data key e2e: a job wrote a checkpoint.")

    refused = api.post(f"/platform-experiments/{pe_id}/data-key", {"agent_id": outsider})
    assert refused.status_code == 404, f"an agent that is not signed up got HTTP {refused.status_code}: {refused.text}"

    key = _data_key(api, pe_id, agent_b)
    assert key == _data_key(api, pe_id, agent_b), "a second request returned a different key"
    assert key["AWS_ENDPOINT_URL"] == key["AWS_ENDPOINT_URL_S3"]
    store = s3.client(key)
    bucket, shared_prefix = store.split_s3_uri(key["HYPOTHESISLOOP_DATA_SHARED"])
    _, own_prefix = store.split_s3_uri(key["HYPOTHESISLOOP_DATA_URI"])
    assert own_prefix == f"{pe_id}/{agent_b}/", f"the key's writable prefix is {own_prefix!r}"

    theirs = [k for k in store.list_keys(bucket, shared_prefix) if k.startswith(f"{pe_id}/{agent_a}/{job_a}/")]
    assert len(theirs) == 1, f"agent B lists {theirs} under agent A's job, want its one checkpoint"
    status, body = store.s3("GET", store.object_path(bucket, theirs[0]))
    assert (status, body.decode()) == (200, CHECKPOINT_VALUE), f"reading agent A's checkpoint: {status} {body[:200]!r}"

    status, body = store.s3("PUT", store.object_path(bucket, own_prefix + "note.txt"), body=b"agent b was here")
    assert status == 200, f"writing under the agent's own prefix: {status} {body[:200]!r}"

    status, body = store.s3("PUT", store.object_path(bucket, f"{pe_id}/{agent_a}/overwritten.txt"), body=b"must not land")
    assert status == 403, f"writing under another agent's prefix: {status} {body[:200]!r}, want 403"
    status, _ = store.s3("DELETE", store.object_path(bucket, theirs[0]))
    assert status == 403, f"deleting another agent's object: {status}, want 403"
