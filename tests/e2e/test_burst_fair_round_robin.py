"""Burst-tier admission fairness across agents: an agent with a deep burst queue must not be able
to claim every unit of capacity that frees up in one tick ahead of another agent with only one job
waiting (see interleaveByAgent in controlplane/services/scheduler/loop_sort.go).

One full-node filler job (accelerator_count=8), owned by a THIRD agent so its completion and
summary gate never touch either contender, frees the whole node in a single terminal transition --
both of the node's two half-node (accelerator_count=4) slots become free together, in the same
tick, with no risk of the two slots freeing at different times. The three then-queued jobs are A2,
A3 (agent A, submitted first) and B1 (agent B, submitted last). Plain FIFO/priority-tiebreak order
would admit A2 and A3 together (both older than B1) in that tick, starving B1 entirely.
Round-robin interleaving must instead cap agent A at one of the two slots and admit B1 alongside
it -- whichever of A2/A3 actually wins that one slot (a transient per-job admission failure can
legitimately let A3 go instead of A2; that is not what this scenario checks for).

Ported 1:1 from tests/scenarios/burst-fair-round-robin.sh.

CLUSTER_EXCLUSIVE on the A100 flavor: needs the whole node's capacity accounted for with nothing
else touching this accelerator type concurrently, or "the node has exactly two free half-node
slots" stops being observable. The local dev cluster carries exactly one A100 node
(fake-a100-4, 8 accelerators) -- matching what test_multi_stage_chain.py's header documents about
this scenario and preemption-requeue being the only two A100-exclusive scenarios.

PROVEN against the live stack: 3x solo green (see the port's report).
"""
from __future__ import annotations

import pytest

from conftest import make_agent
from support.wait import Deadline, eventually

pytestmark = pytest.mark.exclusive("a100")

ACCELERATOR_TYPE = "nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe"
HOURS = 0.02

ADMITTED = ("RUNNING", "ADMITTED")
TERMINAL = ("COMPLETED", "FAILED", "EVICTED", "REJECTED")


def _is_admitted(api, job_id: str) -> bool:
    return api.experiment(job_id)["status"] in ADMITTED


