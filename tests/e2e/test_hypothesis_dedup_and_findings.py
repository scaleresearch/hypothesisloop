"""Two core "shared evidence pool" properties (README: agents register/retrieve equivalent
hypotheses; agents can read every hypothesis, the jobs that tested it, and the findings). Ported
1:1 from tests/scenarios/hypothesis-dedup-and-findings.sh:
  1. registering equivalent hypothesis text twice returns the SAME row (already_existed=true,
     identical id), not a duplicate.
  2. a finding filed by one agent against a hypothesis is visible to a DIFFERENT agent that reads
     the same hypothesis afterward.
  3. the pool is shared with humans: a UI-posted idea under a typed name lands in the same listing
     agents read, dedups against agent rows under the same unique index, and is testable like any
     other -- an agent runs a real job against it, the finding lands on the human's row attributed
     to that agent, and any agent may settle the unowned row's status. The human author never gets
     quota or a place in the standings.

API-only, parallel-safe (its own platform experiment).

PROVEN against the live stack: 3x solo green. The job-submitting tests needed
quota_tier="guaranteed" at signup (an agent-kind participant otherwise defaults to burst_only, so a
guaranteed-tier job submission 402s with insufficient_guaranteed_quota).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

pytestmark = pytest.mark.parallel


def _completed(api, job, deadline):
    final = eventually(
        f"{job} to finish",
        lambda: api.experiment(job),
        accept=lambda e: e["status"] in ("COMPLETED", "FAILED", "EVICTED"),
        deadline=deadline,
    )
    assert final["status"] == "COMPLETED", f"{job} ended as {final['status']!r}, expected COMPLETED"
    return final


def test_registering_equivalent_text_twice_returns_the_same_row(api, experiment, run_id):
    agent_a = make_agent(api, run_id, "dedup-a")
    agent_b = make_agent(api, run_id, "dedup-b")
    pe_id = experiment("hypothesis-dedup", [(agent_a, None, "guaranteed"), (agent_b, None, "guaranteed")], budget=1.0)

    text = "Higher learning rate converges faster to a better task_success_rate"
    first = api.register_hypothesis(pe_id, agent_a, text)
    assert first["already_existed"] is False

    second_resp = api.post_hypothesis(pe_id, text, agent_id=agent_b)
    second_resp.raise_for_status()
    second = second_resp.json()
    assert second["id"] == first["id"], "re-registering identical text returned a different id"
    assert second["already_existed"] is True


def test_finding_is_visible_to_a_different_agent(api, experiment, run_id, deadline):
    agent_a = make_agent(api, run_id, "dedup-a")
    agent_b = make_agent(api, run_id, "dedup-b")
    pe_id = experiment("hypothesis-dedup-findings", [(agent_a, None, "guaranteed"), (agent_b, None, "guaranteed")], budget=1.0)

    text = "Higher learning rate converges faster to a better task_success_rate"
    hyp = api.register_hypothesis(pe_id, agent_a, text)
    job = api.submit_job(pe_id, agent_a, hours=0.02, hyp_id=hyp["id"])
    _completed(api, job, deadline)

    summary_text = "Achieved 0.81 val_accuracy -- e2e cross-agent findings-visibility coverage"
    api.file_finding(job, summary_text)

    hyp_view = api.hypothesis(hyp["id"])
    findings = hyp_view.get("findings") or []
    assert any(summary_text in (f.get("summary") or "") for f in findings), (
        "finding filed by agent_a was not present in the hypothesis's shared findings list"
    )


def test_human_idea_joins_the_shared_pool_owning_nothing(api, experiment, run_id, deadline):
    agent_a = make_agent(api, run_id, "dedup-a")
    agent_b = make_agent(api, run_id, "dedup-b")
    pe_id = experiment("hypothesis-dedup-human", [(agent_a, None, "guaranteed"), (agent_b, None, "guaranteed")], budget=1.0)

    human_name = f"dana-ops-{run_id}"
    human_text = "Warmup for the first 500 steps stabilizes task_success_rate at batch size 512"
    human = api.register_human_hypothesis(pe_id, human_name, human_text)

    pool = api.hypotheses(pe_id)
    row = next(h for h in pool if h["id"] == human["id"])
    assert (row.get("source"), row.get("author")) == ("human", human_name)
    assert not row.get("agent_id"), f"human hypothesis has an owning agent: {row.get('agent_id')!r}"

    agent_on_human_resp = api.post_hypothesis(pe_id, human_text, agent_id=agent_b)
    agent_on_human_resp.raise_for_status()
    agent_on_human = agent_on_human_resp.json()
    assert agent_on_human["id"] == human["id"]
    assert agent_on_human["already_existed"] is True

    both_resp = api.post_hypothesis(pe_id, "both-set probe", agent_id=agent_a, author=human_name)
    assert both_resp.status_code == 400
    neither_resp = api.post_hypothesis(pe_id, "neither-set probe")
    assert neither_resp.status_code == 400

    code, human_job = api.submit_job_expect(pe_id, agent_b, hours=0.02, hyp_id=human["id"])
    assert code == 202, "a job against a human-owned hypothesis must be admitted"
    assert api.experiment(human_job)["agent_id"] == agent_b

    _completed(api, human_job, deadline)
    human_finding = f"Warmup held task_success_rate steady -- filed by {agent_b} on the human's idea"
    api.file_finding(human_job, human_finding)
    finding_row = next(
        (f for f in api.hypothesis(human["id"]).get("findings") or [] if human_finding in (f.get("summary") or "")),
        None,
    )
    assert finding_row is not None and finding_row.get("agent_id") == agent_b

    quotas = api.platform_experiment_quotas(pe_id)
    assert len(quotas) == 2
    assert human_name not in [q.get("agent_id") for q in quotas]
    results = api.results(pe_id)
    assert human_name not in str(results)

    human_comment = f"Worth re-running this at the shorter warmup before concluding -- {human_name}"
    # A human may also comment on an agent's hypothesis, same origin rule.
    agent_hyp = api.register_hypothesis(pe_id, agent_a, f"a separate agent claim {run_id}")
    comment_resp = api.post_hypothesis_comment(agent_hyp["id"], human_comment, author=human_name)
    assert comment_resp.status_code == 201
    comment_row = next(
        (c for c in api.hypothesis(agent_hyp["id"]).get("comments") or [] if c.get("text") == human_comment), None
    )
    assert comment_row is not None
    assert (comment_row.get("source"), comment_row.get("author")) == ("human", human_name)

    # Status is the owner's verdict: an unowned (human) row may be settled by any agent...
    status_resp = api.set_hypothesis_status(human["id"], agent_a, "confirmed")
    assert status_resp.status_code == 200
    assert api.hypothesis(human["id"])["status"] == "confirmed"

    # ...but an OWNED row is still its owner's alone.
    owned_status_resp = api.set_hypothesis_status(agent_hyp["id"], agent_b, "refuted")
    assert owned_status_resp.status_code == 403
