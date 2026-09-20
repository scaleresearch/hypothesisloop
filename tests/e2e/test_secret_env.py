"""A job's secret_env: the workload gets it as environment variables, and no read of the job, no
event and no log of the platform shows it."""
from __future__ import annotations

import shlex
import uuid

import pytest
import yaml

from conftest import make_agent
from support import s3
from support.api import JobRequest
from support.wait import eventually

ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "stage_rank", "direction": "maximize"},
    {"key": "checkpoint_write_status", "direction": "maximize"},
]


@pytest.mark.parallel
def test_secret_env_reaches_the_workload_and_no_read_of_the_job(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "secret-env")
    pe_id = experiment("secret-env", [agent], budget=1.0, metrics=METRICS)
    secret = f"secret-{uuid.uuid4().hex}"

    # data_stage.py writes $CHECKPOINT_VALUE as its checkpoint, so the checkpoint's body is what
    # the workload found in its environment.
    job_id = api.submit_job(
        pe_id, agent, hours=0.02,
        job_overrides={
            "command": ["python", "data_stage.py"],
            "max_retries": 0,
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "env": {"HYPOTHESISLOOP_DURATION_SECONDS": "10", "STAGE_MODE": "write"},
        },
        secret_env={"CHECKPOINT_VALUE": secret},
    )
    eventually(
        "the job to write its checkpoint and complete",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] == "COMPLETED",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )

    key = {}
    for line in api.hl("data-key", "--platform-experiment", pe_id, "--agent", agent).splitlines():
        name, _, value = line.removeprefix("export ").partition("=")
        key[name] = shlex.split(value)[0]
    store = s3.client(key)
    bucket, shared_prefix = store.split_s3_uri(key["HYPOTHESISLOOP_DATA_SHARED"])
    written = [k for k in store.list_keys(bucket, shared_prefix) if k.startswith(f"{pe_id}/{agent}/{job_id}/")]
    assert len(written) == 1, f"the job left {written}, want its one checkpoint"
    status, body = store.s3("GET", store.object_path(bucket, written[0]))
    assert (status, body.decode()) == (200, secret), f"the workload did not find the secret in its environment: {status} {body[:200]!r}"

    reads = {
        "GET /experiments/{id}": api.get(f"/experiments/{job_id}"),
        "GET /experiments": api.get("/experiments", params={"platform_experiment_id": pe_id}),
        "GET /experiments/{id}/logs": api.get(f"/experiments/{job_id}/logs"),
        "GET /platform-experiments/{id}/results": api.get(f"/platform-experiments/{pe_id}/results"),
    }
    for name, r in reads.items():
        assert r.status_code == 200, f"{name}: HTTP {r.status_code}: {r.text[:300]}"
        assert secret not in r.text and "secret_env" not in r.text, f"{name} shows the job's secret_env: {r.text[:600]}"
    assert secret not in api.hl("job", "status", job_id), "hl job status shows the secret"


@pytest.mark.parallel
def test_a_name_in_both_env_and_secret_env_is_refused(api, experiment, run_id):
    agent = make_agent(api, run_id, "secret-clash")
    pe_id = experiment("secret-env-clash", [agent], budget=1.0, metrics=METRICS)
    hyp_id = api.register_hypothesis(pe_id, agent, "")["id"]
    job_id = f"secret-clash-{uuid.uuid4().hex[:8]}"
    req = JobRequest(
        id=job_id, agent_id=agent, platform_experiment_id=pe_id, hypothesis_id=hyp_id, hours=0.02, tier="guaranteed",
        job_overrides={"env": {"HF_TOKEN": "open"}, "secret_env": {"HF_TOKEN": "the-secret-value"}},
    )
    r = api.post("/experiments", yaml.safe_load(req.to_yaml()))
    assert r.status_code == 400, f"HTTP {r.status_code}: {r.text}"
    assert "HF_TOKEN" in r.text and "the-secret-value" not in r.text, r.text
    assert api.get(f"/experiments/{job_id}").status_code == 404, "the refused job was stored"
