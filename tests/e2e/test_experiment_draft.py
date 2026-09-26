"""A platform experiment made from one sentence, end to end against the control plane.

POST /platform-experiments/drafts makes a draft with a planner the platform runs. The planner
runs a baseline job against the draft's allowance, files what it found, and proposes the
experiment; the proposal is held to what that job really did. A person asks for a change in a
sentence, the planner proposes again, and the person accepts at a cost they state: the draft
becomes an open platform experiment with a hosted coordinator and its researchers, and starts.

No launcher process, no provider and no model is involved. What the planner would do is done
here with its own token, which the launcher's desired state carries, and each reconcile call is
the one a launcher makes. A draft names no repository, so nothing here needs GitHub.

Exclusive, as test_hosted_agents.py is: it asks as the one launcher the local control plane
gives hosted machines to.
"""
from __future__ import annotations

import pytest

from support.api import API
from support.wait import Deadline, eventually

pytestmark = pytest.mark.exclusive

LAUNCHER = "main"
GOAL = "Find out whether a larger hidden layer improves validation accuracy of the sample classifier"


def _planner_instance(api: API, pe_id: str) -> tuple[dict | None, set[str]]:
    desired = api.launcher_desired(LAUNCHER)
    own = [i for i in desired["instances"] if i["env"]["PLATFORM_EXPERIMENT_ID"] == pe_id and i["env"].get("AGENT_FLAVOR") == "planner"]
    assert len(own) <= 1, own
    return (own[0] if own else None), set(desired["ending"])


def _proposal(job_id: str, accelerator_type: str = "none-yet", **more) -> dict:
    return {
        "accelerator_type": accelerator_type,
        "experiment": {
            "name": "Hidden layer width", "description": "Vary the hidden layer of the sample classifier and report val_accuracy.",
            "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "report_interval_seconds": 10,
        },
        "budget_accelerator_hours": 2, "duration_hours": 3, "agents": [{"flavor": "hyperparameter-search"}, {}],
        "baseline_job_id": job_id, "metric_reason": "Validation accuracy is what the goal asks about.",
        "measurement": "The sample workload scores a split it does not train on.",
    } | more


def test_a_sentence_becomes_a_running_experiment_through_a_checked_proposal(api: API, deadline: Deadline):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": GOAL})
    assert created.status_code == 201, created.text
    draft = created.json()
    pe_id = draft["id"]
    try:
        assert draft["status"] == "draft" and draft["goal"] == GOAL and draft["budget_accelerator_hours"] == 2.0, draft
        assert draft["created_by"]["kind"] in ("staff", "user", "key"), draft["created_by"]
        assert "repository_url" not in draft

        # Its one agent is the planner, and the launcher is asked for its machine.
        roster = api.agents(pe_id)
        assert [(a["name"], a["flavor"], a["role"], a["hosted"]) for a in roster] == [("Planner", "planner", "baseline", True)], roster
        planner_id = roster[0]["agent_id"]
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None and instance["env"]["AGENT_ID"] == planner_id, instance
        planner = api.as_caller(instance["env"]["HL_TOKEN"], api.org_id, api.project_id)

        # Nobody joins a draft, nobody but the planner proposes, and nothing is accepted yet.
        joining = api.post(f"/platform-experiments/{pe_id}/agents", {})
        assert (joining.status_code, joining.json()["reason"]) == (400, "signup_closed"), joining.text
        assert api.put(f"/platform-experiments/{pe_id}/proposal", _proposal("job-none")).status_code == 403
        early = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "agents": [{}]})
        assert (early.status_code, early.json()["reason"]) == (409, "invalid_transition"), early.text

        # A proposal rests on a baseline the platform saw: none, then one still running.
        job = planner.submit_job(pe_id, planner_id, hours=0.02, hyp_text="the baseline: the sample classifier as it is")
        # The type the baseline runs on is the one the proposal sizes its jobs for.
        on = api.experiment(job)["job"]["accelerator_type"]
        untyped = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job))
        assert (untyped.status_code, untyped.json()["reason"]) == (422, "unknown_accelerator_type"), untyped.text
        none = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal("job-none", on))
        assert (none.status_code, none.json()["reason"]) == (422, "no_baseline"), none.text
        running = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job, on))
        assert (running.status_code, running.json()["reason"]) == (422, "baseline_not_completed"), running.text
        eventually("the planner's baseline job to complete", lambda: api.experiment(job),
                   accept=lambda e: e["status"] == "COMPLETED", reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"), deadline=deadline)
        unfiled = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job, on))
        assert (unfiled.status_code, unfiled.json()["reason"]) == (422, "baseline_without_finding"), unfiled.text
        planner.file_finding(job, "baseline: the sample classifier as it is")
        unmeasured = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job, on, experiment=_proposal(job)["experiment"] | {
            "metrics": [{"key": "latency_never_reported", "direction": "minimize"}]}))
        assert (unmeasured.status_code, unmeasured.json()["reason"]) == (422, "baseline_missing_metric"), unmeasured.text

        proposed = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job, on))
        assert proposed.status_code == 200, proposed.text
        assert proposed.json()["proposed_at"] and proposed.json()["status"] == "draft"
        # Proposed, the planner's machine is only let wind down.
        instance, _ = _planner_instance(api, pe_id)
        assert instance is None, "a machine is kept beside a proposal that waits for a person"

        # A change asked for in a sentence brings the planner back, and it proposes again.
        revised = api.post(f"/platform-experiments/{pe_id}/revise", {"message": "Use three researchers at most."})
        assert revised.status_code == 200 and "proposed_at" not in revised.json(), revised.text
        assert [r["message"] for r in revised.json()["revisions"]] == ["Use three researchers at most."]
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None, "a revised draft has no planner"
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(job, on, agents=[{}, {}, {}])).status_code == 200

        # Accepted at the cost the person states, whatever was proposed.
        accepted = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 2, "agents": [{"flavor": "data-search"}, {}]})
        assert accepted.status_code == 200, accepted.text
        pe = accepted.json()
        assert (pe["status"], pe["name"], pe["budget_accelerator_hours"], pe["max_agents"]) == ("open", "Hidden layer width", 1, 2), pe
        assert pe["coordinator"]["launcher"] == LAUNCHER and pe["goal"] == GOAL
        researchers = [a for a in api.agents(pe_id) if a["flavor"] != "planner"]
        assert sorted(a["flavor"] for a in researchers) == ["data-search", "generalist"] and all(a["hosted"] for a in researchers), researchers
        again = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 2, "agents": [{}]})
        assert (again.status_code, again.json()["reason"]) == (409, "invalid_transition"), again.text

        # Started by the caller or by the platform's own sweep, whichever is first; the budget is
        # the researchers' and the planner takes no share of it.
        api.post(f"/platform-experiments/{pe_id}/start")
        eventually("the accepted draft to run", lambda: api.get_json(f"/platform-experiments/{pe_id}"), accept=lambda p: p["status"] == "running", deadline=deadline)
        shares = {q["agent_id"]: q["guaranteed_accelerator_hours"] for q in api.platform_experiment_quotas(pe_id)}
        assert shares[planner_id] == 2.0, "the planner's quota is still the planning allowance"
        assert all(shares[a["agent_id"]] > 0 for a in researchers), shares
        instance, _ = _planner_instance(api, pe_id)
        assert instance is None, "the planner is run in the experiment it planned"
    finally:
        api.close_platform_experiment(pe_id)


