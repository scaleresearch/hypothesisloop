"""Signup roles, end to end. Ported 1:1 from tests/scenarios/agent-roles.sh: a signup is
competitor (ranked, cut-eligible), baseline (the declared control) or reviewer (re-checks other
agents' claims); only competitor is ranked or cut. Covers the paths that differ from the default
flow:
  - an unrecognized role is refused at signup with reason=unknown_role, never defaulted.
  - baseline/reviewer signups do not consume max_agents, the field being measured.
  - the baseline posts the best ranking-metric value and is still absent from standings; rank 1
    is the best *competitor*, and the baseline's own metrics stay readable.
  - the baseline's job is billed exactly like a competitor's -- role changes ranking, never
    accounting -- and the summary gate applies to it the same way.
  - a reviewer comments on another agent's hypothesis without ever entering standings.
  - at a real stage boundary, a baseline deliberately holding the worst value in the run is
    never cut, and every cut agent comes from the competitor field.

The ranking arithmetic itself is pinned by controller/stages_rank_test.go and
quota/platform_experiments_roles_test.go; what only a live run can show is that the role recorded
at signup actually reaches the ranker, the cut, the quota ledger and the summary gate.

API-only, parallel-safe (own platform experiments, own agents).

PROVEN against the live stack: 3x solo green. The job-submitting tests needed
quota_tier="guaranteed" at signup (an agent-kind participant otherwise defaults to burst_only, see
the other exclusive-marker ports' convention for the same fix) and the stage-cut test needed
pinning to H100 (acch_rate 1.0, matching tests/scenarios/agent-roles.sh's own ACCELERATOR_TYPE) --
CUT_BUDGET/CUT_STAGES are sized so the first boundary lands right at 20% of budget from the 6
jobs' real settled run time, which a lower-rate default flavor cannot reach.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel

JOB_HOURS = 0.01
RUN_SECONDS = 20
HIGH_ANCHOR = "0.90"
LOW_ANCHOR = "0.10"

CUT_STAGES = [
    {"length_pct": 20, "evict_pct": 50},
    {"length_pct": 30, "evict_pct": 25},
    {"length_pct": 50, "evict_pct": 0},
]
CUT_JOB_HOURS = 0.0028
CUT_RUN_SECONDS = 30
CUT_ANCHORS = [0.30, 0.40, 0.50, 0.60, 0.70]
CUT_BUDGET = 0.10


def _env(seconds: int, baseline: str | None = None) -> dict:
    env = {"HYPOTHESISLOOP_DURATION_SECONDS": str(seconds)}
    if baseline is not None:
        env["HYPOTHESISLOOP_BASELINE"] = baseline
    return env


def _wait_completed(api, job, deadline):
    final = eventually(
        f"{job} to complete",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] == "COMPLETED",
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"{job} ended as {final['status']}"
    return final


def test_unknown_role_refused_and_writes_no_signup(api, pe, run_id):
    pe_id = pe("agent-roles-probe", 1.0, 5)
    stranger = make_agent(api, run_id, "roles-stranger")

    r = api.signup(pe_id, stranger, "referee")
    assert r.status_code == 400, "a typo'd role must be refused, not defaulted to competitor"
    assert r.json().get("reason") == "unknown_role"

    code, _ = api.signup_role(pe_id, stranger)
    assert code == 404, "the refused signup left a row behind"


def test_baseline_and_reviewer_do_not_count_against_max_agents(api, run_id, experiment):
    competitors = [make_agent(api, run_id, f"roles-cap-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-cap-baseline")
    reviewer = make_agent(api, run_id, "roles-cap-reviewer")

    roster = [(a, "competitor") for a in competitors] + [(baseline, "baseline"), (reviewer, "reviewer")]
    # max_agents=5 sizes the field being ranked, so it counts competitors only: if baseline or
    # reviewer counted against it, one of these two signups would be refused max_agents_reached.
    pe_id = experiment("agent-roles-cap", roster, budget=10.0, max_agents=5, report_interval_seconds=5)

    for agent, want_role in [(baseline, "baseline"), (reviewer, "reviewer"), (competitors[0], "competitor")]:
        code, got_role = api.signup_role(pe_id, agent)
        assert code == 200 and got_role == want_role, (
            f"{agent} reads back as {code}/{got_role}, want 200/{want_role}"
        )


def test_baseline_and_reviewer_excluded_from_standings_billing_and_gate(api, run_id, experiment, deadline):
    competitors = [make_agent(api, run_id, f"roles-rank-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-rank-baseline")
    reviewer = make_agent(api, run_id, "roles-rank-reviewer")
    # quota_tier="guaranteed" explicit: an agent-kind participant's default tier is burst_only
    # (see conftest.py/other exclusive-marker ports for this convention), and every job below
    # submits at the guaranteed tier.
    roster = [(a, "competitor", "guaranteed") for a in competitors] + [
        (baseline, "baseline", "guaranteed"), (reviewer, "reviewer", "guaranteed"),
    ]
    pe_id = experiment("agent-roles-rank", roster, budget=10.0, max_agents=5, report_interval_seconds=5)

    # A reviewer comments on another agent's hypothesis -- its own job is not required to exist
    # yet for this to be exercised.
    reviewed_hyp = api.register_hypothesis(pe_id, competitors[0], f"reviewed claim from {competitors[0]}")
    r = api.post_hypothesis_comment(
        reviewed_hyp["id"], "reviewer re-checked the reported metric against the job's code_ref", agent_id=reviewer
    )
    assert r.status_code == 201, "a reviewer must be able to comment on another agent's hypothesis"
    hyp = api.hypothesis(reviewed_hyp["id"])
    authors = {c.get("agent_id") or c.get("author") for c in hyp.get("comments") or []}
    assert reviewer in authors, "the reviewer's comment is not readable on the hypothesis"

    # The baseline and reviewer run HIGH, every competitor runs LOW: rank 1 can then only be a
    # competitor if role genuinely filters the standings.
    baseline_job = api.submit_job(pe_id, baseline, hours=JOB_HOURS, job_overrides={"env": _env(RUN_SECONDS, HIGH_ANCHOR)})
    reviewer_job = api.submit_job(pe_id, reviewer, hours=JOB_HOURS, job_overrides={"env": _env(RUN_SECONDS, HIGH_ANCHOR)})
    comp_jobs = [
        api.submit_job(pe_id, a, hours=JOB_HOURS, job_overrides={"env": _env(RUN_SECONDS, LOW_ANCHOR)})
        for a in competitors
    ]

    for job in [baseline_job, reviewer_job, *comp_jobs]:
        _wait_completed(api, job, deadline)
    for job in comp_jobs:
        api.file_finding(job)

    # The summary gate applies to the baseline exactly like a competitor: its next submission is
    # refused until it files the finding for its completed job.
    blocked_code, _ = api.submit_job_expect(pe_id, baseline, hours=JOB_HOURS)
    assert blocked_code == 403, (
        f"the baseline's submission answered {blocked_code}, want 403 -- the reference run needs "
        "a finding as much as a treatment does"
    )
    api.file_finding(baseline_job)
    allowed_code, allowed_job = api.submit_job_expect(pe_id, baseline, hours=JOB_HOURS)
    assert allowed_code < 400, (
        f"the baseline's submission still answered {allowed_code} after filing its finding -- the gate never opens"
    )
    api.cancel_job(allowed_job)

    baseline_best = api.metric_max(baseline_job, "val_accuracy")
    reviewer_best = api.metric_max(reviewer_job, "val_accuracy")
    assert baseline_best is not None, "the baseline's val_accuracy must be recorded and readable"

    best_comp, best_comp_value = None, None
    for a, job in zip(competitors, comp_jobs):
        v = api.metric_max(job, "val_accuracy")
        if v is None:
            continue
        if best_comp_value is None or v > best_comp_value:
            best_comp, best_comp_value = a, v
    assert best_comp is not None, "no competitor reported val_accuracy"
    assert baseline_best > best_comp_value, (
        f"the baseline ({baseline_best}) did not outscore the best competitor ({best_comp_value}) -- "
        "the standings assertions below would pass for the wrong reason"
    )

    results = api.results(pe_id)
    ranking = next(m for m in results["metrics"] if m["metric"] == "val_accuracy")
    ranked_agents = [s["agent_id"] for s in ranking["standings"]]

    assert baseline not in ranked_agents, "the baseline is ranked despite holding the best value in the run"
    assert reviewer not in ranked_agents, "the reviewer is ranked despite reporting a top-of-field value"
    assert sorted(ranked_agents) == sorted(competitors), (
        f"standings are {sorted(ranked_agents)}, want exactly the {len(competitors)} competitors"
    )
    assert ranked_agents[0] == best_comp, (
        f"rank 1 is {ranked_agents[0]!r}, want the best competitor {best_comp!r} -- not the higher-scoring baseline"
    )

    # Billing: identical jobs, so if role touched accounting anywhere these numbers would differ.
    eventually(
        "the baseline's and a competitor's quota to settle",
        lambda: (api.experiment(baseline_job).get("quota_settled_at"), api.experiment(comp_jobs[0]).get("quota_settled_at")),
        accept=lambda v: bool(v[0]) and bool(v[1]),
        deadline=deadline,
    )
    # quota_settled_at flips in Postgres once the metrics write to GreptimeDB succeeds, but
    # used_guaranteed_acch is served from that same eventually-consistent store, so it can still
    # lag a beat behind the flag under load -- wait on the actual fields being asserted on.
    baseline_used, comp_used = eventually(
        "the baseline's and a competitor's used_guaranteed_acch to reflect settlement",
        lambda: (
            api.quota_field(pe_id, baseline, "used_guaranteed_acch"),
            api.quota_field(pe_id, competitors[0], "used_guaranteed_acch"),
        ),
        accept=lambda v: v[0] > 0 and v[1] > 0,
        deadline=deadline,
    )
    assert baseline_used > 0, "the baseline's job was not billed against its guaranteed quota -- a role that runs for free is a hole in the budget"
    assert abs(baseline_used - comp_used) <= 0.01, (
        f"baseline billed {baseline_used} AccH vs competitor {comp_used} for identical jobs -- "
        "role changed accounting, which it must never do"
    )


@pytest.mark.timeout(400)
def test_baseline_survives_stage_cut_drawn_only_from_competitors(api, run_id, experiment, deadline):
    competitors = [make_agent(api, run_id, f"roles-cut-comp-{i}") for i in range(5)]
    baseline = make_agent(api, run_id, "roles-cut-baseline")
    roster = [(a, "competitor", "guaranteed") for a in competitors] + [(baseline, "baseline", "guaranteed")]
    # Five competitors is exactly minSurvivorsForCut: the boundary cuts floor(50% x 5) = 2 and
    # leaves 3, clearing minSurvivorsAfterCut (2). If the baseline counted as a survivor the field
    # would be 6 and the cut 3 -- and the baseline, holding the worst value in the run, would
    # certainly be one of them.
    pe_id = experiment(
        "agent-roles-cut", roster, budget=CUT_BUDGET, max_agents=5,
        report_interval_seconds=10, stages=CUT_STAGES,
    )

    # The baseline runs on the LOW anchor and every competitor on a distinct HIGH-ish anchor, so
    # the baseline holds the single worst value in the run -- the agent a cut would take first if
    # role were ignored.
    # Pinned to H100 (acch_rate 1.0, see controlplane/settings/hypothesisloop.yaml), not the
    # TEST_ACCELERATOR_TYPE default (L40, rate 0.25): CUT_BUDGET/CUT_STAGES are sized so settled
    # cost from all 6 jobs' real run time lands the first boundary right at 20% of budget -- at a
    # lower rate the same run time settles proportionally less and the boundary is never reached.
    # Matches tests/scenarios/agent-roles.sh's own ACCELERATOR_TYPE choice for this exact reason.
    CUT_ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
    baseline_job = api.submit_job(
        pe_id, baseline, hours=CUT_JOB_HOURS,
        job_overrides={"env": _env(CUT_RUN_SECONDS, LOW_ANCHOR), "accelerator_type": CUT_ACCELERATOR_TYPE},
    )
    comp_jobs = [
        api.submit_job(
            pe_id, a, hours=CUT_JOB_HOURS,
            job_overrides={"env": _env(CUT_RUN_SECONDS, str(anchor)), "accelerator_type": CUT_ACCELERATOR_TYPE},
        )
        for a, anchor in zip(competitors, CUT_ANCHORS)
    ]

    stages = eventually(
        "the first stage boundary",
        lambda: api.stages(pe_id),
        accept=lambda st: st["current_stage"] >= 2,
        deadline=deadline,
    )

    cut_agents = {c["agent_id"] for c in stages["cut_agents"]}
    active_agents = set(stages["active_agents"])
    cut_n = len(cut_agents)

    assert baseline not in cut_agents, "the baseline was cut despite holding the worst value in the run"
    assert baseline in active_agents, "the baseline is neither cut nor active -- it fell out of the roster"

    if cut_n == 0:
        # Legitimate only if a tie group straddled the line -- but this scenario's anchors are
        # spaced to avoid ties, so no cut at all means the assertions above proved nothing.
        pytest.fail("the boundary cut nobody -- the 5-competitor field should have cut floor(50%*5)=2")

    strays = cut_agents - set(competitors)
    assert not strays, f"cut agent(s) {strays} are not competitors -- the cut was drawn from the wrong field"
    assert cut_n == 2, (
        f"cut {cut_n} agents, want exactly floor(50% x 5)=2 -- a larger cut means the baseline was "
        "counted into the field"
    )
