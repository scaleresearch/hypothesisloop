"""One experiment id, one experiment -- even when the same id arrives N times at the same instant.
Ported 1:1 from tests/scenarios/duplicate-submission-race.sh.

Submission decides inside its admission transaction whether an id is new, already QUEUED, or
taken. Two outcomes are both correct and must be told apart:
  - the SAME agent re-submitting its own queued job is an idempotent retry: every POST returns 2xx
    and exactly one row exists.
  - a DIFFERENT agent naming that id is a collision, refused 409 without touching the owner's job.

What must never happen either way is a 5xx: that was the original defect, where concurrent
submissions all passed a pre-transaction check and the loser hit the primary key.

A submit that states no id is made safe to send again by its submission_key, unique per agent in
a platform experiment: the platform issues the id once and answers every repeat with that job as
it is then. hl derives the key from the request.

API-only, no accelerator required (the jobs need never be admitted), parallel-safe: own agent(s)
and platform experiment, own run-scoped id.
"""
from __future__ import annotations

import json
import re
import uuid
from concurrent.futures import ThreadPoolExecutor

import pytest
import yaml

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

N = 4
JOB_ID = re.compile(r"job-[0-9a-f]{32}")


def test_concurrent_identical_submissions_are_idempotent_and_second_agent_is_refused(api, experiment, run_id):
    agent = make_agent(api, run_id, "dup-owner")
    other_agent = make_agent(api, run_id, "dup-other")
    pe_id = experiment("dup-race", [agent, other_agent], budget=5.0)

    job_id = f"job-dup-{uuid.uuid4().hex[:8]}-{run_id}"
    # Built once and reused: the race under test is N POSTs of one identical request, not N racing
    # body constructions (each of which would register its own hypothesis first).
    body = api.submission_body_for_id(job_id, pe_id, agent)

    with ThreadPoolExecutor(max_workers=N) as pool:
        responses = list(pool.map(lambda _: api.post_experiment_body(body), range(N)))
    codes = [r.status_code for r in responses]

    accepted = sum(1 for c in codes if 200 <= c < 300)
    # No 5xx, and no 409 either: every one of these is the owning agent retrying its own submission.
    assert accepted == N, f"only {accepted} of {N} concurrent retries succeeded, codes={codes}"

    exp = api.experiment(job_id)
    assert exp["status"] in ("QUEUED", "SUBMITTED", "RUNNING"), exp
    # The id is the primary key, so a second row is impossible by construction; this checks the
    # winner is a complete row and not a half-written one from a rolled-back transaction.
    assert exp.get("estimated_cost_acch") not in (None, ""), exp

    # -- a second agent cannot claim, or disturb, a job id it does not own --
    # The id check is scoped to the submitter, so a different agent naming the same id is a
    # collision, not a legal re-submission of work it does not own.
    body_b = api.submission_body_for_id(job_id, pe_id, other_agent)
    resp_b = api.post_experiment_body(body_b)
    assert resp_b.status_code == 409 and resp_b.json()["reason"] == "duplicate", resp_b.text

    assert api.experiment(job_id)["agent_id"] == agent


def _cancelled(api, job_id: str, deadline) -> None:
    api.cancel_job(job_id)
    eventually(f"{job_id} to end as CANCELLED", lambda: api.experiment(job_id)["status"],
               accept=lambda status: status == "CANCELLED", deadline=deadline)


def test_a_submission_key_makes_one_job_of_a_submit_sent_again(api, experiment, run_id, deadline):
    agent = make_agent(api, run_id, "key-owner")
    other_agent = make_agent(api, run_id, "key-other")
    pe_id = experiment("dup-key", [agent, other_agent], budget=5.0)

    def listed(of: str) -> set[str]:
        return {j["id"] for j in api.get_json(f"/experiments?agent_id={of}&platform_experiment_id={pe_id}&limit=50")}

    # No id is stated, so the platform issues one, and the key is what makes the N submits one job.
    key = f"key-{uuid.uuid4().hex}"
    body = {k: v for k, v in api.submission_body_for_id("unused", pe_id, agent).items() if k != "id"} | {"submission_key": key}
    with ThreadPoolExecutor(max_workers=N) as pool:
        responses = list(pool.map(lambda _: api.post_experiment_body(body), range(N)))
    assert [r.status_code for r in responses] == [202] * N, [(r.status_code, r.text) for r in responses]
    (job_id,) = {r.json()["id"] for r in responses}
    assert JOB_ID.fullmatch(job_id), f"the platform issued {job_id!r}"
    assert listed(agent) == {job_id}, "submits with one key created more than one job"
    assert "submission_key" not in api.experiment(job_id), "the key is answered on the job"

    # Another key is another job, and the same key of another agent is that agent's own.
    second = api.post_experiment_body(body | {"submission_key": f"{key}.2"})
    assert second.status_code == 202 and second.json()["id"] != job_id, second.text
    body_b = {k: v for k, v in api.submission_body_for_id("unused", pe_id, other_agent).items() if k != "id"} | {"submission_key": key}
    theirs = api.post_experiment_body(body_b)
    assert theirs.status_code == 202 and theirs.json()["agent_id"] == other_agent, theirs.text
    assert theirs.json()["id"] not in (job_id, second.json()["id"])

    # The job is answered as it is then, whatever became of it, and nothing more is created.
    _cancelled(api, job_id, deadline)
    again = api.post_experiment_body(body)
    assert again.status_code == 202 and (again.json()["id"], again.json()["status"]) == (job_id, "CANCELLED"), again.text
    assert listed(agent) == {job_id, second.json()["id"]}

    # hl sends the key itself: the same file submitted twice is one job, and --repeat 2 another.
    spec = yaml.safe_dump({k: v for k, v in body.items() if k != "submission_key"}, sort_keys=False)
    submit = ("job", "submit", "--agent", agent, "-")
    first = json.loads(api.hl(*submit, input=spec))
    repeated = api.hl_expect(*submit, input=spec)
    assert repeated.returncode == 0 and json.loads(repeated.stdout)["id"] == first["id"], repeated.stderr
    assert "an identical submit already created" in repeated.stderr or json.loads(repeated.stdout)["status"] == "QUEUED", repeated.stderr
    another = json.loads(api.hl("job", "submit", "--agent", agent, "--repeat", "2", "-", input=spec))
    assert JOB_ID.fullmatch(first["id"]) and another["id"] != first["id"], (first, another)
    # Of a job that ended, an identical submit says so and fails: nothing was submitted.
    _cancelled(api, first["id"], deadline)
    ended = api.hl_expect(*submit, input=spec)
    assert ended.returncode == 1 and first["id"] in ended.stderr and "--repeat 2" in ended.stderr, (ended.returncode, ended.stderr)
    assert listed(agent) == {job_id, second.json()["id"], first["id"], another["id"]}
