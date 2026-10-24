"""A platform experiment made from one sentence, end to end against the control plane.

POST /platform-experiments/drafts makes a draft with a planner the platform runs. The planner
proposes the experiment's definition and runs nothing. A person asks for a change in a sentence,
the planner proposes again, and the person accepts at a cost they state: the draft becomes an open
platform experiment with a sandboxed coordinator, its executors and one agent of role baseline. No
executor's machine is asked for and nothing starts until an agent of role baseline has measured
the baseline and it is declared; then the platform starts the run. Here a second baseline agent,
run by the test, measures it and the test declares its job.

What the planner would do is done here with the planner's own token, and whether a sandbox is
asked for an agent is read from its instance on the roster. The draft keeps its code in the
suite's repository, which is only named here: nothing is pushed to it.

A stack with a sandbox provider rents a sandbox for the planner and each agent asked for, and the
planner it runs works on the same draft, so the module carries the real_providers marker.
"""
from __future__ import annotations

from datetime import datetime

import pytest

from support import admin
from support.api import API, BASELINE_JOB_WAIT_SECONDS
from support.wait import Deadline, eventually

pytestmark = [pytest.mark.exclusive, pytest.mark.real_providers]

GOAL = "Find out whether a larger hidden layer improves validation accuracy of the sample classifier"


def _at(moment: str) -> datetime:
    return datetime.fromisoformat(moment.replace("Z", "+00:00"))


def _asked(agent: dict) -> bool:
    """Whether a sandbox is asked for the agent now."""
    return agent["instance"]["state"] in ("requested", "starting", "running")


def _planner(api: API, pe_id: str) -> dict:
    planners = [a for a in api.agents(pe_id) if a["agent_type"] == "planner"]
    assert len(planners) == 1, planners
    return planners[0]


def _planner_caller(api: API, pe_id: str) -> API:
    return api.as_caller(admin.token(f"agent:{pe_id}/{_planner(api, pe_id)['agent_id']}"), api.org_id, api.project_id)


def _proposal(accelerator_type: str, **more) -> dict:
    return {
        "experiment": {
            "name": "Hidden layer width",
            "description": "Vary the hidden layer of the sample classifier and report val_accuracy. BASELINE config: the sample classifier as it is measured: not yet established",
            "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "report_interval_seconds": 10,
        },
        "goal_version": 1, "accelerator_type": accelerator_type, "cpu": "1", "memory": "1Gi", "storage": "1Gi",
        "budget_accelerator_hours": 2, "duration_hours": 3, "baseline_minutes": 45, "agents": [{"agent_type": "hyperparameter-search"}, {}],
        "metric_reason": "Validation accuracy is what the goal asks about.",
        "measurement": "The sample workload scores a split it does not train on.",
        "sizing_reason": "An estimate, nothing has run: a trial is assumed to take a minute, and two executors with a few trials each fit in 2 hours of budget.",
    } | more


def _asked_for(api: API, pe_id: str, agents: list[dict]) -> list[dict]:
    ids = {a["agent_id"] for a in agents}
    return [a for a in api.agents(pe_id) if a["agent_id"] in ids and _asked(a)]


