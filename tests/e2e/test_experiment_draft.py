"""A platform experiment made from one sentence, end to end against the control plane.

POST /platform-experiments/drafts makes a draft with a planner the platform runs. The planner
proposes the experiment's definition and runs nothing. A person asks for a change in a sentence,
the planner proposes again, and the person accepts at a cost they state: the draft becomes an open
platform experiment with a hosted coordinator, its researchers and one agent of role baseline. No
researcher's machine is asked for and nothing starts until an agent of role baseline has measured
the baseline and it is declared; then the platform starts the run. Here a second baseline agent,
run by the test, measures it and the test declares its job.

No launcher process, no provider and no model is involved. What the planner would do is done
here with its own token, which the launcher's desired state carries, and each reconcile call is
the one a launcher makes. The draft keeps its code in the suite's repository, which is only named
here: nothing is pushed to it.

Exclusive, as test_hosted_agents.py is: it asks as the one launcher the local control plane
gives hosted machines to.
"""
from __future__ import annotations

from datetime import datetime

import pytest

from support.api import API, BASELINE_JOB_WAIT_SECONDS, token_scope
from support.wait import Deadline, eventually

pytestmark = pytest.mark.exclusive

LAUNCHER = "main"
GOAL = "Find out whether a larger hidden layer improves validation accuracy of the sample classifier"


def _at(moment: str) -> datetime:
    return datetime.fromisoformat(moment.replace("Z", "+00:00"))


def _planner_instance(api: API, pe_id: str) -> tuple[dict | None, set[str]]:
    desired = api.launcher_desired(LAUNCHER)
    planners = {a["agent_id"] for a in api.agents(pe_id) if a["flavor"] == "planner"}
    own = [i for i in desired["instances"]
           if (scope := token_scope(i["env"]["HL_TOKEN"]))[:2] == ("agent", pe_id) and scope[2] in planners]
    assert len(own) <= 1, own
    return (own[0] if own else None), set(desired["ending"])


def _proposal(accelerator_type: str, image: str, **more) -> dict:
    return {
        "experiment": {
            "name": "Hidden layer width",
            "description": "Vary the hidden layer of the sample classifier and report val_accuracy. BASELINE config: the sample classifier as it is measured: not yet established",
            "metrics": [{"key": "val_accuracy", "direction": "maximize"}], "report_interval_seconds": 10,
        },
        "accelerator_type": accelerator_type, "image": image,
        "budget_accelerator_hours": 2, "duration_hours": 3, "agents": [{"flavor": "hyperparameter-search"}, {}],
        "metric_reason": "Validation accuracy is what the goal asks about.",
        "measurement": "The sample workload scores a split it does not train on.",
        "sizing_reason": "An estimate, nothing has run: a trial is assumed to take a minute, and two researchers with a few trials each fit in 2 hours of budget.",
    } | more


def _researcher_instances(api: API, pe_id: str, researchers: list[dict]) -> list[dict]:
    ids = {a["agent_id"] for a in researchers}
    return [i for i in api.launcher_desired(LAUNCHER)["instances"]
            if (scope := token_scope(i["env"]["HL_TOKEN"]))[:2] == ("agent", pe_id) and scope[2] in ids]


