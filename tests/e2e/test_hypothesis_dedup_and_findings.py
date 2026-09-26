"""Two core "shared evidence pool" properties (README: agents register/retrieve equivalent
hypotheses; agents can read every hypothesis, the jobs that tested it, and the findings). Ported
1:1 from tests/scenarios/hypothesis-dedup-and-findings.sh:
  1. registering equivalent hypothesis text twice returns the SAME row (already_existed=true,
     identical id), not a duplicate, and the second caller's links are not applied.
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

import json
from datetime import datetime

import pytest

from conftest import make_agent
from support.api import link
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

    other = api.register_hypothesis(pe_id, agent_b, f"Cosine decay beats step decay {run_id}")
    second_resp = api.post_hypothesis(pe_id, text, agent_id=agent_b, links=[link("builds_on", other["id"])])
    second_resp.raise_for_status()
    second = second_resp.json()
    assert second["id"] == first["id"], "re-registering identical text returned a different id"
    assert second["already_existed"] is True

    # A dedup hit returns the existing node as it was: the links the second caller supplied are not applied.
    out = [(lk["type"], lk["hypothesis_id"]) for lk in api.hypothesis(first["id"])["links"]["out"]]
    assert out == [("builds_on", pe_id)], f"dedup hit changed the existing node's links: {out}"


def test_finding_is_visible_to_a_different_agent(api, experiment, run_id, deadline):
    agent_a = make_agent(api, run_id, "dedup-a")
    agent_b = make_agent(api, run_id, "dedup-b")
    pe_id = experiment("hypothesis-dedup-findings", [(agent_a, None, "guaranteed"), (agent_b, None, "guaranteed")], budget=1.0)

    text = "Higher learning rate converges faster to a better task_success_rate"
    hyp = api.register_hypothesis(pe_id, agent_a, text)
    job = api.submit_job(pe_id, agent_a, hours=0.02, hyp_id=hyp["id"])
    _completed(api, job, deadline)

    summary_text = "Achieved 0.81 val_accuracy -- e2e cross-agent findings-visibility coverage"
    filed = api.hl_expect("job", "summary", job, "--text", summary_text)
    assert filed.returncode == 0 and filed.stdout == '{"status":"ok"}\n', filed
    assert f"hl hypothesis status {hyp['id']} inconclusive, or confirmed|refuted --against CONTROL_JOB" in filed.stderr, (
        f"the summary of a job on the agent's own open hypothesis did not point at the verdict: {filed.stderr}"
    )

    hyp_view = api.hypothesis(hyp["id"])
    findings = hyp_view.get("findings") or []
    assert any(summary_text in (f.get("summary") or "") for f in findings), (
        "finding filed by agent_a was not present in the hypothesis's shared findings list"
    )
    (row,) = api.query_rows(pe_id, "MATCH (h:Hypothesis {id: $id}) RETURN $evidence[h.id] AS e", id=hyp["id"])
    evidence = row["e"]
    assert (evidence["jobs"], evidence["completed"], evidence["findings"], evidence["comments"]) == (1, 1, 1, 0), evidence
    assert datetime.fromisoformat(evidence["last_finding_at"]) == datetime.fromisoformat(findings[0]["created_at"]), evidence


def test_human_idea_joins_the_shared_pool_owning_nothing(api, experiment, run_id, deadline):
    agent_a = make_agent(api, run_id, "dedup-a")
    agent_b = make_agent(api, run_id, "dedup-b")
    pe_id = experiment("hypothesis-dedup-human", [(agent_a, None, "guaranteed"), (agent_b, None, "guaranteed")], budget=1.0)

    human_name = f"dana-ops-{run_id}"
    human_text = "Warmup for the first 500 steps stabilizes task_success_rate at batch size 512"
    # Nothing a request says names who registered it: the field is refused, and the actor is the
    # credential, here the staff token, which names nobody.
    named = api.post("/hypotheses", {"author": human_name, "platform_experiment_id": pe_id, "text": human_text,
                                     "links": [link("builds_on", pe_id)]})
    assert named.status_code == 422, f"a hypothesis stating its author: {named.status_code} {named.text}"
    human = api.register_human_hypothesis(pe_id, human_text)
    assert human["actor"] == {"kind": "staff", "id": "", "name": "staff"} and "author" not in human, human

    pool = api.hypotheses(pe_id)
    row = next(h for h in pool if h["id"] == human["id"])
    assert (row.get("source"), row.get("actor")) == ("human", {"kind": "staff", "id": "", "name": "staff"}) and "author" not in row, row
    assert not row.get("agent_id"), f"human hypothesis has an owning agent: {row.get('agent_id')!r}"

    agent_on_human_resp = api.post_hypothesis(pe_id, human_text, agent_id=agent_b)
    agent_on_human_resp.raise_for_status()
    agent_on_human = agent_on_human_resp.json()
    assert agent_on_human["id"] == human["id"]
    assert agent_on_human["already_existed"] is True
    assert not agent_on_human["agent_id"] and agent_on_human["actor"] == {"kind": "staff", "id": "", "name": "staff"}, "restating a claim changed whose it is"

    # Registered for an agent by another credential: the agent owns it, and who did it is kept.
    for_agent = api.register_hypothesis(pe_id, agent_a, f"registered for the agent {run_id}")
    assert (for_agent["agent_id"], for_agent["source"], for_agent["actor"]) == (agent_a, "agent", {"kind": "staff", "id": "", "name": "staff"}), for_agent

    code, human_job = api.submit_job_expect(pe_id, agent_b, hours=0.02, hyp_id=human["id"])
    assert code == 202, "a job against a human-owned hypothesis must be admitted"
    assert api.experiment(human_job)["agent_id"] == agent_b

    done = _completed(api, human_job, deadline)
    assert done["finished_at"] == done["updated_at"] and "summary" not in done, done
    human_finding = f"Warmup held task_success_rate steady -- filed by {agent_b} on the human's idea"
    api.file_finding(human_job, human_finding)
    # The job read, and so hl job status, carries its own summary and keeps its finish time.
    status = json.loads(api.hl("job", "status", human_job))
    assert (status["summary"], status["finished_at"]) == (human_finding, done["finished_at"]), status
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
    # A caller that is no agent may also comment on an agent's hypothesis, as itself. The note is
    # no agent's, and who wrote it is the credential: here the staff token, which names nobody.
    agent_hyp = api.register_hypothesis(pe_id, agent_a, f"a separate agent claim {run_id}")
    named = api.post(f"/hypotheses/{agent_hyp['id']}/comments", {"author": human_name, "text": human_comment})
    assert named.status_code == 422, f"a comment stating its author: {named.status_code} {named.text}"
    comment_resp = api.post_hypothesis_comment(agent_hyp["id"], human_comment)
    assert comment_resp.status_code == 201
    assert comment_resp.json()["actor"] == {"kind": "staff", "id": "", "name": "staff"}, comment_resp.text
    comment_row = next(
        (c for c in api.hypothesis(agent_hyp["id"]).get("comments") or [] if c.get("text") == human_comment), None
    )
    assert comment_row is not None
    assert (comment_row.get("source"), comment_row.get("agent_id")) == ("human", "") and "author" not in comment_row, comment_row
    assert comment_row["actor"] == {"kind": "staff", "id": "", "name": "staff"}, comment_row
    by_agent = api.post_hypothesis_comment(agent_hyp["id"], f"its owner's note {run_id}", agent_id=agent_a)
    assert by_agent.status_code == 201 and by_agent.json()["agent_id"] == agent_a and by_agent.json()["source"] == "agent", by_agent.text
    assert by_agent.json()["actor"] == {"kind": "staff", "id": "", "name": "staff"}, "a note written for an agent names who wrote it"

    # Status is the owner's verdict: an unowned (human) row may be settled by any agent...
    assert human["status_actor"] is None, "a hypothesis nobody ruled on names who ruled"
    status_resp = api.set_hypothesis_status(human["id"], agent_a, "confirmed")
    assert status_resp.status_code == 200
    settled = api.hypothesis(human["id"])
    # ...and who gave the verdict is the credential that sent it, whichever agent it was given as.
    assert settled["status"] == "confirmed" and settled["status_actor"] == {"kind": "staff", "id": "", "name": "staff"}, settled
    assert status_resp.json()["status_actor"] == {"kind": "staff", "id": "", "name": "staff"} and not settled["agent_id"], status_resp.text
    basis = [c for c in settled["comments"] if c["text"].startswith("Verdict confirmed")]
    assert len(basis) == 1 and basis[0]["agent_id"] == agent_a and basis[0]["actor"] == {"kind": "staff", "id": "", "name": "staff"}, settled["comments"]

    # What the experiment has settled, in one read each: the judged hypothesis with its newest
    # finding and best job, and the count of hypotheses by status.
    (judged,) = api.get_json(f"/platform-experiments/{pe_id}/findings")
    assert (judged["id"], judged["status"], judged["finding"]["summary"]) == (human["id"], "confirmed", human_finding), judged
    metric = api.experiment(human_job).get("metric")
    assert judged.get("best_job") == ({"id": human_job, "metric": metric, "summary": human_finding} if metric else None), judged
    stats = api.get_json(f"/hypotheses/stats?platform_experiment_id={pe_id}")
    assert stats["by_status"]["confirmed"] == 1 and stats["total"] == len(api.hypotheses(pe_id)), stats

    # ...but an OWNED row is still its owner's alone.
    owned_status_resp = api.set_hypothesis_status(agent_hyp["id"], agent_b, "refuted")
    assert owned_status_resp.status_code == 403
    assert api.set_hypothesis_status(agent_hyp["id"], "", "refuted").status_code == 403, "a caller naming no agent ruled on an agent's hypothesis"
    assert api.hypothesis(agent_hyp["id"])["status_actor"] is None