def test_a_goal_the_planner_declines_shows_why_and_can_be_answered(api: API):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": "Make it better"})
    assert created.status_code == 201, created.text
    pe_id = created.json()["id"]
    try:
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None
        planner = api.as_caller(instance["env"]["HL_TOKEN"], api.org_id, api.project_id)
        why = "The goal names nothing to measure: say what should get better, and by which number."

        assert api.put(f"/platform-experiments/{pe_id}/decline", {"reason": why}).status_code == 403, "only the planner declines"
        assert planner.put(f"/platform-experiments/{pe_id}/decline", {"reason": " "}).status_code == 422
        declined = planner.put(f"/platform-experiments/{pe_id}/decline", {"reason": why})
        assert declined.status_code == 200, declined.text
        draft = api.get_json(f"/platform-experiments/{pe_id}")
        assert (draft["status"], draft["declined_reason"]) == ("draft", why) and draft["proposed_at"] and "proposal" not in draft, draft
        # Answered, the planner's machine is released, and there is nothing to accept.
        instance, _ = _planner_instance(api, pe_id)
        assert instance is None, "a machine is kept for a planner that declined"
        accept = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "agents": [{}]})
        assert (accept.status_code, accept.json()["reason"]) == (409, "invalid_transition") and "declined" in accept.json()["message"], accept.text

        # The person says what was missing: the planner is back, and the reason is still readable.
        revised = api.post(f"/platform-experiments/{pe_id}/revise", {"message": "Validation accuracy of the sample classifier."})
        assert revised.status_code == 200 and "proposed_at" not in revised.json() and revised.json()["declined_reason"] == why, revised.text
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None, "a revised draft has no planner"
    finally:
        api.close_platform_experiment(pe_id)
    assert api.get_json(f"/platform-experiments/{pe_id}")["status"] == "closed"


def test_a_draft_nobody_wants_is_closed_and_frees_its_place(api: API):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": GOAL})
    assert created.status_code == 201, created.text
    pe_id = created.json()["id"]
    api.close_platform_experiment(pe_id)
    assert api.get_json(f"/platform-experiments/{pe_id}")["status"] == "closed"
    empty = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": "  "})
    assert empty.status_code == 422, empty.text
