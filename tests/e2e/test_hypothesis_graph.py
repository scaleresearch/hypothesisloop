"""The hypothesis graph (Memgraph-backed): every hypothesis is placed with at least one builds_on
link to the experiment's objective root or to existing hypotheses, links stay inside one platform
experiment, builds_on stays acyclic, only a hypothesis's registering agent links from it, reads
(get, list, graph, read-only Cypher through POST /hypotheses/query) agree with what was written,
and an explicit link is a hypothesis.link watch event, live and on replay.

API-only, parallel-safe (each test owns its platform experiments). The one job submission is
cancelled as soon as it is accepted.
"""
from __future__ import annotations

import json
import os
import subprocess
import time
from pathlib import Path

import pytest

from conftest import make_agent
from support.api import link
from support.wait import Deadline, eventually

pytestmark = pytest.mark.parallel

BUDGET = 1.0
# Must match the text index the control plane creates and documents in POST /hypotheses/query.
# The platform ties the yielded node to $pe. A parenthesis opening with a parameter reads as a node
# pattern and is refused, so the status filter needs none.
TEXT_SEARCH = (
    "CALL text_search.search_all('hypothesis_text', $q) YIELD node, score "
    "WITH node, score WHERE $status = '' OR node.status = $status "
    "RETURN node.id AS id, node.status AS status, score ORDER BY score DESC"
)
CHILDREN = (
    "MATCH (c:Hypothesis)-[:BUILDS_ON]->(p:Hypothesis {id: $id}) "
    "RETURN c.id AS id ORDER BY id"
)
# The objective is a node of its own label, which the platform ties to $pe by its id.
OBJECTIVE_CHILDREN = "MATCH (c:Hypothesis)-[:BUILDS_ON]->(p:Objective) RETURN c.id AS id ORDER BY id"


def _out(hyp: dict) -> set[tuple[str, str]]:
    return {(lk["type"], lk["hypothesis_id"]) for lk in hyp["links"]["out"]}


def _in(hyp: dict) -> set[tuple[str, str]]:
    return {(lk["type"], lk["hypothesis_id"]) for lk in hyp["links"]["in"]}


def _out_of_response(resp: dict) -> set[tuple[str, str]]:
    return {(lk["type"], lk["hypothesis_id"]) for lk in resp["links"]}


@pytest.fixture
def graph_pe(api, experiment, run_id):
    agent_a = make_agent(api, run_id, "graph-a")
    agent_b = make_agent(api, run_id, "graph-b")
    pe_id = experiment("hypothesis-graph", [agent_a, agent_b], budget=BUDGET)
    return pe_id, agent_a, agent_b