def test_a_sentence_becomes_a_running_experiment_through_a_checked_proposal(api: API, deadline: Deadline):
    created = api.post("/platform-experiments/drafts", {"project_id": api.project_id, "goal": GOAL})
    assert created.status_code == 201, created.text
    draft = created.json()
    pe_id = draft["id"]
    try:
        assert draft["status"] == "draft" and draft["goal"] == GOAL and draft["budget_accelerator_hours"] == 2.0, draft
        assert draft["actor"]["kind"] in ("staff", "user", "key"), draft["actor"]
        assert draft["repository"] == api.sandbox_repository() and draft["repository"] in draft["repository_url"], draft

        # Its one agent is the planner, and the launcher is asked for its machine.
        roster = api.agents(pe_id)
        assert [(a["name"], a["flavor"], a["role"], a["hosted"]) for a in roster] == [("Planner", "planner", "planner", True)], roster
        planner_id = roster[0]["agent_id"]
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None and token_scope(instance["env"]["HL_TOKEN"])[2] == planner_id, instance
        planner = api.as_caller(instance["env"]["HL_TOKEN"], api.org_id, api.project_id)

        # Nobody joins a draft, nobody but the planner proposes, and nothing is accepted yet.
        joining = api.post(f"/platform-experiments/{pe_id}/agents", {})
        assert (joining.status_code, joining.json()["reason"]) == (409, "signup_closed"), joining.text
        cluster, line = next((c, a) for c in api.capacity()["clusters"] for a in c["accelerators"] if a["kind"] != "cpu" and a["available"] > 0)
        on, image = line["accelerator_type"], cluster.get("job_image") or line["job_images"][0]
        assert api.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image)).status_code == 403
        early = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "agents": [{}]})
        assert (early.status_code, early.json()["reason"]) == (409, "invalid_transition"), early.text

        # A planner runs nothing: a draft takes no job, and a proposal names none. It states the
        # type its jobs are sized for, which must be one the org can run on.
        hypothesis = planner.post_hypothesis(pe_id, "the baseline: the sample classifier as it is", agent_id=planner_id)
        if hypothesis.status_code in (200, 201):
            body = planner.submission_body_for_id(f"job-{pe_id[-8:]}", pe_id, planner_id)
            refused = planner.post_experiment_body(body)
            assert refused.status_code >= 400 and "experiment_not_started" in refused.text, refused.text
        naming_a_job = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image, baseline_job_id="job-none"))
        assert naming_a_job.status_code == 422, naming_a_job.text
        unreachable = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal("no-such-accelerator", image))
        assert (unreachable.status_code, unreachable.json()["reason"]) == (422, "accelerator_unreachable"), unreachable.text
        no_image = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, "no-such-image"))
        assert (no_image.status_code, no_image.json()["reason"]) == (422, "unknown_job_image"), no_image.text
        stating_none = _proposal(on, image)
        del stating_none["image"]
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", stating_none).status_code == 422, "a proposal that states no image was stored"
        no_quantity = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image, memory="plenty"))
        assert (no_quantity.status_code, no_quantity.json()["reason"]) == (422, "invalid_resources"), no_quantity.text
        working = api.get_json(f"/platform-experiments/{pe_id}")
        assert (working["status_reason"]["reason"], working["status_reason"]["waits_for"]) == ("planning", "platform"), working["status_reason"]
        assert "planner_ends_at" not in working, working

        node = {"cpu": "2", "memory": "4Gi", "storage": "10Gi"}
        proposed = planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image, **node))
        assert proposed.status_code == 200, proposed.text
        assert proposed.json()["proposed_at"] and proposed.json()["status"] == "draft"
        waits = api.get_json(f"/platform-experiments/{pe_id}")["status_reason"]
        assert (waits["reason"], waits["waits_for"]) == ("proposed", "owner"), waits
        assert proposed.json()["proposal"]["accelerator_type"] == on and "baseline_job_id" not in proposed.json()["proposal"], proposed.json()["proposal"]
        assert {key: proposed.json()["proposal"][key] for key in ("image", *node)} == {"image": image, **node}, proposed.json()["proposal"]
        # Proposed, the planner's machine is kept for a revision asked for soon: until the
        # draft's planner_ends_at, which a second answer does not move.
        kept_until = proposed.json()["planner_ends_at"]
        assert _at(kept_until) > _at(proposed.json()["proposed_at"]), proposed.json()
        kept, _ = _planner_instance(api, pe_id)
        assert kept is not None and kept["name"] == instance["name"], "the planner's machine was released with its proposal"
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image, **node)).json()["planner_ends_at"] == kept_until

        # A change asked for in a sentence reaches the planner that is still there, and it
        # proposes again.
        revised = api.post(f"/platform-experiments/{pe_id}/revise", {"message": "Use three researchers at most."})
        assert revised.status_code == 200 and "proposed_at" not in revised.json() and "planner_ends_at" not in revised.json(), revised.text
        assert [r["message"] for r in revised.json()["revisions"]] == ["Use three researchers at most."]
        assert revised.json()["status_reason"]["reason"] == "revising"
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None and instance["name"] == kept["name"], "a draft revised while its planner was kept got another machine"
        assert planner.put(f"/platform-experiments/{pe_id}/proposal", _proposal(on, image, agents=[{}, {}, {}])).status_code == 200

        # Accepted at the cost the person states, whatever was proposed, and under the name the
        # person gives it.
        accepted = api.post(f"/platform-experiments/{pe_id}/accept", {
            "budget_accelerator_hours": 1, "duration_hours": 2, "agents": [{"flavor": "data-search"}, {}], "name": "Hidden layer width, accepted"})
        assert accepted.status_code == 200, accepted.text
        pe = accepted.json()
        assert (pe["status"], pe["name"], pe["budget_accelerator_hours"], pe["max_agents"]) == ("open", "Hidden layer width, accepted", 1, 2), pe
        assert pe["coordinator"]["launcher"] == LAUNCHER and pe["goal"] == GOAL
        # The accept made the researchers and the one agent that measures the baseline.
        joined = [a for a in api.agents(pe_id) if a["flavor"] != "planner"]
        assert all(a["hosted"] for a in joined), joined
        researchers = [a for a in joined if a["role"] == "competitor"]
        assert sorted(a["flavor"] for a in researchers) == ["data-search", "generalist"], researchers
        measurers = [a for a in joined if a["role"] == "baseline"]
        assert [a["name"] for a in measurers] == ["Baseline 1"] and len(joined) == 3, joined
        again = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 2, "agents": [{}]})
        assert (again.status_code, again.json()["reason"]) == (409, "invalid_transition"), again.text

        # Accepted, nothing has run and nothing starts: the baseline agent's machine is asked for
        # and no researcher's, no researcher's job is taken, and it stays open while the baseline
        # is measured.
        assert "baseline_job_id" not in pe, pe
        assert _researcher_instances(api, pe_id, researchers) == [], "a researcher's machine is asked for before there is a baseline"
        assert len(_researcher_instances(api, pe_id, measurers)) == 1, "the baseline agent of an accepted draft has no machine asked for"
        assert all((a["instance"]["state"], a["status_reason"]["reason"]) == ("waiting", "baseline_measuring") for a in researchers), researchers
        unstarted = api.get_json(f"/platform-experiments/{pe_id}")
        assert (unstarted["status"], unstarted["status_reason"]["reason"], unstarted["status_reason"]["waits_for"]) == ("open", "baseline_measuring", "platform"), unstarted
        assert unstarted["baseline_due_at"] and "ends_at" not in unstarted and "started_at" not in unstarted, unstarted
        too_early = api.post_experiment_body(api.submission_body_for_id(f"job-{pe_id[-8:]}-r", pe_id, researchers[0]["agent_id"]))
        assert too_early.status_code >= 400 and "experiment_not_started" in too_early.text, too_early.text

        # What the coordinator has done: an agent of role baseline measures the control, and the
        # baseline is declared with the brief that names it. The platform starts the run itself.
        baseline = api.declare_baseline(pe_id, job_wait_seconds=BASELINE_JOB_WAIT_SECONDS)
        declared = api.get_json(f"/platform-experiments/{pe_id}")
        assert declared["baseline_job_id"] == baseline.job and declared["job_estimated_duration_hours"] > 0, declared
        eventually("the accepted draft to run once its baseline is declared", lambda: api.get_json(f"/platform-experiments/{pe_id}"),
                   accept=lambda p: p["status"] == "running", deadline=deadline, interval=2)
        assert len(_researcher_instances(api, pe_id, researchers)) == len(researchers), "a running experiment's researchers have no machine asked for"
        shares = {q["agent_id"]: q["guaranteed_accelerator_hours"] for q in api.platform_experiment_quotas(pe_id)}
        assert planner_id not in shares, "a planner holds a quota"
        # The first stage's share of the budget over the researchers' places.
        assert shares[baseline.agent] == pytest.approx(pe["stages"][0]["length_pct"] / 100 * 1 / 2), shares
        assert all(0 < shares[a["agent_id"]] < shares[baseline.agent] for a in researchers), "the baseline's cost was not taken from the budget before the shares"
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
        assert (draft["status"], draft["status_reason"]["reason"], draft["declined_reason"]) == ("draft", "declined", why), draft
        assert draft["declined_at"] and "proposed_at" not in draft and "proposal" not in draft, draft
        assert draft["status_reason"]["waits_for"] == "owner", draft["status_reason"]
        # Answered, the planner's machine is kept for a revision asked for soon, as after a
        # proposal, and there is nothing to accept.
        assert _at(draft["planner_ends_at"]) > _at(draft["declined_at"]), draft
        kept, _ = _planner_instance(api, pe_id)
        assert kept is not None and kept["name"] == instance["name"], "the machine of a planner that declined was released at once"
        accept = api.post(f"/platform-experiments/{pe_id}/accept", {"budget_accelerator_hours": 1, "duration_hours": 1, "agents": [{}]})
        assert (accept.status_code, accept.json()["reason"]) == (409, "invalid_transition") and "declined" in accept.json()["message"], accept.text

        # The person says what was missing: the planner is back, and the reason is still readable.
        revised = api.post(f"/platform-experiments/{pe_id}/revise", {"message": "Validation accuracy of the sample classifier."})
        assert revised.status_code == 200 and "declined_at" not in revised.json() and revised.json()["declined_reason"] == why, revised.text
        assert api.get_json(f"/platform-experiments/{pe_id}")["status_reason"]["reason"] == "revising"
        instance, _ = _planner_instance(api, pe_id)
        assert instance is not None and instance["name"] == kept["name"], "a draft revised while its planner was kept got another machine"
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
