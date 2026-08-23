"""The whole human-facing flow through the Go `hl` CLI (cli/), end to end against a real
controlplane, ported 1:1 from tests/scenarios/cli-full-flow.sh: register a human identity, create
and start a platform experiment, sign up for it, submit and list a hypothesis, submit a real job
from a YAML file, list and inspect it, and watch it reach a terminal status -- asserting the CLI's
own output and exit codes at each step, not just the raw API. Every step of the flow a human or
agent needs goes through `hl` and a checked-in YAML file (experiment.yaml/hypothesis.yaml/job.yaml
shapes), never raw curl.

API-only, parallel-safe, one single-accelerator job (split into multiple test functions per
tests/improve.md's "split oversized scenarios" guidance -- each below is an independently
diagnosable behavior of the CLI, sharing only what genuinely has to be shared).
"""
from __future__ import annotations

import json
import subprocess
import time
import uuid

import pytest
import yaml

from conftest import TEST_ACCELERATOR_TYPE, make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.02
PE_BUDGET = 0.2
# Matches tests/lib/common.sh's ADMISSION_BUDGET_SECONDS -- the --timeout budget `hl watch --until`
# gets to observe one job reach a terminal status.
WATCH_TIMEOUT_SECONDS = 120


def _hl_popen(api, *args: str) -> subprocess.Popen:
    return subprocess.Popen(
        [str(api.hl_bin), *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=api.hl_env(),
    )


def _finish_popen(proc: subprocess.Popen, *, timeout: float) -> tuple[int, str, str]:
    """Always reaps the subprocess -- communicate() within a bounded timeout on the happy path,
    kill+reap on any exception or hang, so a stuck `hl watch` can never block the suite."""
    try:
        out, err = proc.communicate(timeout=timeout)
        return proc.returncode, out, err
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.communicate()


def test_hl_register_returns_the_registered_agent_id(api, run_id):
    # -- hl register --
    agent = f"human-cli-{uuid.uuid4().hex[:8]}-{run_id}"
    out = api.hl("register", "--id", agent, "--name", "CLI Flow Human", "--kind", "human")
    register_id = json.loads(out)["id"]
    assert register_id == agent, f"hl register: expected id {agent}, got {register_id}"


def test_cli_full_flow_happy_path(api, run_id, deadline):
    """register -> platform-experiments create/start -> signup -> hypothesis submit --file/list ->
    job submit -> job list/status -> watch --until a terminal status -> hypothesis dedup. Ported as
    one sequential test since each step consumes state (pe_id, hyp_id, job_id) produced by the one
    before it -- the same reason the bash scenario runs it as a single script."""
    agent = f"human-cli-{uuid.uuid4().hex[:8]}-{run_id}"
    api.hl("register", "--id", agent, "--name", "CLI Flow Human", "--kind", "human")

    # -- hl platform-experiments create / start --
    pe_body = {
        "name": f"cli-full-flow-{run_id}",
        "description": f"cli-full-flow-{run_id}",
        "budget_accelerator_hours": PE_BUDGET,
        "max_agents": 1,
        "report_interval_seconds": 10,
        "metrics": [{"key": "val_accuracy", "direction": "maximize"}],
    }
    pe_create_out = api.hl("platform-experiments", "create", "-", input=yaml.safe_dump(pe_body, sort_keys=False))
    pe_id = json.loads(pe_create_out)["id"]
    assert pe_id, f"hl platform-experiments create: no id in response: {pe_create_out}"

    # -- hl signup --
    signup_out = api.hl("signup", "--platform-experiment", pe_id, "--agent", agent)
    json.loads(signup_out)  # hl signup: returned valid JSON

    api.hl("platform-experiments", "start", "--id", pe_id)  # hl platform-experiments start: succeeded

    # -- hl hypothesis submit --file / list --
    hyp_text = f"Higher batch size improves throughput without hurting val_accuracy (cli-{run_id})"
    hyp_yaml_path = api.hl_bin.parent / f"cli-hypothesis-{run_id}.yaml"
    hyp_yaml_path.write_text(
        yaml.safe_dump({"agent_id": agent, "platform_experiment_id": pe_id, "text": hyp_text}, sort_keys=False)
    )
    try:
        submit_out = api.hl("hypothesis", "submit", "--file", str(hyp_yaml_path))
    finally:
        hyp_yaml_path.unlink(missing_ok=True)
    hyp_id = json.loads(submit_out)["id"]
    assert hyp_id, "hl hypothesis submit --file: no id in response"

    list_out = api.hl("hypothesis", "list", "--platform-experiment", pe_id, "--agent", agent)
    assert hyp_text in list_out, f"hl hypothesis list: submitted text missing from listing: {list_out}"

    # -- hl job submit --
    job_id = f"job-cli-{uuid.uuid4().hex[:8]}-{run_id}"
    job_body = {
        "id": job_id,
        "metadata": {
            "platform_experiment_id": pe_id,
            "project_id": "e2e",
            "hypothesis_id": hyp_id,
            "theory": "e2e scenario coverage",
            "objective": "maximize val_accuracy",
            "estimated_duration_hours": JOB_HOURS,
            "code_ref": "git://hypothesisloop@" + "a" * 40,
            "config_hash": "",
            "data_ref": "",
            "capacity_tier": "guaranteed",
        },
        "job": {
            "image": "localhost/hypothesisloop-workload:latest",
            "cpu": "250m",
            "memory": "128Mi",
            "storage": "512Mi",
            "max_retries": 3,
            "accelerator_count": 1,
            "accelerator_type": TEST_ACCELERATOR_TYPE,
            "accelerator_tolerations": ["nvidia.com/gpu"],
        },
    }
    job_submit_out = api.hl("job", "submit", "--agent", agent, "-", input=yaml.safe_dump(job_body, sort_keys=False))
    job_submit_id = json.loads(job_submit_out)["id"]
    assert job_submit_id == job_id, f"hl job submit: expected id {job_id}, got {job_submit_id}"

    # -- hl job list / status --
    list_jobs_out = api.hl("job", "list", "--agent", agent, "--platform-experiment", pe_id)
    assert job_id in list_jobs_out, f"hl job list: submitted job missing from listing: {list_jobs_out}"

    status_out = api.hl("job", "status", "--agent", agent, "--id", job_id)
    status_job_id = json.loads(status_out)["id"]
    assert status_job_id == job_id, f"hl job status: expected id {job_id}, got {status_job_id}"

    # -- hl watch --until --
    proc = _hl_popen(
        api, "watch", "--experiment", job_id,
        "--until", "status in COMPLETED,FAILED,EVICTED",
        "--timeout", str(WATCH_TIMEOUT_SECONDS),
    )
    code, out, err = _finish_popen(proc, timeout=WATCH_TIMEOUT_SECONDS + 30)
    assert code == 0, f"hl watch: exited {code} on reaching a terminal status (stderr: {err})"
    assert '"kind":"experiment.status"' in out, f"hl watch: no experiment.status event printed: {out}"

    # -- hl hypothesis submit: dedup --
    dedup_out = api.hl("hypothesis", "submit", "--agent", agent, "--platform-experiment", pe_id, "--text", hyp_text)
    dedup = json.loads(dedup_out)
    assert dedup["already_existed"] is True
    assert dedup["id"] == hyp_id, f"hl hypothesis submit: dedup returned {dedup['id']}, want {hyp_id}"

    api.close_platform_experiment(pe_id)


@pytest.fixture
def cli_watch_pe(api, experiment, run_id):
    agent = make_agent(api, run_id, "cli-watch")
    pe_id = experiment("cli-full-flow-watch", [agent], budget=PE_BUDGET)
    return pe_id, agent


def test_watch_comment_new_delivered_live(api, cli_watch_pe, deadline):
    # -- hl watch: comment.new delivered live --
    pe_id, agent = cli_watch_pe
    hyp_id = api.register_hypothesis(pe_id, agent, f"cli watch comment.new hypothesis {uuid.uuid4().hex[:8]}")["id"]

    proc = _hl_popen(api, "watch", "--platform-experiment", pe_id, "--kinds", "comment.new", "--timeout", "15")
    try:
        time.sleep(2)  # let the watch connection establish before posting, mirrors the bash scenario's `sleep 2`
        api.post_hypothesis_comment(
            hyp_id, f"does this hold at batch 512 ({uuid.uuid4().hex[:8]})?", agent_id=agent
        ).raise_for_status()
        code, out, err = _finish_popen(proc, timeout=30)
    except Exception:
        _finish_popen(proc, timeout=5)
        raise
    assert code == 0, f"hl watch --kinds comment.new: exited {code} (stderr: {err})"
    assert f'"kind":"comment.new","subject":"{hyp_id}"' in out, (
        f"hl watch --kinds comment.new: event missing or wrong subject: {out}"
    )


def test_watch_finding_new_delivered_live_after_job_completion(api, cli_watch_pe, deadline):
    # -- hl watch: finding.new delivered live after job completion --
    pe_id, agent = cli_watch_pe
    job_id = api.submit_job(pe_id, agent, hours=JOB_HOURS)
    final = eventually(
        f"{job_id} to reach a terminal status",
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    if final["status"] != "COMPLETED":
        pytest.skip(f"job ended {final['status']}, not COMPLETED -- filing a finding requires a terminal run")
    hyp_id = final["hypothesis_id"]

    proc = _hl_popen(api, "watch", "--platform-experiment", pe_id, "--kinds", "finding.new", "--timeout", "15")
    try:
        time.sleep(2)  # let the watch connection establish before filing, mirrors the bash scenario's `sleep 2`
        api.file_finding(job_id, f"cli-full-flow finding ({uuid.uuid4().hex[:8]})")
        code, out, err = _finish_popen(proc, timeout=30)
    except Exception:
        _finish_popen(proc, timeout=5)
        raise
    assert code == 0, f"hl watch --kinds finding.new: exited {code} (stderr: {err})"
    assert f'"kind":"finding.new","subject":"{hyp_id}","value":"{job_id}"' in out, (
        f"hl watch --kinds finding.new: event missing or wrong subject/value: {out}"
    )


def test_job_submit_malformed_yaml_exits_2(api, run_id):
    # -- hl job submit: real API failures surface cleanly (malformed YAML) --
    agent = make_agent(api, run_id, "cli-bad-yaml")
    proc = api.hl_expect("job", "submit", "--agent", agent, "-", input="not: [valid, yaml: broken\n")
    assert proc.returncode == 2, (
        f"hl job submit: malformed YAML exited {proc.returncode}, want 2 ({proc.stderr})"
    )


def test_job_submit_by_unsigned_agent_is_refused(api, experiment, run_id):
    # -- hl job submit: real API failures surface cleanly (agent not signed up) --
    agent = make_agent(api, run_id, "cli-nosignup-owner")
    pe_id = experiment("cli-full-flow-nosignup", [agent], budget=PE_BUDGET)
    hyp_id = api.register_hypothesis(pe_id, agent, "")["id"]
    body = api.submission_body_for_id(
        f"job-cli-nosignup-{run_id}", pe_id, agent, hours=JOB_HOURS
    )
    body["metadata"]["hypothesis_id"] = hyp_id
    no_such_agent = f"no-such-agent-{run_id}"
    proc = api.hl_expect("job", "submit", "--agent", no_such_agent, "-", input=yaml.safe_dump(body, sort_keys=False))
    assert proc.returncode != 0, "hl job submit: expected a non-zero exit for an unsigned-up agent"
    assert "not_signed_up" in proc.stderr.lower() or "http 4" in proc.stderr.lower(), (
        f"hl job submit: expected a refusal naming not_signed_up/HTTP 4xx, got: {proc.stderr}"
    )


def test_watch_nonexistent_experiment_is_refused_without_retry(api, run_id):
    # -- hl watch --experiment: nonexistent experiment is refused (exit 2, not retried) --
    proc = api.hl_expect("watch", "--experiment", f"no-such-job-{run_id}", "--timeout", "5", timeout=15)
    assert proc.returncode == 2, (
        f"hl watch --experiment: expected exit 2 + refusal for a nonexistent experiment, got exit={proc.returncode}: {proc.stderr}"
    )
    assert "no such experiment" in proc.stderr.lower(), (
        f"hl watch --experiment: expected 'no such experiment' in stderr, got: {proc.stderr}"
    )


def test_usage_errors_exit_2(api):
    # -- hl usage errors exit 2 --
    register_bad = api.hl_expect("register")
    assert register_bad.returncode == 2, (
        f"hl register with no --id exited {register_bad.returncode}, want 2"
    )
    watch_bad = api.hl_expect("watch")
    assert watch_bad.returncode == 2, (
        f"hl watch with no target exited {watch_bad.returncode}, want 2"
    )
