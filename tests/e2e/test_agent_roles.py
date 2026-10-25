"""Signup roles, end to end. A signup is executor (runs jobs, ranked,
cut-eligible), researcher (steers the work through the hypothesis graph and runs no job), baseline
(the declared control) or reviewer (re-checks other agents' claims); only executor is ranked or
cut. The team of a platform experiment is its agents list, whose entries' agent types give their
roles. Covers the paths that differ from the default flow:
  - GET /agent-roles lists every role with what its agents do.
  - only executors count toward max_agents, in the agents list and at signup.
  - a researcher submits no job, holds no quota, pushes nothing and still reads the repository.
  - a researcher runs on the coordinator's model unless it names one.
  - an unrecognized role is refused at signup with reason=unknown_role, never defaulted.
  - baseline/reviewer signups do not consume max_agents, the field being measured.
  - the baseline posts the best ranking-metric value and is still absent from standings; rank 1
    is the best *executor*, and the baseline's own metrics stay readable.
  - the baseline's job is billed exactly like an executor's; role changes ranking, never
    accounting.
  - a reviewer comments on another agent's hypothesis without ever entering standings.
  - at a real stage boundary, a baseline deliberately holding the worst value in the run is
    never cut, and every cut agent comes from the executor field.

The ranking arithmetic itself is pinned by controller/stages_rank_test.go and
service/platformexperiments/roles_test.go; what only a live run can show is that the role recorded
at signup actually reaches the ranker, the cut and the quota ledger.

API-only, parallel-safe (own platform experiments, own agents).

The job-submitting tests need quota_tier="guaranteed" at signup (an agent-kind participant
otherwise defaults to burst_only, see the other exclusive-marker ports' convention for the same
fix). The stage-cut test reaches its boundary the way support/stages.py describes.
"""
from __future__ import annotations

import uuid

import pytest
import requests

from conftest import make_agent
from support.api import API, ENDED, branch
from support.stages import BUDGET, REPORT_INTERVAL_SECONDS, STAGES, cross_first_boundary_ranked
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.01
RUN_SECONDS = 20
HIGH_ANCHOR = "0.90"
LOW_ANCHOR = "0.10"

CUT_ANCHORS = [0.30, 0.40, 0.50, 0.60, 0.70]


def _env(seconds: int, baseline: str | None = None) -> dict:
    env = {"RUN_SECONDS": str(seconds)}
    if baseline is not None:
        env["HYPOTHESISLOOP_BASELINE"] = baseline
    return env


def _wait_completed(api, job, deadline):
    final = eventually(
        f"{job} to complete",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] == "COMPLETED",
        reject=lambda e: e["status"] in ENDED,
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"{job} ended as {final['status']}"
    return final


def test_unknown_role_refused_and_writes_no_signup(api, pe, run_id):
    pe_id = pe("agent-roles-probe", 1.0, 5)
    stranger = make_agent(api, run_id, "roles-stranger")

    r = api.signup(pe_id, stranger, "referee")
    assert r.status_code == 422, "a typo'd role must be refused, not defaulted to executor"
    assert r.json().get("reason") == "unknown_role"

    code, _ = api.signup_role(pe_id, stranger)
    assert code == 404, "the refused signup left a row behind"