def test_a_sentence_becomes_a_running_experiment_through_a_checked_proposal(api: API, deadline: Deadline):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": GOAL})
    assert created.status_code == 201, created.text
    draft = created.json()
    pe_id = draft["id"]
    try:
        # A draft has no budget: the proposal states one and the person accepts at a cost they state.
        assert draft["status"] == "draft" and draft["goal"] == GOAL and draft["budget_accelerator_hours"] == 0, draft
        assert draft["actor"]["kind"] in ("staff", "user", "key"), draft["actor"]
        assert draft["repository"] == api.sandbox_repository() and draft["repository"] in draft["repository_url"], draft

        # Its one agent is the planner, and a sandbox is asked for it.
        roster = api.agents(pe_id)
        assert [(a["name"], a["agent_type"], a["role"], a["in_agentsandbox"]) for a in roster] == [("Planner", "planner", "planner", True)], roster
        planner_id = roster[0]["agent_id"]
        assert _asked(roster[0]), roster[0]
        instance = roster[0]["instance"]
        planner = _planner_caller(api, pe_id)

        # Nobody joins a draft, nobody but the planner proposes, and nothing is accepted yet.
        joining = api.post(f"/platform-experiments/{pe_id}/agents", {})
        assert (joining.status_code, joining.json()["reason"]) == (409, "signup_closed"), joining.text
        on = next(a["accelerator_type"] for c in api.capacity()["clusters"] for a in c["accelerators"] if a["kind"] != "cpu" and a["available"] > 0)
        assert api.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on)).status_code == 403
        early = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "baseline_minutes": 45, "agents": [{}]})
        assert (early.status_code, early.json()["reason"]) == (409, "invalid_transition"), early.text

        # A planner runs nothing: a draft takes no job, and a proposal names none. It states the
        # type its jobs are sized for, which must be one the org can run on.
        hypothesis = planner.post_hypothesis(pe_id, "the baseline: the sample classifier as it is", agent_id=planner_id)
        if hypothesis.status_code in (200, 201):
            body = planner.submission_body_for_id(f"job-{pe_id[-8:]}", pe_id, planner_id)
            refused = planner.post_experiment_body(body)
            assert refused.status_code >= 400 and "experiment_not_started" in refused.text, refused.text
        naming_a_job = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, baseline_job_id="job-none"))
        assert naming_a_job.status_code == 422, naming_a_job.text
        unreachable = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal("no-such-accelerator"))
        assert (unreachable.status_code, unreachable.json()["reason"]) == (422, "accelerator_unreachable"), unreachable.text
        no_quantity = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, memory="plenty"))
        assert (no_quantity.status_code, no_quantity.json()["reason"]) == (422, "invalid_resources"), no_quantity.text
        working = api.get_json(f"/platform-experiments/{pe_id}")
        assert (working["status_reason"]["reason"], working["status_reason"]["waits_for"]) == ("planning", "platform"), working["status_reason"]
        assert "planner_ends_at" not in working, working

        node = {"cpu": "2", "memory": "4Gi", "storage": "10Gi"}
        proposed = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, **node))
        assert proposed.status_code == 200, proposed.text
        assert proposed.json()["proposed_at"] and proposed.json()["status"] == "draft"
        # Proposed, the platform accepts it as proposed at accepts_at unless the person acts
        # first, and the agent that measures the baseline is signed up with a machine asked for.
        counting = api.get_json(f"/platform-experiments/{pe_id}")
        assert (counting["status_reason"]["reason"], counting["status_reason"]["waits_for"]) == ("accepting", "platform"), counting["status_reason"]
        assert _at(counting["accepts_at"]) > _at(proposed.json()["proposed_at"]), counting
        measurers = [a for a in api.agents(pe_id) if a["role"] == "baseline"]
        assert [a["name"] for a in measurers] == ["Baseline 1"], measurers
        assert len(_asked_for(api, pe_id, measurers)) == 1, "the baseline agent of a proposal has no machine asked for"
        # Held, it waits for the person and the baseline agent's machine is let go until the accept.
        held = api.post(f"/platform-experiments/{pe_id}/hold")
        assert held.status_code == 200 and "accepts_at" not in held.json(), held.text
        waits = api.get_json(f"/platform-experiments/{pe_id}")["status_reason"]
        assert (waits["reason"], waits["waits_for"]) == ("proposed", "owner"), waits
        assert proposed.json()["proposal"]["accelerator_type"] == on and "baseline_job_id" not in proposed.json()["proposal"], proposed.json()["proposal"]
        assert {key: proposed.json()["proposal"][key] for key in node} == node, proposed.json()["proposal"]
        # Proposed, the planner's machine is kept for a goal changed soon: until the
        # draft's planner_ends_at, which a second answer does not move.
        kept_until = proposed.json()["planner_ends_at"]
        assert _at(kept_until) > _at(proposed.json()["proposed_at"]), proposed.json()
        kept = _planner(api, pe_id)
        assert _asked(kept) and kept["instance"].get("name") == instance.get("name"), "the planner's machine was released with its proposal"
        kept = kept["instance"]
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, **node)).json()["planner_ends_at"] == kept_until
        assert api.post(f"/platform-experiments/{pe_id}/hold").status_code == 200

        # A changed goal reaches the planner that is still there, and it proposes again from it.
        goal = api.get_json(f"/platform-experiments/{pe_id}")["goal"] + " Use three executors at most."
        revised = api.patch(f"/platform-experiments/{pe_id}", {"goal": goal})
        assert revised.status_code == 200 and "proposed_at" not in revised.json() and "planner_ends_at" not in revised.json(), revised.text
        assert (revised.json()["goal"], revised.json()["goal_version"]) == (goal, 2), revised.json()
        assert revised.json()["status_reason"]["reason"] == "revising"
        instance = _planner(api, pe_id)["instance"]
        assert instance.get("name") == kept.get("name"), "a draft whose goal changed while its planner was kept got another machine"
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, agents=[{}, {}, {}], goal_version=2)).status_code == 200
        assert api.post(f"/platform-experiments/{pe_id}/hold").status_code == 200

        # Accepted at the cost the person states, whatever was proposed, and under the name the
        # person gives it.
        accepted = api.post(f"/platform-experiments/{pe_id}/accept", {
            "budget_accelerator_hours": 1, "duration_hours": 2, "baseline_minutes": 45, "agents": [{"agent_type": "data-search"}, {}], "name": "Hidden layer width, accepted"})
        assert accepted.status_code == 200, accepted.text
        pe = accepted.json()
        assert (pe["status"], pe["name"], pe["budget_accelerator_hours"], pe["max_agents"]) == ("open", "Hidden layer width, accepted", 1, 2), pe
        assert pe["coordinator"]["launcher"] and pe["goal"] == GOAL
        # The accept made the executors, beside the one agent the proposal signed up to measure
        # the baseline.
        joined = [a for a in api.agents(pe_id) if a["agent_type"] != "planner"]
        assert all(a["in_agentsandbox"] for a in joined), joined
        executors = [a for a in joined if a["role"] == "executor"]
        assert sorted(a["agent_type"] for a in executors) == ["data-search", "generalist"], executors
        measurers = [a for a in joined if a["role"] == "baseline"]
        assert [a["name"] for a in measurers] == ["Baseline 1"] and len(joined) == 3, joined
        again = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 2, "baseline_minutes": 45, "agents": [{}]})
        assert (again.status_code, again.json()["reason"]) == (409, "invalid_transition"), again.text

        # Accepted, nothing has run and nothing starts: the baseline agent's machine is asked for
        # and no executor's, no executor's job is taken, and it stays open while the baseline
        # is measured.
        assert "baseline_job_id" not in pe, pe
        assert _asked_for(api, pe_id, executors) == [], "an executor's machine is asked for before there is a baseline"
        assert len(_asked_for(api, pe_id, measurers)) == 1, "the baseline agent of an accepted draft has no machine asked for"
        assert all((a["instance"]["state"], a["status_reason"]["reason"]) == ("waiting", "baseline_measuring") for a in executors), executors
        unstarted = api.get_json(f"/platform-experiments/{pe_id}")
        assert (unstarted["status"], unstarted["status_reason"]["reason"], unstarted["status_reason"]["waits_for"]) == ("open", "baseline_measuring", "platform"), unstarted
        assert unstarted["baseline_due_at"] and "ends_at" not in unstarted and "started_at" not in unstarted, unstarted
        too_early = api.post_experiment_body(api.submission_body_for_id(f"job-{pe_id[-8:]}-r", pe_id, executors[0]["agent_id"]))
        assert (too_early.status_code, too_early.json()["reason"]) == (422, "experiment_not_started"), too_early.text
        assert "baseline_measuring" in too_early.json()["message"], f"the refusal does not say what the start waits for: {too_early.text}"

        # What the coordinator has done: an agent of role baseline measures the control, and the
        # baseline is declared with the brief that names it. The platform starts the run itself.
        baseline = api.declare_baseline(pe_id, job_wait_seconds=BASELINE_JOB_WAIT_SECONDS)
        declared = api.get_json(f"/platform-experiments/{pe_id}")
        assert declared["baseline_job_id"] == baseline.job and declared["job_estimated_duration_hours"] > 0, declared
        eventually("the accepted draft to run once its baseline is declared", lambda: api.get_json(f"/platform-experiments/{pe_id}"),
                   accept=lambda p: p["status"] == "running", deadline=deadline, interval=2)
        assert len(_asked_for(api, pe_id, executors)) == len(executors), "a running experiment's executors have no machine asked for"
        shares = {q["agent_id"]: q["guaranteed_accelerator_hours"] for q in api.platform_experiment_quotas(pe_id)}
        assert planner_id not in shares, "a planner holds a quota"
        # Half the budget.
        assert shares[baseline.agent] == pytest.approx(1 / 2), shares
        # Two executors are too few for a cut, so the run has one stage and each holds its whole
        # share of what the baseline left of the budget.
        assert [(s["length_pct"], s["evict_pct"]) for s in api.stages(pe_id)["stages"]] == [(100, 0)]
        spent = api.quota_field(pe_id, baseline.agent, "used_guaranteed_acch")
        assert spent > 0, "the baseline job cost nothing"
        assert all(shares[a["agent_id"]] == pytest.approx((1 - spent) / 2, abs=1e-6) for a in executors), \
            f"the baseline's cost was not taken from the budget before the shares: {shares}, baseline spent {spent}"
        assert not _asked(_planner(api, pe_id)), "the planner is run in the experiment it planned"
    finally:
        api.close_platform_experiment(pe_id)


