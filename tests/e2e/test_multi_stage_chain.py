"""A chain of jobs: one stage writes durable data, the next one reads it back. Ported 1:1 from
tests/scenarios/multi-stage-chain.sh.

No other scenario submits a chain -- test_job_lifecycle.py follows a single job end to end, and
nothing else covers parent_id plus data handed from one stage to the next. What is asserted here:

  1. stage A, a GROUPED job, writes a checkpoint under $HYPOTHESISLOOP_DATA_URI and completes.
     Grouped deliberately: groups and cross-stage data are two features that have to COMPOSE, and
     proving each alone would not say that the pair works.
  2. stage B, single-node, parent_id A, finds that checkpoint under $HYPOTHESISLOOP_DATA_SHARED and
     reports its contents as a metric -- data crossing a job boundary, end to end.
  3. GET /experiments/{id}/lineage returns the chain.
  4. a write by B into A's prefix is REFUSED by the store, while the very same client, holding the
     very same credentials, reads A's object and writes its own prefix successfully.
  5. GET /experiments/{id}/data lists what A wrote, and returns an empty array -- not an error --
     for a job that wrote nothing.

Nothing here asserts where either stage ran, on purpose. Data is addressed, not attached: if a
later stage had to land where an earlier one did, adding a cluster would make the platform worse
rather than bigger. A scenario that pinned placement would encode the opposite design.

Split in two: the chain itself (A -> B, lineage) is one test function because a chain is serial by
construction -- stage B cannot be submitted until stage A has completed and left its checkpoint
behind, so splitting it would mean paying for two admit-run-settle cycles twice, not once each.
"A completed job that wrote nothing lists as an empty array, not an error" is independently
diagnosable with a single job and no chain at all, so it gets its own (much cheaper) test.

tests/run.sh's SLOW_TESTS carries the comment for why: "A chain is serial by definition... That is
longer than the shared ceiling by construction, not by accident." multi-stage-chain is NOT in
CLUSTER_EXCLUSIVE (confirmed against tests/run.sh), so this stays `parallel` -- it owns only its
own UUID-namespaced PE/agents and at most 1-2 A100s at a time, tolerant of queueing -- with `slow`
added to the chain test for the same reason run.sh keeps it out of the default fast loop.
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import eventually

TERMINAL = {"COMPLETED", "FAILED", "EVICTED", "REJECTED"}

# A100. The suite runs its slow group concurrently and this scenario is in it (see run.sh): of the
# slow group, distributed-jobs holds H100 and everything else runs on the TEST_ACCELERATOR_TYPE
# default (L40), so A100 is the one flavor nothing else in the group competes for. The two
# scenarios that do use A100 -- preemption-requeue and burst-fair-round-robin -- are
# CLUSTER_EXCLUSIVE and run in their own phase, after this one has finished.
ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"

# The estimate the cost/quota arithmetic is priced against; each stage's REAL runtime is set
# separately and kept short, because a chain is serial -- every second stage A runs is a second
# stage B has not started yet, and nothing in this scenario is proven better by a longer run.
JOB_HOURS = 0.02
RUN_SECONDS = 10

# Every metric this scenario reads has to be declared, and each is a fact only the job itself can
# report: what it found in the parent's checkpoint, and what the store answered when it tried to
# write where it may not. val_accuracy stays the ranking metric, as in every other scenario.
CHAIN_METRICS = [
    {"key": "val_accuracy", "direction": "maximize"},
    {"key": "stage_rank", "direction": "maximize"},
    {"key": "checkpoint_write_status", "direction": "maximize"},
    {"key": "checkpoint_value", "direction": "maximize"},
    {"key": "session_token_present", "direction": "maximize"},
    {"key": "shared_read_status", "direction": "maximize"},
    {"key": "own_prefix_write_status", "direction": "maximize"},
    {"key": "foreign_write_status", "direction": "maximize"},
]

# The value stage A writes and stage B must report back. Arbitrary, and that is the point: it
# exists nowhere in the platform, so the only way B can report it is by reading A's bytes.
CHECKPOINT_VALUE = 4242


def _wait_completed(api, deadline, job_id, description):
    exp = eventually(
        description,
        lambda: api.experiment(job_id),
        accept=lambda e: e["status"] == "COMPLETED",
        reject=lambda e: e["status"] in (TERMINAL - {"COMPLETED"}),
        deadline=deadline,
    )
    return exp


@pytest.mark.parallel
def test_completed_job_that_wrote_nothing_lists_as_empty_data(api, experiment, run_id, deadline):
    # Empty and error are different answers, and only one of them is right: keeping nothing is the
    # ordinary case for most jobs, so a 404 or a 500 here would make every such job look broken.
    agent = make_agent(api, run_id, "chain-nodata")
    pe_id = experiment("multi-stage-chain-nodata", [agent], budget=1.0)

    job_id = api.submit_job(
        pe_id, agent, hours=JOB_HOURS,
        job_overrides={"env": {"HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS)}, "accelerator_type": ACCELERATOR_TYPE},
    )

    exp = _wait_completed(api, deadline, job_id, "the no-data job to complete")
    api.file_finding(job_id, "multi-stage chain: a completed job that kept nothing.")

    resp = api.experiment_data(job_id)
    assert resp.status_code == 200 and resp.json() == [], (
        f"GET /experiments/{job_id}/data returned HTTP {resp.status_code} with body {resp.text!r}, "
        "want 200 and []"
    )


@pytest.mark.parallel
@pytest.mark.slow
def test_grouped_stage_writes_checkpoint_read_by_child_with_scoped_credentials(api, experiment, run_id, deadline):
    agents = [make_agent(api, run_id, f"chain-{role}") for role in ("write", "read")]
    agent_a, agent_b = agents
    pe_id = experiment("multi-stage-chain", agents, budget=1.0, report_interval_seconds=10, metrics=CHAIN_METRICS)

    # ------------------------------------------------------------------------------------------
    # Stage A: a grouped job writes a checkpoint to its own prefix
    # ------------------------------------------------------------------------------------------
    # Stage A's shape. Grouped, so the top-level per-node fields job.yaml carries must be DELETED
    # (None pops the key, mirroring mk_body.py) -- domain.JobSpec.ValidateGroups rejects a spec
    # that states a thing twice. spread_across_hosts is off because this is a 3-node job and
    # nothing here is about placement; leaving the default on would make the chain's first stage
    # depend on how many hosts advertise A100, which is exactly the coupling this scenario exists
    # to say does not matter.
    job_a = api.submit_job(
        pe_id, agent_a, hours=JOB_HOURS,
        job_overrides={
            "command": ["python", "data_stage.py"],
            "max_retries": 0,
            "cpu": None, "memory": None, "storage": None, "accelerator_count": None,
            "accelerator_type": ACCELERATOR_TYPE,
            "topology": {"spread_across_hosts": False},
            "groups": [
                {"name": "writer", "replicas": 1, "cpu": "250m", "memory": "128Mi", "storage": "512Mi", "accelerator_count": 1},
                {"name": "helper", "replicas": 2, "cpu": "100m", "memory": "64Mi", "storage": "256Mi", "accelerator_count": 0},
            ],
            "env": {
                "HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS),
                "STAGE_MODE": "write",
                "CHECKPOINT_VALUE": str(CHECKPOINT_VALUE),
            },
        },
    )

    exp_a = _wait_completed(api, deadline, job_a, "grouped stage A to complete")
    assert exp_a["status"] == "COMPLETED", (
        f"stage A did not complete (status={exp_a['status']}, "
        f"eviction_reason={exp_a.get('eviction_reason')}, not_admitted_reason={exp_a.get('not_admitted_reason')})"
    )
    api.file_finding(job_a, "multi-stage chain: grouped stage A wrote its checkpoint.")

    # Three nodes, three DISTINCT global ranks. A grouped pod is never handed RANK -- a job-global
    # rank is the group's offset plus the pod's index within its group, and Kubernetes cannot add
    # -- so this number is the workload adding the two terms the platform published. Were the
    # group-local index used instead, the helper group would report ranks 0 and 1 again and this
    # count would be 2.
    ranks = api.metric_values(job_a, "stage_rank")
    distinct_ranks = set(ranks)
    assert len(distinct_ranks) == 3, (
        f"stage A reported {len(distinct_ranks)} distinct rank(s) {sorted(distinct_ranks)}, want 3 "
        "-- two pods in different groups share a rank"
    )
    assert max(distinct_ranks) == pytest.approx(2.0, abs=0.001), (
        f"highest reported rank is {max(distinct_ranks)}, want 2 -- ranks should be 0..2 rather "
        "than three copies of one group's"
    )

    # ------------------------------------------------------------------------------------------
    # GET /experiments/{id}/data -- what a job left behind
    # ------------------------------------------------------------------------------------------
    resp = api.experiment_data(job_a)
    objects = resp.json() if resp.status_code == 200 else None
    assert resp.status_code == 200 and objects is not None and len(objects) == 3, (
        f"GET /experiments/{job_a}/data returned HTTP {resp.status_code} with "
        f"{len(objects) if objects is not None else '<not a list>'} object(s), want 200 and 3"
    )
    # Listed live from the store, so the keys are the real ones the job wrote, not a copy the
    # control plane kept -- a checkpoint nobody wrote cannot appear here.
    keys = [o.get("key") for o in objects]
    rank0_keys = [k for k in keys if (k or "").endswith("checkpoint-rank0.txt")]
    assert len(rank0_keys) == 1, (
        f"stage A's listing has {len(rank0_keys)} rank-0 checkpoint(s), want 1: {keys}"
    )

    # ------------------------------------------------------------------------------------------
    # Stage B: parent_id A, reads A's checkpoint back
    # ------------------------------------------------------------------------------------------
    # parent_id is metadata about how a result was derived, never part of how the job executes, so
    # it rides in the request's metadata rather than its job spec.
    job_b = api.submit_job(
        pe_id, agent_b, hours=JOB_HOURS,
        job_overrides={
            "command": ["python", "data_stage.py"],
            "max_retries": 0,
            "accelerator_type": ACCELERATOR_TYPE,
            "accelerator_count": 1,
            "env": {
                "HYPOTHESISLOOP_DURATION_SECONDS": str(RUN_SECONDS),
                "STAGE_MODE": "read",
                "STAGE_PARENT_ID": job_a,
            },
        },
        metadata_overrides={"parent_id": job_a},
    )

    exp_b = _wait_completed(api, deadline, job_b, "stage B to complete")
    assert exp_b["status"] == "COMPLETED", (
        f"stage B did not complete (status={exp_b['status']}, eviction_reason={exp_b.get('eviction_reason')})"
    )
    api.file_finding(job_b, "multi-stage chain: stage B read its parent's checkpoint.")

    # The whole of "data crosses the job boundary" in one number: 4242 exists nowhere in the
    # platform's own records, so B can only report it by having read the bytes A wrote -- in
    # another agent's prefix, from wherever it was placed, over an address rather than a volume.
    value = api.metric_max(job_b, "checkpoint_value")
    assert value is not None, "stage B never reported checkpoint_value -- it completed without reading its parent's data"
    assert value == pytest.approx(float(CHECKPOINT_VALUE), abs=0.001), (
        f"stage B reported checkpoint_value={value}, want {CHECKPOINT_VALUE} -- it read something else"
    )

    # --- the write into A's prefix, and why its refusal means what it says ---
    # Three facts have to hold before the refusal is evidence of anything. Each is reported by the
    # same process, using one client and one set of credentials:
    #   1. the job holds a session token at all. Without one it is not running under a scoped
    #      session, and the store would refuse a cross-prefix write for an entirely different
    #      reason -- the assertion would pass while proving nothing.
    #   2. that client CAN read A's object across prefixes: the endpoint, the signature and the
    #      credentials all work, so a later refusal is not a broken client.
    #   3. that client CAN write -- in its own prefix. So the refusal is about WHERE, not about
    #      whether this job may write at all.
    # Only then does 403 on A's prefix mean the session's policy is what stopped it.
    token = api.metric_max(job_b, "session_token_present")
    assert token == pytest.approx(1.0, abs=0.001), (
        "stage B did not report a session token -- every credential assertion below would be about the wrong thing"
    )
    read_status = api.metric_max(job_b, "shared_read_status")
    assert read_status == pytest.approx(200.0, abs=0.001), (
        f"cross-prefix read returned {read_status}, want 200 -- the same client should read A's object across prefixes"
    )
    own_write = api.metric_max(job_b, "own_prefix_write_status")
    assert own_write == pytest.approx(200.0, abs=0.001), (
        f"own-prefix write returned {own_write}, want 200 -- this job may write, somewhere"
    )
    foreign = api.metric_max(job_b, "foreign_write_status")
    assert foreign is not None, "stage B never reported foreign_write_status -- the refusal was never attempted"
    assert foreign == pytest.approx(403.0, abs=0.001), (
        f"a write into another job's prefix answered {foreign}, want 403 -- a 200 means an agent "
        "can overwrite another's evidence"
    )

    # ------------------------------------------------------------------------------------------
    # Lineage: what this result was derived from
    # ------------------------------------------------------------------------------------------
    chain = [e["id"] for e in api.lineage(job_b)]
    assert chain == [job_a, job_b], f"lineage of stage B is {chain}, want [{job_a}, {job_b}]"
    # A job with no parent is a chain of one -- the chain endpoint always starts at the row itself,
    # so an empty answer here would mean the row is missing rather than that it has no ancestry.
    root_chain = [e["id"] for e in api.lineage(job_a)]
    assert root_chain == [job_a], f"lineage of stage A is {root_chain}, want [{job_a}]"