def test_baseline_and_reviewer_do_not_count_against_max_agents(api, run_id, experiment):
    executors = [make_agent(api, run_id, f"roles-cap-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-cap-baseline")
    reviewer = make_agent(api, run_id, "roles-cap-reviewer")

    roster = [(a, "executor") for a in executors] + [(baseline, "baseline"), (reviewer, "reviewer")]
    # max_agents=5 sizes the field being ranked, so it counts executors only: if baseline or
    # reviewer counted against it, one of these two signups would be refused max_agents_reached.
    pe_id = experiment("agent-roles-cap", roster, budget=10.0, max_agents=5, report_interval_seconds=5)

    for agent, want_role in [(baseline, "baseline"), (reviewer, "reviewer"), (executors[0], "executor")]:
        code, got_role = api.signup_role(pe_id, agent)
        assert code == 200 and got_role == want_role, (
            f"{agent} reads back as {code}/{got_role}, want 200/{want_role}"
        )


def test_baseline_and_reviewer_excluded_from_standings_billing_and_gate(api, run_id, experiment, deadline):
    executors = [make_agent(api, run_id, f"roles-rank-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-rank-baseline")
    reviewer = make_agent(api, run_id, "roles-rank-reviewer")
    # quota_tier="guaranteed" explicit: an agent-kind participant's default tier is burst_only
    # (see conftest.py/other exclusive-marker ports for this convention), and every job below
    # submits at the guaranteed tier.
    roster = [(a, "executor", "guaranteed") for a in executors] + [
        (baseline, "baseline", "guaranteed"), (reviewer, "reviewer", "guaranteed"),
    ]
    pe_id = experiment("agent-roles-rank", roster, budget=10.0, max_agents=5, report_interval_seconds=5)

    # A reviewer comments on another agent's hypothesis -- its own job is not required to exist
    # yet for this to be exercised.
    reviewed_hyp = api.register_hypothesis(pe_id, executors[0], f"reviewed claim from {executors[0]}")
    r = api.post_hypothesis_comment(
        reviewed_hyp["id"], "reviewer re-checked the reported metric against the job's code_ref", agent_id=reviewer
    )
    assert r.status_code == 201, "a reviewer must be able to comment on another agent's hypothesis"
    hyp = api.hypothesis(reviewed_hyp["id"])
    authors = {c["agent_id"] for c in hyp.get("comments") or []}
    assert reviewer in authors, "the reviewer's comment is not readable on the hypothesis"

    # The baseline is the objective's own measurement: its agent registers no hypothesis, and a
    # job of it that names one is refused.
    r = api.post_hypothesis(pe_id, "a claim of the baseline agent's", agent_id=baseline)
    assert (r.status_code, r.json()["reason"]) == (403, "baseline_registers_no_hypothesis"), f"{r.status_code} {r.text}"
    naming_one = api.submission_body_for_id(f"job-{uuid.uuid4().hex[:8]}", pe_id, executors[0])
    naming_one["metadata"] |= {"agent_id": baseline, "branch": branch(pe_id, baseline, "")}
    r = api.post_experiment_body(naming_one)
    assert (r.status_code, r.json()["reason"]) == (422, "baseline_tests_no_hypothesis"), f"{r.status_code} {r.text}"

    # The baseline and reviewer run HIGH, every executor runs LOW: rank 1 can then only be a
    # executor if role genuinely filters the standings.
    baseline_job = api.submit_job(pe_id, baseline, hours=JOB_HOURS, hyp_id="", job_overrides={"env": _env(RUN_SECONDS, HIGH_ANCHOR)})
    on_objective = api.experiment(baseline_job)
    assert on_objective["objective"] is True and "hypothesis_id" not in on_objective, on_objective
    reviewer_job = api.submit_job(pe_id, reviewer, hours=JOB_HOURS, job_overrides={"env": _env(RUN_SECONDS, HIGH_ANCHOR)})
    comp_jobs = [
        api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": _env(RUN_SECONDS, LOW_ANCHOR)})
        for a in executors
    ]

    for job in [baseline_job, reviewer_job, *comp_jobs]:
        _wait_completed(api, job, deadline)
    for job in comp_jobs:
        api.file_finding(job)

    api.file_finding(baseline_job)

    # A job is ranked by the last value it reported.
    baseline_best = api.metric_last(baseline_job, "val_accuracy")
    assert baseline_best is not None, "the baseline's val_accuracy must be recorded and readable"

    best_comp, best_comp_value = None, None
    for a, job in zip(executors, comp_jobs):
        v = api.metric_last(job, "val_accuracy")
        if v is None:
            continue
        if best_comp_value is None or v > best_comp_value:
            best_comp, best_comp_value = a, v
    assert best_comp is not None, "no executor reported val_accuracy"
    assert baseline_best > best_comp_value, (
        f"the baseline ({baseline_best}) did not outscore the best executor ({best_comp_value}): "
        "the standings assertions below would pass for the wrong reason"
    )

    results = api.results(pe_id)
    ranking = next(m for m in results["metrics"] if m["metric"] == "val_accuracy")
    ranked_agents = [s["agent_id"] for s in ranking["standings"]]

    assert baseline not in ranked_agents, "the baseline is ranked despite holding the best value in the run"
    assert reviewer not in ranked_agents, "the reviewer is ranked despite reporting a top-of-field value"
    assert sorted(ranked_agents) == sorted(executors), (
        f"standings are {sorted(ranked_agents)}, want exactly the {len(executors)} executors"
    )
    assert ranked_agents[0] == best_comp, (
        f"rank 1 is {ranked_agents[0]!r}, want the best executor {best_comp!r}, and the higher-scoring baseline is not ranked"
    )

    # Billing: identical jobs, so if role touched accounting anywhere these numbers would differ.
    eventually(
        "the baseline's and an executor's quota to settle",
        lambda: (api.experiment(baseline_job).get("quota_settled_at"), api.experiment(comp_jobs[0]).get("quota_settled_at")),
        accept=lambda v: bool(v[0]) and bool(v[1]),
        deadline=deadline,
    )
    # quota_settled_at flips in Postgres once the metrics write to GreptimeDB succeeds, but
    # used_guaranteed_acch is served from that same eventually-consistent store, so it can still
    # lag a beat behind the flag under load -- wait on the actual fields being asserted on.
    baseline_used, comp_used = eventually(
        "the baseline's and an executor's used_guaranteed_acch to reflect settlement",
        lambda: (
            api.quota_field(pe_id, baseline, "used_guaranteed_acch"),
            api.quota_field(pe_id, executors[0], "used_guaranteed_acch"),
        ),
        accept=lambda v: v[0] > 0 and v[1] > 0,
        deadline=deadline,
    )
    assert baseline_used > 0, "the baseline's job was not billed against its guaranteed quota -- a role that runs for free is a hole in the budget"
    assert abs(baseline_used - comp_used) <= 0.01, (
        f"baseline billed {baseline_used} AccH vs executor {comp_used} for identical jobs: "
        "role changed accounting, which it must never do"
    )


@pytest.mark.timeout(400)
def test_baseline_survives_stage_cut_drawn_only_from_executors(api, run_id, experiment, deadline):
    executors = [make_agent(api, run_id, f"roles-cut-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-cut-baseline")
    roster = [(a, "executor", "guaranteed") for a in executors] + [(baseline, "baseline", "guaranteed")]
    # Five executors is exactly minSurvivorsForCut: the boundary cuts floor(50% x 5) = 2 and
    # leaves 3, clearing minSurvivorsAfterCut (2). If the baseline counted as a survivor the field
    # would be 6 and the cut 3 -- and the baseline, holding the worst value in the run, would
    # certainly be one of them.
    pe_id = experiment(
        "agent-roles-cut", roster, budget=BUDGET, max_agents=5,
        report_interval_seconds=REPORT_INTERVAL_SECONDS, stages=STAGES,
    )

    # The baseline's completed job runs on the LOW anchor and every executor's on a distinct
    # HIGH-ish anchor, so the baseline holds the single worst value in the run -- the agent a cut
    # would take first if role were ignored.
    anchors = {baseline: LOW_ANCHOR, **{a: str(anchor) for a, anchor in zip(executors, CUT_ANCHORS)}}
    stages, _ = cross_first_boundary_ranked(api, pe_id, [baseline, *executors], deadline, baselines=anchors)

    cut_agents = {c["agent_id"] for c in stages["cut_agents"]}
    active_agents = set(stages["active_agents"])
    cut_n = len(cut_agents)

    assert baseline not in cut_agents, "the baseline was cut despite holding the worst value in the run"
    assert baseline in active_agents, "the baseline is neither cut nor active -- it fell out of the roster"

    if cut_n == 0:
        # Legitimate only if a tie group straddled the line -- but this scenario's anchors are
        # spaced to avoid ties, so no cut at all means the assertions above proved nothing.
        pytest.fail("the boundary cut nobody, and the 5-executor field should have cut floor(50%*5)=2")

    strays = cut_agents - set(executors)
    assert not strays, f"cut agent(s) {strays} are not executors: the cut was drawn from the wrong field"
    assert cut_n == 2, (
        f"cut {cut_n} agents, want exactly floor(50% x 5)=2 -- a larger cut means the baseline was "
        "counted into the field"
    )


def test_repeat_signup_of_a_member_succeeds_once_running_and_full(api, experiment, run_id):
    # An agent restarting signs up again after the run started and filled; a newcomer is refused while the member is at work.
    member = make_agent(api, run_id, "repeat-member")
    pe_id = experiment("repeat-signup", [member], budget=0.05, max_agents=1)

    for _ in range(2):
        r = api.signup(pe_id, member)
        assert r.status_code == 201, f"repeat signup of a member: HTTP {r.status_code} {r.text}"
    api.hl("platform-experiments", "agents", "add", "--platform-experiment", pe_id, "--agent", member)

    r = api.signup(pe_id, make_agent(api, run_id, "repeat-newcomer"))
    assert r.status_code == 409 and r.json()["reason"] == "max_agents_reached", f"newcomer after start: HTTP {r.status_code} {r.text}"


def _create(api: API, name: str, **more) -> requests.Response:
    return api.post("/platform-experiments", {
        "project_id": api.project_id, "name": name, "description": name, "budget_accelerator_hours": 1.0,
        "report_interval_seconds": 10, "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "duration_hours": 24, **more,
    })


def test_the_roles_are_listed_with_what_their_agents_do(api):
    roles = api.get_json("/agent-roles")
    assert [r["role"] for r in roles] == ["executor", "researcher", "baseline", "coordinator", "planner"], roles
    by_role = {r["role"]: r for r in roles}
    assert all(r["name"] and r["description"] for r in roles), roles
    assert (by_role["executor"]["signup_role"], by_role["executor"]["in_team"], by_role["executor"]["org_types"]) == ("executor", True, True)
    researcher = by_role["researcher"]
    assert (researcher["agent_type"], researcher["signup_role"], researcher["in_team"], researcher["org_types"]) == ("researcher", "researcher", True, False), researcher
    types = {t["id"]: t["role"] for t in api.get_json(f"/orgs/{api.org_id}/agent-types")}
    assert types.get("researcher") == "researcher" and types.get("generalist") == "executor", types


def test_only_executors_count_toward_max_agents(api, run_id):
    # The team is one list: two researchers and one executor fit max_agents 1, two executors do not.
    r = _create(api, f"roles-team-{run_id}", max_agents=1, agents=[{"agent_type": "researcher", "count": 2}, {"agent_type": "generalist", "count": 1}])
    assert r.status_code == 201, f"a team of two researchers and one executor under max_agents 1: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    api.record_platform_experiment(pe_id)
    try:
        stored = api.get_json(f"/platform-experiments/{pe_id}")
        assert [(e["agent_type"], e["count"]) for e in stored["agents"]] == [("researcher", 2), ("generalist", 1)], stored["agents"]
        r = _create(api, f"roles-team-over-{run_id}", max_agents=1, agents=[{"agent_type": "generalist", "count": 2}])
        assert (r.status_code, r.json().get("reason")) == (422, "invalid_agents"), f"two executors under max_agents 1: {r.status_code} {r.text}"

        # At signup too: the one place max_agents gives is the executor's, and researchers join beside it.
        executor = api.add_agents(pe_id)[0]
        researchers = api.add_agents(pe_id, 2, role="researcher")
        assert all((a["role"], a["agent_type"]) == ("researcher", "researcher") for a in researchers), researchers
        r = api.post(f"/platform-experiments/{pe_id}/agents", {})
        assert (r.status_code, r.json().get("reason")) == (409, "max_agents_reached"), f"a second executor: {r.status_code} {r.text}"
        assert api.signup_role(pe_id, executor["agent_id"]) == (200, "executor")
    finally:
        api.close_platform_experiment(pe_id)


def test_a_researcher_runs_no_job_holds_no_quota_and_pushes_nothing(api, pe, run_id, tmp_path):
    pe_id = pe("roles-researcher", 1.0, 2)
    executor = api.add_agents(pe_id, quota_tier="guaranteed")[0]
    researcher = api.add_agents(pe_id, role="researcher")[0]
    api.declare_baseline_and_wait_running(pe_id)
    as_researcher = api.as_caller(researcher["token"], api.org_id, api.project_id)

    # Its job is refused before anything of it is read, whatever it names.
    job_id = api._job_id()
    body = api.submission_body_for_id(job_id, pe_id, executor["agent_id"])
    body["metadata"] |= {"agent_id": researcher["agent_id"]}
    r = as_researcher.post_experiment_body(body)
    assert (r.status_code, r.json().get("reason")) == (422, "researcher_submits_no_job"), f"{r.status_code} {r.text}"
    assert api.get(f"/experiments/{job_id}").status_code == 404, "the refused job was stored"

    r = as_researcher.get(f"/platform-experiments/{pe_id}/quotas/{researcher['agent_id']}")
    assert (r.status_code, r.json().get("reason")) == (404, "researcher_has_no_quota"), f"{r.status_code} {r.text}"
    assert researcher["agent_id"] not in {q["agent_id"] for q in api.platform_experiment_quotas(pe_id)}
    assert executor["agent_id"] in {q["agent_id"] for q in api.platform_experiment_quotas(pe_id)}

    # It clones the repository through the git proxy and pushes nothing, not even a branch of its own name.
    read = as_researcher.get_json(f"/platform-experiments/{pe_id}")
    cloned = as_researcher.git(read, tmp_path, "clone", read["repository_url"], "work")
    assert cloned.returncode == 0, cloned.stderr
    work = tmp_path / "work"
    own = f"hl/{pe_id}/researcher-1/e2e-{run_id}"
    assert as_researcher.git(read, work, "checkout", "-b", own).returncode == 0
    (work / "e2e.txt").write_text(f"pushed by {run_id}\n")
    assert as_researcher.git(read, work, "add", "e2e.txt").returncode == 0
    assert as_researcher.git(read, work, "commit", "-m", f"e2e {run_id}").returncode == 0
    denied = as_researcher.git(read, work, "push", "origin", own)
    assert denied.returncode != 0 and "read-only" in denied.stderr, f"a researcher pushed: {denied.stderr}"
    assert as_researcher.git(read, work, "fetch", "origin").returncode == 0, "the researcher lost its read"


def test_a_researcher_runs_on_the_coordinators_model_unless_it_names_one(api, pe):
    pe_id = pe("roles-researcher-model", 1.0, 1)
    researcher = api.add_agents(pe_id, role="researcher")[0]
    models = api.get_json("/models")
    type_model = api.get_json(f"/orgs/{api.org_id}/agent-types/researcher")["current"]["settings"].get("model") or ""
    got = api.as_caller(researcher["token"]).get_json(f"/platform-experiments/{pe_id}/agents/{researcher['agent_id']}/instructions")
    assert got["role"] == "researcher" and got["agent_type"] == "researcher", got
    assert got["settings"]["model"] == (type_model or models["coordinator_model"]), (
        f"a researcher naming no model runs on {got['settings']['model']}, want its type's or the coordinator default {models['coordinator_model']}"
    )
    executor = api.add_agents(pe_id)[0]
    got = api.as_caller(executor["token"]).get_json(f"/platform-experiments/{pe_id}/agents/{executor['agent_id']}/instructions")
    assert got["role"] == "executor", got


@pytest.mark.real_providers
@pytest.mark.exclusive
def test_a_team_spec_starts_its_researcher_beside_its_executor_on_the_coordinators_model(api: API, sandbox_repository: str, run_id: str):
    """Each agent of the team is rented at the sandbox provider, so this runs only with
    make e2e RENT=1."""
    coordinator_model = api.get_json("/models")["coordinator_model"]
    r = _create(api, f"roles-team-sandboxed-{run_id}", max_agents=1, coordinator_in_agentsandbox=True, coordinator_model=coordinator_model,
                repository=sandbox_repository, branch="main",
                agents=[{"agent_type": "researcher", "count": 1}, {"agent_type": "generalist", "count": 1}])
    assert r.status_code == 201, f"create: {r.status_code} {r.text}"
    pe_id = r.json()["id"]
    api.record_platform_experiment(pe_id)
    try:
        team = [a for a in api.agents(pe_id) if a["role"] != "baseline"]
        assert sorted((a["role"], a["agent_type"]) for a in team) == [("executor", "generalist"), ("researcher", "researcher")], team
        assert all(a["in_agentsandbox"] for a in team), team
        researcher = next(a for a in team if a["role"] == "researcher")
        got = api.get_json(f"/platform-experiments/{pe_id}/agents/{researcher['agent_id']}/instructions")
        assert got["settings"]["model"] == coordinator_model, f"the researcher runs on {got['settings']['model']}, want the coordinator's {coordinator_model}"
        counted = api.get_json(f"/platform-experiments/{pe_id}")["agents_at_work"]
        assert [e["agent_type"] for e in counted["pool"]] == ["researcher", "generalist"], counted
    finally:
        api.close_platform_experiment(pe_id)