def test_a_goal_the_planner_declines_shows_why_and_can_be_answered(api: API):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": "Make it better"})
    assert created.status_code == 201, created.text
    pe_id = created.json()["id"]
    try:
        assert _asked(_planner(api, pe_id))
        instance = _planner(api, pe_id)["instance"]
        planner = _planner_caller(api, pe_id)
        why = "The goal names nothing to measure: say what should get better, and by which number."

        assert api.put(f"/platform-experiments/{pe_id}/decline", {"reason": why, "goal_version": 1}).status_code == 403, "only the planner declines"
        assert planner.put(f"/platform-experiments/{pe_id}/decline", {"reason": " ", "goal_version": 1}).status_code == 422
        declined = planner.put(f"/platform-experiments/{pe_id}/decline", {"reason": why, "goal_version": 1})
        assert declined.status_code == 200, declined.text
        draft = api.get_json(f"/platform-experiments/{pe_id}")
        assert (draft["status"], draft["status_reason"]["reason"], draft["declined_reason"]) == ("draft", "declined", why), draft
        assert draft["declined_at"] and "proposed_at" not in draft and "proposal" not in draft, draft
        assert draft["status_reason"]["waits_for"] == "owner", draft["status_reason"]
        # Answered, the planner's machine is kept for a goal changed soon, as after a
        # proposal, and there is nothing to accept.
        assert _at(draft["planner_ends_at"]) > _at(draft["declined_at"]), draft
        kept = _planner(api, pe_id)
        assert _asked(kept) and kept["instance"].get("name") == instance.get("name"), "the machine of a planner that declined was released at once"
        kept = kept["instance"]
        accept = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "baseline_minutes": 45, "agents": [{}]})
        assert (accept.status_code, accept.json()["reason"]) == (409, "invalid_transition") and "declined" in accept.json()["message"], accept.text

        # The person says what was missing: the planner is back, and the reason is still readable.
        revised = api.patch(f"/platform-experiments/{pe_id}", {"goal": "Make the model better: validation accuracy of the sample classifier."})
        assert revised.status_code == 200 and "declined_at" not in revised.json() and revised.json()["declined_reason"] == why, revised.text
        assert api.get_json(f"/platform-experiments/{pe_id}")["status_reason"]["reason"] == "revising"
        instance = _planner(api, pe_id)["instance"]
        assert instance.get("name") == kept.get("name"), "a draft whose goal changed while its planner was kept got another machine"
    finally:
        api.close_platform_experiment(pe_id)
    assert api.get_json(f"/platform-experiments/{pe_id}")["status"] == "closed"


def test_a_draft_nobody_wants_is_closed(api: API):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": GOAL})
    assert created.status_code == 201, created.text
    pe_id = created.json()["id"]
    api.close_platform_experiment(pe_id)
    assert api.get_json(f"/platform-experiments/{pe_id}")["status"] == "closed"
    empty = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": "  "})
    assert empty.status_code == 422, empty.text
