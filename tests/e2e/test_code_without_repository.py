"""An agent of a platform experiment that names no repository keeps its code with the platform.

It commits in a repository with no origin, `hl code push` stores the commit as a git bundle,
`hl job submit` pins it as the job's code_ref, and `hl-clone` checks that commit out again, on
another agent's machine and in a job. Nothing here needs GitHub.
"""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest
import yaml

from support import admin
from support.api import API, JobRequest

REPO_ROOT = Path(__file__).resolve().parents[2]
HL_CLONE = REPO_ROOT / "agents" / "coordinator" / "experiments" / "hl-clone"
GIT_ENV = {
    "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_TERMINAL_PROMPT": "0",
    "GIT_AUTHOR_NAME": "hl e2e", "GIT_AUTHOR_EMAIL": "e2e@hypothesisloop.local",
    "GIT_COMMITTER_NAME": "hl e2e", "GIT_COMMITTER_EMAIL": "e2e@hypothesisloop.local",
}


def _git(cwd: Path, *args: str) -> str:
    done = subprocess.run(["git", *args], cwd=cwd, env=os.environ | GIT_ENV, capture_output=True, text=True, timeout=60)
    assert done.returncode == 0, f"git {args}: {done.stderr}"
    return done.stdout.strip()


def _hl(agent: API, cwd: Path, *args: str) -> subprocess.CompletedProcess:
    env = os.environ | GIT_ENV | agent.hl_env() | {"PATH": f"{agent.hl_bin.parent}:{os.environ['PATH']}"}
    return subprocess.run([str(agent.hl_bin), *args], cwd=cwd, env=env, capture_output=True, text=True, timeout=120)


def _hl_clone(agent: API, ref: str, into: Path, **env: str) -> subprocess.CompletedProcess:
    """hl-clone as an image has it: beside an hl on PATH."""
    base = os.environ | GIT_ENV | {"PATH": f"{agent.hl_bin.parent}:{os.environ['PATH']}"}
    return subprocess.run(["sh", str(HL_CLONE), ref, str(into)], env=base | env, capture_output=True, text=True, timeout=120)


@pytest.mark.parallel
def test_code_pushed_to_the_platform_is_what_a_job_carries_and_what_another_agent_checks_out(api: API, run_id: str, tmp_path: Path):
    pe_id = api.create_platform_experiment(f"no-repository-{run_id}", 1.0, 3)
    other_pe = api.create_platform_experiment(f"no-repository-other-{run_id}", 1.0, 3)
    try:
        assert "repository_url" not in api.get_json(f"/platform-experiments/{pe_id}")
        first, second = api.add_agents(pe_id)[0], api.add_agents(pe_id)[0]
        outsider = api.add_agents(other_pe)[0]
        writer = api.as_caller(first["token"], api.org_id, api.project_id)
        reader = api.as_caller(second["token"], api.org_id, api.project_id)
        stranger = api.as_caller(outsider["token"], api.org_id, api.project_id)
        api.start_platform_experiment(pe_id)
        agent_env = ["--agent", first["agent_id"], "--platform-experiment", pe_id]

        work = tmp_path / "work"
        work.mkdir()
        _git(work, "init", "-q", "-b", "main")
        (work / "train.py").write_text("print('v1')\n")
        hypothesis = writer.register_hypothesis(pe_id, first["agent_id"], f"code without a repository {run_id}")["id"]
        spec = yaml.safe_load(JobRequest(id="", agent_id=first["agent_id"], platform_experiment_id=pe_id, hypothesis_id=hypothesis, hours=0.02,
                                         tier="guaranteed", job_overrides={}, metadata_overrides={"code_ref": ""}).to_yaml())
        (work / "job.yaml").write_text(yaml.safe_dump(spec, sort_keys=False))
        _git(work, "add", ".")
        _git(work, "commit", "-q", "-m", "v1")
        sha = _git(work, "rev-parse", "HEAD")

        # A commit the platform does not hold is not submitted.
        refused = _hl(writer, work, "job", "submit", "job.yaml", *agent_env)
        assert refused.returncode != 0 and "hl code push" in refused.stderr, refused.stderr

        pushed = _hl(writer, work, "code", "push", *agent_env)
        assert pushed.returncode == 0, pushed.stderr
        ref = pushed.stdout.strip()
        assert ref == f"{api.base_url}/platform-experiments/{pe_id}/agents/{first['agent_id']}/code@{sha}", ref

        submitted = _hl(writer, work, "job", "submit", "job.yaml", *agent_env)
        assert submitted.returncode == 0, submitted.stderr
        job_id = json.loads(submitted.stdout)["id"]
        assert writer.experiment(job_id)["metadata"]["code_ref"] == ref

        # Another agent of the experiment reads it, with a code_ref written with any address:
        # the bundle comes from the API its own machine was given.
        elsewhere = ref.replace(api.base_url, "https://api.as-someone-else-reached-it.example")
        out = tmp_path / "read"
        cloned = _hl_clone(reader, elsewhere, out, API_URL=api.base_url, HL_TOKEN=reader.token)
        assert cloned.returncode == 0 and cloned.stdout.strip() == str(out), cloned.stderr
        assert _git(out, "rev-parse", "HEAD") == sha and (out / "train.py").read_text() == "print('v1')\n"

        # A job has the same under its own names, and reads with its own token.
        as_job = tmp_path / "job"
        cloned = _hl_clone(writer, ref, as_job, HYPOTHESISLOOP_API_URL=api.base_url, HYPOTHESISLOOP_TOKEN=admin.token(f"job:{job_id}"))
        assert cloned.returncode == 0 and _git(as_job, "rev-parse", "HEAD") == sha, cloned.stderr

        # Nobody outside the experiment reads it, and nobody but its agent writes it.
        path = f"/platform-experiments/{pe_id}/agents/{first['agent_id']}/code/{sha}"
        assert stranger.get(path).status_code == 404
        bundle = reader.get(path)
        assert bundle.status_code == 200 and bundle.content.startswith(b"# v")
        assert reader.session.put(reader._url(path), data=bundle.content, headers={"Content-Type": "application/octet-stream"}).status_code == 403

        # A rerun of the same commit carries the same reference, with nothing pushed again.
        again = _hl(writer, work, "job", "submit", "job.yaml", *agent_env)
        assert again.returncode == 0 and writer.experiment(json.loads(again.stdout)["id"])["metadata"]["code_ref"] == ref, again.stderr

        # Once the experiment is closed nothing more is stored, and what was stored still reads.
        (work / "train.py").write_text("print('v2')\n")
        _git(work, "commit", "-q", "-am", "v2")
        api.close_platform_experiment(pe_id)
        late = _hl(writer, work, "code", "push", *agent_env)
        assert late.returncode != 0 and "closed" in late.stderr, late.stderr
        assert api.get(path).status_code == 200
    finally:
        api.close_platform_experiment(pe_id)
        api.close_platform_experiment(other_pe)