def test_registration_requires_placement(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    parent = api.register_hypothesis(pe_id, agent_a, f"placed parent {run_id}")

    refused = {
        "no links": [],
        "only a related link": [link("related", parent["id"])],
        "unknown target": [link("builds_on", f"no-such-hypothesis-{run_id}")],
        "root as a contradicts target": [link("builds_on", pe_id), link("contradicts", pe_id)],
        "unknown link type": [link("extends", parent["id"])],
    }
    for case, links in refused.items():
        r = api.post_hypothesis(pe_id, f"unplaced {case} {run_id}", agent_id=agent_a, links=links)
        assert r.status_code == 400, f"{case}: got {r.status_code} {r.text}"
        assert r.text.strip(), f"{case}: 400 without a readable message"

    proc = api.hl_expect("hypothesis", "submit", "--agent", agent_a, "--platform-experiment", pe_id, "--text", f"cli unplaced {run_id}")
    assert proc.returncode == 2, f"hl hypothesis submit without --builds-on exited {proc.returncode}"
    assert "--builds-on root" in proc.stderr, f"usage error does not say how to place the idea: {proc.stderr}"


def test_tree_combination_and_cross_links(api, graph_pe, run_id):
    pe_id, agent_a, agent_b = graph_pe
    root_child = api.register_hypothesis(pe_id, agent_a, f"warmup helps {run_id}")
    assert root_child["already_existed"] is False
    assert _out_of_response(root_child) == {("builds_on", pe_id)}

    grandchild = api.register_hypothesis(
        pe_id, agent_a, f"longer warmup helps more {run_id}", links=[link("builds_on", root_child["id"])],
        distinct_from=[root_child["id"]],
    )
    sibling = api.register_hypothesis(pe_id, agent_b, f"higher lr helps {run_id}", distinct_from=[root_child["id"], grandchild["id"]])
    combined = api.register_hypothesis(
        pe_id,
        agent_b,
        f"higher lr only helps with warmup {run_id}",
        links=[
            link("builds_on", root_child["id"]),
            link("builds_on", sibling["id"]),
            link("contradicts", grandchild["id"]),
            link("related", grandchild["id"]),
        ],
        distinct_from=[root_child["id"], sibling["id"]],
    )

    combined_view = api.hypothesis(combined["id"])
    assert _out(combined_view) == {
        ("builds_on", root_child["id"]),
        ("builds_on", sibling["id"]),
        ("contradicts", grandchild["id"]),
        ("related", grandchild["id"]),
    }
    contradicted = next(lk for lk in combined_view["links"]["out"] if lk["type"] == "contradicts")
    assert contradicted["text"] == f"longer warmup helps more {run_id}"
    assert contradicted["status"] == "open"

    root_child_view = api.hypothesis(root_child["id"])
    root_link = root_child_view["links"]["out"][0]
    assert (root_link["hypothesis_id"], root_link["text"], root_link["status"]) == (pe_id, "", "")
    assert _in(root_child_view) == {("builds_on", grandchild["id"]), ("builds_on", combined["id"])}
    assert _in(api.hypothesis(grandchild["id"])) == {("contradicts", combined["id"]), ("related", combined["id"])}

    rows = {h["id"]: h for h in api.hypotheses(pe_id)}
    assert rows[root_child["id"]]["child_count"] == 2
    assert rows[sibling["id"]]["child_count"] == 1
    assert rows[grandchild["id"]]["child_count"] == 0
    assert rows[root_child["id"]]["builds_on"] == [pe_id]
    assert sorted(rows[combined["id"]]["builds_on"]) == sorted([root_child["id"], sibling["id"]])

    assert [r["id"] for r in api.query_rows(pe_id, OBJECTIVE_CHILDREN)] == sorted([root_child["id"], sibling["id"]])
    assert [r["id"] for r in api.query_rows(pe_id, CHILDREN, id=root_child["id"])] == sorted([grandchild["id"], combined["id"]])

    graph = api.hypothesis_graph(pe_id)
    assert graph["root"] == pe_id
    node_ids = {n["id"] for n in graph["nodes"]}
    assert node_ids == {root_child["id"], grandchild["id"], sibling["id"], combined["id"]}
    edges = {(e["from"], e["to"], e["type"]) for e in graph["edges"]}
    assert edges == {
        (root_child["id"], pe_id, "builds_on"),
        (sibling["id"], pe_id, "builds_on"),
        (grandchild["id"], root_child["id"], "builds_on"),
        (combined["id"], root_child["id"], "builds_on"),
        (combined["id"], sibling["id"], "builds_on"),
        (combined["id"], grandchild["id"], "contradicts"),
        (combined["id"], grandchild["id"], "related"),
    }
    by_id = {n["id"]: n for n in graph["nodes"]}
    assert by_id[root_child["id"]]["child_count"] == rows[root_child["id"]]["child_count"]


def test_link_rules(api, graph_pe, experiment, run_id):
    pe_id, agent_a, agent_b = graph_pe
    a = api.register_hypothesis(pe_id, agent_a, f"dropout reduces overfitting {run_id}")
    b = api.register_hypothesis(pe_id, agent_a, f"weight decay stabilizes long runs {run_id}", links=[link("builds_on", a["id"])])
    theirs = api.register_hypothesis(pe_id, agent_b, f"larger batches need warmup {run_id}")

    r = api.post_hypothesis_link(b["id"], "related", theirs["id"], agent_id=agent_a)
    assert r.status_code == 201, r.text
    assert ("related", theirs["id"]) in _out(api.hypothesis(b["id"]))
    again = api.post_hypothesis_link(b["id"], "related", theirs["id"], agent_id=agent_a)
    assert again.status_code == 201, again.text
    assert sum(1 for lk in api.hypothesis(b["id"])["links"]["out"] if lk["type"] == "related") == 1, "duplicate link stored twice"

    cycle = api.post_hypothesis_link(a["id"], "builds_on", b["id"], agent_id=agent_a)
    assert cycle.status_code == 400, f"builds_on cycle accepted: {cycle.status_code} {cycle.text}"
    self_link = api.post_hypothesis_link(a["id"], "related", a["id"], agent_id=agent_a)
    assert self_link.status_code == 400, f"self link accepted: {self_link.status_code}"
    unknown = api.post_hypothesis_link(a["id"], "related", f"no-such-hypothesis-{run_id}", agent_id=agent_a)
    assert unknown.status_code == 400, f"link to an unknown target accepted: {unknown.status_code}"

    not_owner = api.post_hypothesis_link(a["id"], "contradicts", theirs["id"], agent_id=agent_b)
    assert not_owner.status_code == 403, f"agent_b linked from agent_a's hypothesis: {not_owner.status_code}"
    assert ("contradicts", theirs["id"]) not in _out(api.hypothesis(a["id"]))

    human = api.register_human_hypothesis(pe_id, f"human idea {run_id}")
    by_anyone = api.post_hypothesis_link(human["id"], "related", theirs["id"], agent_id=agent_b)
    assert by_anyone.status_code == 201, f"a human hypothesis must be linkable by any agent: {by_anyone.text}"

    other_pe = experiment("hypothesis-graph-other", [agent_a], budget=BUDGET)
    foreign = api.register_hypothesis(other_pe, agent_a, f"foreign idea {run_id}")
    cross_link = api.post_hypothesis_link(a["id"], "related", foreign["id"], agent_id=agent_a)
    assert cross_link.status_code == 400, f"cross-experiment link accepted: {cross_link.status_code}"
    cross_register = api.post_hypothesis(pe_id, f"built on a foreign idea {run_id}", agent_id=agent_a, links=[link("builds_on", foreign["id"])])
    assert cross_register.status_code == 400, f"cross-experiment builds_on accepted: {cross_register.status_code}"
    foreign_root = api.post_hypothesis(pe_id, f"built on a foreign root {run_id}", agent_id=agent_a, links=[link("builds_on", other_pe)])
    assert foreign_root.status_code == 400, f"builds_on another experiment's root accepted: {foreign_root.status_code}"


def test_cli_tree_from_one_file_and_reads(api, graph_pe, run_id, tmp_path: Path):
    pe_id, agent_a, _ = graph_pe
    spec = tmp_path / "tree.yaml"
    spec.write_text(
        f"""agent_id: {agent_a}
platform_experiment_id: {pe_id}
links: [{{type: builds_on, to: root}}]
hypotheses:
  - key: base
    text: "two seeds agree within noise {run_id}"
  - text: "the baseline reaches ninety percent {run_id}"
    links: [{{type: builds_on, to: base}}]
"""
    )
    created = [json.loads(line) for line in api.hl("hypothesis", "submit", "--file", str(spec)).splitlines()]
    assert len(created) == 2
    base, child = created
    assert _out(api.hypothesis(child["id"])) == {("builds_on", base["id"])}

    got = json.loads(api.hl("hypothesis", "get", child["id"]))
    assert got["id"] == child["id"] and _out(got) == {("builds_on", base["id"])}

    listed = [json.loads(line) for line in api.hl("hypothesis", "list", "--platform-experiment", pe_id).splitlines()]
    assert {h["id"] for h in listed} >= {base["id"], child["id"]}

    rows = [
        json.loads(line)
        for line in api.hl("hypothesis", "query", CHILDREN, "--platform-experiment", pe_id, "--param", f"id={base['id']}").splitlines()
    ]
    assert rows == [{"id": child["id"]}]

    sibling = json.loads(
        api.hl("hypothesis", "submit", "--agent", agent_a, "--platform-experiment", pe_id,
               "--text", f"cosine schedules beat step schedules {run_id}", "--builds-on", "root", "--related", base["id"])
    )
    api.hl("hypothesis", "link", "--agent", agent_a, "--from", sibling["id"], "--type", "contradicts", "--to", child["id"])
    assert _out(api.hypothesis(sibling["id"])) == {("builds_on", pe_id), ("related", base["id"]), ("contradicts", child["id"])}

    api.hl("hypothesis", "comment", base["id"], "--agent", agent_a, "--text", f"dead end {run_id}")
    api.hl("hypothesis", "status", "--agent", agent_a, "--status", "refuted", "--despite", api.NO_EVIDENCE, base["id"])
    got = json.loads(api.hl("hypothesis", "get", base["id"]))
    # The verdict's basis is kept beside the agent's own note.
    assert got["status"] == "refuted" and [c["text"] for c in got["comments"]] == [
        f"dead end {run_id}", f"Verdict refuted given without a repeated run and a control: {api.NO_EVIDENCE}"]

    opposite = ["hypothesis", "submit", "--agent", agent_a, "--platform-experiment", pe_id,
                "--text", f"seeds disagree beyond noise {run_id}", "--builds-on", base["id"]]
    held = api.hl_expect(*opposite)
    assert held.returncode != 0 and "refuted_parent" in held.stderr and base["id"] in held.stderr, held.stderr
    created = json.loads(api.hl(*opposite, "--contradicts", base["id"], "--distinct-from", f"{child['id']},{sibling['id']}"))
    assert ("contradicts", base["id"]) in _out(api.hypothesis(created["id"]))


def test_registration_waits_until_close_hypotheses_are_told_apart(api, graph_pe, run_id):
    pe_id, agent_a, agent_b = graph_pe
    bs8 = api.register_hypothesis(pe_id, agent_a, f"batch size 8 beats batch size 32 at the same epoch budget {run_id}")
    reworded = f"a batch size of 8 beats 32 within the same epoch budget {run_id}"

    held = api.post_hypothesis(pe_id, reworded, agent_id=agent_b)
    assert held.status_code == 409, f"reworded duplicate: got {held.status_code} {held.text}"
    body = held.json()
    assert [(h["id"], h["reason"]) for h in body["similar"]] == [(bs8["id"], "close_text")], body
    assert body["similar"][0]["builds_on"] == [pe_id] and body["similar"][0]["agent_id"] == agent_a
    assert body["reason"] == "similar_hypotheses" and "distinct_from" in body["message"] and "detail" not in body, body
    assert [n["id"] for n in api.hypothesis_graph(pe_id)["nodes"]] == [bs8["id"]], "a held-back registration created a node"

    submit = ["hypothesis", "submit", "--agent", agent_b, "--platform-experiment", pe_id, "--text", reworded, "--builds-on", "root"]
    cli_held = api.hl_expect(*submit)
    assert cli_held.returncode != 0 and bs8["id"] in cli_held.stderr, cli_held.stderr
    created = json.loads(api.hl(*submit, "--distinct-from", bs8["id"]))
    assert _out(api.hypothesis(created["id"])) == {("builds_on", pe_id)}

    # A refinement reads close to the parent it names: that alone holds nothing back.
    refined = json.loads(api.hl(
        "hypothesis", "submit", "--agent", agent_b, "--platform-experiment", pe_id, "--builds-on", bs8["id"],
        "--text", f"batch size 4 beats batch size 8 at the same epoch budget {run_id}", "--distinct-from", created["id"],
    ))
    assert _out(api.hypothesis(refined["id"])) == {("builds_on", bs8["id"])}


def test_query_text_search_and_status(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    word = f"zq{run_id.replace('-', '')}"
    kept = api.register_hypothesis(pe_id, agent_a, f"gradient clipping {word} stabilizes training")
    refuted = api.register_hypothesis(pe_id, agent_a, f"label smoothing {word} hurts calibration")
    api.register_hypothesis(pe_id, agent_a, f"an unrelated idea about data order {run_id}")
    assert api.set_hypothesis_status(refuted["id"], agent_a, "refuted").status_code == 200

    def search(q: str, status: str = "") -> list[dict]:
        return api.query_rows(pe_id, TEXT_SEARCH, q=q, status=status)

    deadline = Deadline.in_seconds(30)
    hits = eventually(
        "both hypotheses to be found by their shared word",
        lambda: search(word),
        accept=lambda rows: {r["id"] for r in rows} == {kept["id"], refuted["id"]},
        deadline=deadline,
    )
    assert all(isinstance(r["score"], (int, float)) for r in hits)
    assert search(f"clipping {word}")[0]["id"] == kept["id"], "best match is not first"
    assert [r["id"] for r in search(word, status="refuted")] == [refuted["id"]]
    assert [r["id"] for r in search(word, status="open")] == [kept["id"]]


def test_query_value_mapping(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    parent = api.register_hypothesis(pe_id, agent_a, f"momentum speeds convergence {run_id}")
    child = api.register_hypothesis(pe_id, agent_a, f"label noise caps accuracy {run_id}", links=[link("builds_on", parent["id"])])

    rows = api.query_rows(
        pe_id,
        "MATCH (c:Hypothesis {id: $id})-[r:BUILDS_ON]->(p:Hypothesis) RETURN c, r, p.id AS parent",
        id=child["id"],
    )
    assert len(rows) == 1
    node, rel = rows[0]["c"], rows[0]["r"]
    assert "Hypothesis" in node["_labels"]
    assert (node["id"], node["platform_experiment_id"], node["agent_id"]) == (child["id"], pe_id, agent_a)
    assert (rel["_type"], rel["_from"], rel["_to"], rel["agent_id"]) == ("BUILDS_ON", child["id"], parent["id"], agent_a)
    assert rows[0]["parent"] == parent["id"]

    newer = "MATCH (h:Hypothesis {id: $id}) WHERE h.created_at >= datetime($since) RETURN h.id AS id"
    assert api.query_rows(pe_id, newer, id=child["id"], since=node["created_at"]) == [{"id": child["id"]}]


def test_query_ranks_by_evidence(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    quiet = api.register_hypothesis(pe_id, agent_a, f"weight decay matters past ten epochs {run_id}")
    discussed = api.register_hypothesis(pe_id, agent_a, f"gradient clipping hides divergence {run_id}")
    api.post_hypothesis_comment(discussed["id"], "seen on two seeds", agent_id=agent_a).raise_for_status()

    ranked = (
        "MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.id IN $ids "
        "RETURN h.id AS id, coalesce($evidence[h.id].comments, 0) AS comments, coalesce($evidence[h.id].jobs, 0) AS jobs "
        "ORDER BY comments DESC"
    )
    assert api.query_rows(pe_id, ranked, ids=[quiet["id"], discussed["id"]]) == [
        {"id": discussed["id"], "comments": 1, "jobs": 0},
        {"id": quiet["id"], "comments": 0, "jobs": 0},
    ]
    assert api.query(pe_id, "RETURN 1", evidence={}).status_code == 400


def test_query_reads_one_platform_experiment(api, graph_pe, experiment, run_id):
    pe_id, agent_a, _ = graph_pe
    other_pe = experiment("hypothesis-graph-scope", [agent_a], budget=BUDGET)
    mine = api.register_hypothesis(pe_id, agent_a, f"scoped claim {run_id}")
    theirs = api.register_hypothesis(other_pe, agent_a, f"foreign claim {run_id}")

    ids = [mine["id"], theirs["id"]]
    assert api.query_rows(pe_id, "MATCH (h:Hypothesis {id: $id}) RETURN h.id, h.text", id=theirs["id"]) == []
    assert api.query_rows(pe_id, "MATCH (h:Hypothesis) WHERE h.id IN $ids RETURN h.id AS id", ids=ids) == [{"id": mine["id"]}]
    assert api.query_rows(pe_id, "MATCH (h:Hypothesis) WHERE h.id IN $ids RETURN count(h) AS n", ids=ids) == [{"n": 1}]
    for cypher in (
        "MATCH (h) WHERE h.id IN $ids RETURN h.id, h.text",
        "MATCH (h:Hypothesis {platform_experiment_id: $other}) RETURN h.text",
    ):
        refused = api.query(pe_id, cypher, ids=ids, other=other_pe)
        assert refused.status_code == 400, f"{cypher!r} answered {refused.status_code}: {refused.text}"


def test_query_is_read_only_and_bounded(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    text = f"must survive a write query {run_id}"
    hyp = api.register_hypothesis(pe_id, agent_a, text)
    planted = f"planted-{run_id}"

    writes = [
        ("MATCH (h:Hypothesis {id: $id}) SET h.text = 'vandalized' RETURN h.text", {"id": hyp["id"]}),
        ("CREATE (h:Hypothesis {id: $planted, platform_experiment_id: $pe, text: 'planted'}) RETURN h.id", {"planted": planted}),
        ("MERGE (h:Hypothesis {id: $planted}) RETURN h.id", {"planted": planted}),
        ("MATCH (h:Hypothesis {id: $id}) DETACH DELETE h", {"id": hyp["id"]}),
        ("CREATE INDEX ON :Hypothesis(agent_id)", {}),
    ]
    for cypher, params in writes:
        r = api.query(pe_id, cypher, **params)
        assert r.status_code in (200, 400), f"{cypher!r}: {r.status_code} {r.text}"

    assert api.hypothesis(hyp["id"])["text"] == text
    assert _out(api.hypothesis(hyp["id"])) == {("builds_on", pe_id)}
    assert api.query_rows(pe_id, "MATCH (h:Hypothesis {id: $planted}) RETURN count(h) AS n", planted=planted) == [{"n": 0}]
    assert planted not in {h["id"] for h in api.hypotheses(pe_id)}

    bad = api.query(pe_id, "MATCH (h:Hypothesis) RETRN h")
    assert bad.status_code == 400, f"malformed Cypher answered {bad.status_code}"
    assert "mismatched input 'RETRN'" in bad.json().get("message", ""), f"400 without Memgraph's message: {bad.text}"
    # A statement that opens with no read clause never reaches Memgraph.
    unknown = api.query(pe_id, "MATC (h:Hypothesis) RETURN h")
    assert unknown.status_code == 400 and "a query starts with MATCH" in unknown.json().get("message", ""), unknown.text

    # Refused before it reaches the graph, each with its reason: a statement that reads a file on
    # the database's host, and a rune outside printable ASCII anywhere but in a string or a
    # backtick name, where it is text.
    for cypher, reason in (
        ("UNWIND [1] AS i LOAD CSV FROM '/etc/hostname' NO HEADER AS row RETURN row", "LOAD is not available"),
        ("MATCH (h:Hypothesis) RETURN h.id AS id", "printable ASCII"),
        ("MATCH (h:Hypothesis)‐[:BUILDS_ON]->(p:Hypothesis) RETURN h.id", "printable ASCII"),
    ):
        refused = api.query(pe_id, cypher)
        assert refused.status_code == 400 and reason in refused.json()["message"], f"{cypher!r}: {refused.status_code} {refused.text}"
    assert api.query_rows(pe_id, "MATCH (h:Hypothesis {id: $id}) RETURN 'déjà vu' AS said", id=hyp["id"]) == [{"said": "déjà vu"}]

    capped = api.query(pe_id, "UNWIND range(1, 1000) AS i RETURN i")
    assert capped.status_code == 200, capped.text
    body = capped.json()
    assert body["columns"] == ["i"]
    assert body["truncated"] is True
    assert 0 < len(body["rows"]) < 1000

    small = api.query(pe_id, "UNWIND range(1, 3) AS i RETURN i").json()
    assert (small["rows"], small["truncated"]) == ([[1], [2], [3]], False)


@pytest.mark.accelerator
def test_job_against_a_graph_hypothesis_is_accepted(api, graph_pe, run_id):
    pe_id, agent_a, _ = graph_pe
    parent = api.register_hypothesis(pe_id, agent_a, f"residual connections help depth {run_id}")
    child = api.register_hypothesis(pe_id, agent_a, f"mixed precision halves memory {run_id}", links=[link("builds_on", parent["id"])])
    code, job = api.submit_job_expect(pe_id, agent_a, hours=0.02, hyp_id=child["id"])
    assert code == 202, f"a job against a placed grandchild hypothesis was refused ({code})"
    assert api.experiment(job)["hypothesis_id"] == child["id"]
    api.cancel_job(job)


def _start_watch(api, tmp_path: Path, name: str, *args: str) -> tuple[subprocess.Popen, Path, Path]:
    out_path, err_path = tmp_path / f"{name}.jsonl", tmp_path / f"{name}.err"
    with out_path.open("w") as out_f, err_path.open("w") as err_f:
        proc = subprocess.Popen(
            [str(api.hl_bin), "watch", *args], stdout=out_f, stderr=err_f, text=True, env={**os.environ, "API_URL": api.base_url}
        )
    return proc, out_path, err_path


def _events(path: Path, kind: str) -> list[dict]:
    lines = path.read_text().splitlines() if path.exists() else []
    return [e for e in (json.loads(line) for line in lines if line.startswith("{")) if e.get("kind") == kind]


def _stop(proc: subprocess.Popen) -> None:
    if proc.poll() is None:
        proc.kill()
        proc.wait(timeout=5)


def test_watch_delivers_and_replays_hypothesis_link(api, graph_pe, run_id, tmp_path: Path):
    pe_id, agent_a, agent_b = graph_pe
    deadline = Deadline.in_seconds(60)
    proc, out_path, err_path = _start_watch(
        api, tmp_path, "live", "--platform-experiment", pe_id, "--kinds", "hypothesis.new,hypothesis.link", "--follow", "--timeout", "60"
    )
    try:
        eventually("hl watch to connect", err_path.read_text, accept=lambda t: "hl watch: connected" in t, deadline=deadline)

        a = api.register_hypothesis(pe_id, agent_a, f"early stopping saves compute {run_id}")
        b = api.register_hypothesis(pe_id, agent_b, f"augmentation improves recall {run_id}", links=[link("builds_on", a["id"])])
        assert api.post_hypothesis_link(b["id"], "contradicts", a["id"], agent_id=agent_b).status_code == 201

        live = eventually(
            "the explicit link to arrive as hypothesis.link",
            lambda: _events(out_path, "hypothesis.link"),
            accept=lambda evs: any(e["subject"] == b["id"] for e in evs),
            deadline=deadline,
        )
        news = {e["subject"] for e in _events(out_path, "hypothesis.new")}
    finally:
        _stop(proc)

    assert {a["id"], b["id"]} <= news, f"hypothesis.new missing for a registered node: {news}"
    assert [(e["subject"], e["value"], e["detail"]) for e in live] == [(b["id"], "contradicts", a["id"])], (
        f"expected exactly the explicit link; links created with a hypothesis must not emit hypothesis.link: {live}"
    )
    cursor, position = live[0]["cursor"], live[0]["position"]

    replay_started = time.monotonic()
    replay = api.hl_expect(
        "watch", "--platform-experiment", pe_id, "--kinds", "hypothesis.link", "--since", "1", "--timeout", "5", timeout=20
    )
    assert replay.returncode == 0, f"replay watch exited {replay.returncode}: {replay.stderr}"
    replayed = [json.loads(line) for line in replay.stdout.splitlines() if line.startswith("{")]
    assert [(e["kind"], e["subject"], e["value"], e["detail"], e["position"]) for e in replayed] == [
        ("hypothesis.link", b["id"], "contradicts", a["id"], position)
    ], f"replay from the start did not return exactly the explicit link: {replayed}"
    assert time.monotonic() - replay_started < 15

    after = api.hl_expect(
        "watch", "--platform-experiment", pe_id, "--kinds", "hypothesis.link", "--since", str(cursor), "--timeout", "3", timeout=15
    )
    assert after.returncode == 0, after.stderr
    # A resume repeats the transaction at its cursor and nothing older, so the link comes back once.
    resent = [json.loads(line) for line in after.stdout.splitlines() if line.startswith("{")]
    assert [e["position"] for e in resent] == [position], (
        f"replay from the link's own cursor returned something other than the link once: {after.stdout}"
    )


def _watch_as(api, agent: str, pe_id: str, state_dir: Path) -> subprocess.CompletedProcess:
    # hl takes who watches and where from its flags or its token, and from no other variable.
    env = {**api.hl_env(), "HL_STATE_DIR": str(state_dir)}
    return subprocess.run([str(api.hl_bin), "watch", "--agent", agent, "--platform-experiment", pe_id, "--timeout", "10"],
                          capture_output=True, text=True, env=env, timeout=30)


def test_agent_watch_carries_others_activity_on_its_hypotheses_and_resumes(api, experiment, run_id, tmp_path: Path):
    agent_a = make_agent(api, run_id, "watch-a")
    agent_b = make_agent(api, run_id, "watch-b")
    pe_id = experiment("agent-watch", [agent_a, agent_b], budget=BUDGET)
    mine = api.register_hypothesis(pe_id, agent_a, f"weight decay 0.1 regularises best {run_id}")
    theirs = api.register_hypothesis(pe_id, agent_b, f"dropout before the head helps {run_id}")
    api.post_hypothesis_comment(mine["id"], f"own note {run_id}", agent_id=agent_a).raise_for_status()
    api.post_hypothesis_comment(theirs["id"], f"unrelated {run_id}", agent_id=agent_a).raise_for_status()
    api.post_hypothesis_comment(mine["id"], f"does it hold at depth 24? {run_id}", agent_id=agent_b).raise_for_status()
    assert api.post_hypothesis_link(theirs["id"], "related", mine["id"], agent_id=agent_b).status_code == 201

    first = _watch_as(api, agent_a, pe_id, tmp_path)
    assert first.returncode == 0, first.stderr
    events = [json.loads(line) for line in first.stdout.splitlines() if line.startswith("{")]
    hypothesis_events = [(e["kind"], e["subject"], e["agent_id"]) for e in events
                         if e["kind"] in ("hypothesis.new", "hypothesis.link", "comment.new", "finding.new")]
    assert sorted(hypothesis_events) == sorted([
        ("comment.new", mine["id"], agent_b),
        ("hypothesis.link", theirs["id"], agent_b),
    ]), f"agent watch did not carry exactly others' activity on its own hypothesis: {first.stdout}"

    api.post_hypothesis_comment(mine["id"], f"and at depth 48? {run_id}", agent_id=agent_b).raise_for_status()
    second = _watch_as(api, agent_a, pe_id, tmp_path)
    assert second.returncode == 0, second.stderr
    resumed = [json.loads(line) for line in second.stdout.splitlines() if line.startswith("{")]
    assert [(e["kind"], e["subject"]) for e in resumed] == [("comment.new", mine["id"])], (
        f"a resumed agent watch repeated or missed events: {second.stdout}"
    )