@pytest.mark.timeout(300)
def test_burst_fair_round_robin(api, run_id, deadline):
    agent_filler = make_agent(api, run_id, "agent-rr-filler")
    agent_a = make_agent(api, run_id, "agent-rr-a")
    agent_b = make_agent(api, run_id, "agent-rr-b")

    pe_id = api.create_platform_experiment(f"burst-round-robin-{run_id}", 50.0, 3)
    api.signup_and_start(pe_id, [agent_filler, agent_a, agent_b])

    # Every job here is pinned to the one accelerator type whose capacity this scenario accounts
    # for. submit_job's job_overrides with an explicit accelerator_type also drops
    # acceptable_accelerator_types (JobRequest.to_yaml), so a job is never silently satisfied by a
    # cheaper alternate -- mirroring pin_job_flavor in the bash suite.
    def pinned(count: int) -> dict:
        return {"accelerator_type": ACCELERATOR_TYPE, "accelerator_count": count}

    # One filler (a third agent) claims the whole node -- its single completion frees both
    # half-node slots together, in one tick, with no dependence on two separate jobs completing
    # in sync.
    filler = api.submit_job(pe_id, agent_filler, hours=HOURS, tier="burst", job_overrides=pinned(8))
    filler_running = eventually(
        f"{filler} (filler) to claim the node",
        lambda: api.experiment(filler),
        accept=lambda e: e["status"] in ADMITTED,
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert filler_running["status"] in ADMITTED, (
        f"filler never admitted (status={filler_running['status']}); round-robin setup failed"
    )

    # A2 and A3 (agent A) queue strictly before B1 (agent B) -- plain FIFO/tiebreak order is
    # A2, A3, B1.
    a2 = api.submit_job(pe_id, agent_a, hours=HOURS, tier="burst", job_overrides=pinned(4))
    a3 = api.submit_job(pe_id, agent_a, hours=HOURS, tier="burst", job_overrides=pinned(4))
    b1 = api.submit_job(pe_id, agent_b, hours=HOURS, tier="burst", job_overrides=pinned(4))

    # The filler frees the whole node in one transition. It belongs to neither contending agent,
    # so its own summary-gate requirement (a COMPLETED job without a filed summary blocks further
    # admission for that same (agent, platform experiment) pair -- loop_tick.go 3a) never touches
    # agent A or agent B; only file it so the filler's own agent doesn't matter for cleanup.
    filler_final = eventually(
        f"{filler} (filler) to reach a terminal state",
        lambda: api.experiment(filler),
        accept=lambda e: e["status"] in TERMINAL,
        deadline=deadline,
    )
    assert filler_final["status"] == "COMPLETED", (
        f"filler did not complete cleanly (status={filler_final['status']}) -- setup did not go "
        "as planned"
    )
    api.file_finding(filler)

    # The property under test: of the two slots that free together, agent A may take at most one
    # (whichever of A2/A3 actually claims it -- a transient per-job admission failure legitimately
    # lets A3 go instead of A2, and that is not a fairness violation) and agent B's only queued
    # job must be among the two admitted. The actual violation this guards against is agent A
    # taking BOTH slots (A2 and A3 both admitted) while B1 is left waiting -- that is queue-depth
    # monopolization, the thing interleaveByAgent exists to prevent.
    #
    # Conclusive as soon as either: agent A already has both slots (violation, no point waiting
    # longer), or B1 is admitted (agent A capped at one slot -- success, whichever of A2/A3 it
    # was). eventually() has no "stop on either of two conditions" mode built in, so poll the
    # three flags directly against the shared deadline.
    def race_state():
        a2_admitted = _is_admitted(api, a2)
        a3_admitted = _is_admitted(api, a3)
        b1_admitted = _is_admitted(api, b1)
        return a2_admitted, a3_admitted, b1_admitted

    try:
        a2_admitted, a3_admitted, b1_admitted = eventually(
            "the node's two freed slots to be claimed (agent A capped at one, or agent A takes both)",
            race_state,
            accept=lambda s: (s[0] + s[1]) >= 2 or s[2],
            deadline=deadline,
        )
    finally:
        # Whichever of A2/A3 lost the race is left QUEUED forever -- closing the platform
        # experiment (the fixture's own teardown) flips its own status but does not cancel jobs
        # still queued under it (PlatformExperimentsService.Close), so a losing job here would
        # otherwise sit QUEUED past this test and contend for real A100 capacity in every later
        # exclusive("a100") run. Cancel every non-admitted contender regardless of outcome.
        for j in (a2, a3, b1):
            if not _is_admitted(api, j) and api.experiment(j)["status"] not in TERMINAL:
                api.cancel_job(j)

    if b1_admitted and (a2_admitted + a3_admitted) <= 1:
        return  # round-robin bounded agent A's queue-depth advantage

    if a2_admitted and a3_admitted:
        # Two different states produce this, and only one of them is a fairness bug. The
        # scheduler already grants A2-vs-A3 the same caveat: a job that loses its reservation to
        # a concurrent one is skipped and the next in interleaved order takes the slot. When that
        # job is B1, agent A ends up with both slots without ever having been ordered ahead of
        # B1.
        #
        # not_admitted_reason separates them exactly (see notAdmittedReasonFor): "outranked" means
        # B1 was behind both of A's jobs in admission order -- the monopolization this scenario
        # exists to catch. Anything else means B1 was ordered fine and lost its slot to a
        # transient conflict, which says nothing about interleaving.
        b1_reason = api.experiment(b1).get("not_admitted_reason") or ""
        assert not b1_reason.startswith("outranked"), (
            f"A2 and A3 (both agent A) were admitted together and B1 was {b1_reason!r} -- burst "
            "admission is not interleaving across agents (regression in interleaveByAgent?)"
        )
        return  # agent A took both slots only because B1 lost its own reservation, not ordering

    pytest.fail(
        f"unexpected admission outcome: A2={int(a2_admitted)} B1={int(b1_admitted)} "
        f"A3={int(a3_admitted)} -- investigate admission accounting"
    )
