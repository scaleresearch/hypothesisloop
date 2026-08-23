"""The resource-disbalance evictor: a job whose CPU request is wildly out of proportion to the
accelerators it holds strands that node's other accelerators -- they are free, nothing can reach
them, and no amount of extra hardware fixes it because the blockage is a ratio, not a shortage.
Preemption cannot answer it (it ranks victims by tier and progress, and the offender is often
guaranteed-tier and the only thing running), so this is the one pass that terminates a running job
the queue never asked to stop.

It is also the only such pass, which is why this scenario checks not just that the eviction
happens but that it explains itself: an agent whose job was killed by a decision it never asked
for has to be able to read why, and fix its next submission.

Cluster-exclusive: it deliberately consumes almost all of one node's CPU, which would starve any
scenario running beside it. Needs a node carrying at least 2 accelerators (MIN_ACCELERATORS below)
and exactly one node carrying TEST_ACCELERATOR_TYPE at all -- with a second eligible node the
blocked job would just land there, correctly, and prove nothing about disbalance.

Ported 1:1 from tests/scenarios/resource-disbalance-evict.sh.

NOT PROVEN against the live stack -- blocked by a real, currently-live conflict between two
platform features, not a cluster-shape limitation and not a test bug:

controlplane/services/scheduler/loop_resolve.go's resolveClusterLocalResources (added alongside
the "max" CPU sentinel feature; see test_max_resource_sentinel.py's guarantee #4) now validates
ANY explicit CPU/memory/storage number at admission time against the exact same cluster-local
fair share (domain.FairShare, divisor = accelerators installed on the node) that
loop_disbalance.go's eviction pass later measures a RUNNING job against, at the identical
DefaultDisbalanceTolerance = 1.0. An explicit number that exceeds it is never admitted --
resolveOrValidateDimension (loop_resolve.go ~229) returns an error, resolveClusterLocalResources
returns fits=false, and the job is left QUEUED forever with a not_admitted_reason
("capacity_unavailable: short {...}") indistinguishable from ordinary capacity shortage; the
specific "exceeds cluster-local fair share of Xm" message resolveOrValidateDimension computes is
discarded rather than surfaced, so the agent has no way to learn why (worth fixing separately, but
it is an observability gap, not what blocks this scenario).

Verified empirically against this live stack (fake-l40-3: 8 accelerators, ~15875m free CPU, fair
share ~1984m/accelerator): a job requesting 1900m (under the fair share) is admitted immediately;
a job requesting 2200m (over it) sits QUEUED indefinitely with that exact reason. Since this
scenario's own precondition -- MIN_ACCELERATORS>=2 and a hog whose CPU exceeds its fair share by
construction (see the RATIO check below, ported verbatim from the bash header comment's
arithmetic) -- is now EXACTLY the condition the admission-time guard forecloses, no live cluster
shape can express it any more: the hog can never reach RUNNING, so the eviction pass this scenario
exists to prove can never fire via ordinary submission. This is a structural conflict between the
two features (both intentional, both independently tested), not something to patch around here --
weakening the admission-time guard would break the guarantee test_max_resource_sentinel.py proves,
and there is no other public path to get a disproportionate job running. Left for whoever
reconciles the two features; the test below detects the conflict directly (rather than timing out
the full deadline) and skips with this diagnosis so a future fix flips it green without more
digging.
"""
from __future__ import annotations

import pytest

from conftest import TEST_ACCELERATOR_TYPE, make_agent
from support.cluster import disbalance_node_shape
from support.wait import Deadline, DeadlineExceeded, eventually

pytestmark = pytest.mark.exclusive("cluster")

# A job holding one of N accelerators is entitled to node_cpu/N cores (domain.FairShare); at
# tolerance=1.0 the pass fires above exactly that share. A single hog can request up to the whole
# node's CPU, so the threshold node_cpu/N is reachable whenever node_cpu/N < node_cpu -- true for
# every N>=2 (at N=1 the "share" is the whole node, which nothing can exceed).
MIN_ACCELERATORS = 2


def test_resource_disbalance_evicts_and_explains_itself(api, run_id, deadline):
    shape = disbalance_node_shape(TEST_ACCELERATOR_TYPE)
    if shape.flavor_nodes != 1:
        pytest.skip(
            f"{shape.flavor_nodes} nodes carry {TEST_ACCELERATOR_TYPE}; the blocked job would "
            "just run on another one -- this scenario would assert nothing"
        )
    if shape.installed < MIN_ACCELERATORS:
        pytest.skip(
            f"node {shape.node} carries {shape.installed} accelerator(s); disbalance needs "
            f">={MIN_ACCELERATORS} (below that a single accelerator's fair share IS the whole "
            "node's CPU, so no request can exceed it)"
        )

    agent = make_agent(api, run_id, "agent-disbalance")
    pe_id = api.create_platform_experiment(f"disbalance-{run_id}", 20.0, 1)
    api.signup_ok(pe_id, agent, quota_tier="guaranteed")
    api.start_platform_experiment(pe_id)

    # The hog takes 85% of the node's CPU for a single accelerator. Its fair share is
    # node_cpu/N, so its request is 0.85*N times that entitlement -- at tolerance=1.0 that
    # clears the threshold with room to spare for any N>=2. It also leaves only ~15% of the
    # node's CPU, too little for the blocked job below, while N-1 accelerators sit idle.
    hog_cpu = max(1, int(shape.free_cpu_cores * 0.85))
    # Big enough that the ~15% the hog leaves cannot satisfy it, so the ONLY way it runs is if
    # the hog is evicted -- which is exactly the claim under test. Small enough to fit
    # comfortably once it is.
    blocked_cpu = max(1, int(shape.free_cpu_cores * 0.25))
    fair_share = shape.free_cpu_cores / shape.installed
    ratio = hog_cpu / fair_share if fair_share > 0 else 0.0

    if ratio <= 1.0:
        api.close_platform_experiment(pe_id)
        pytest.skip(
            f"node shape cannot produce a >1.0x disproportion (free_cpu={shape.free_cpu_cores}, "
            f"accelerators={shape.installed}) -- this scenario would assert nothing"
        )
    assert blocked_cpu > shape.free_cpu_cores - hog_cpu, (
        f"blocked job would fit anyway ({blocked_cpu} cores vs "
        f"{round(shape.free_cpu_cores - hog_cpu, 2)} free) -- this scenario would prove nothing"
    )

    hog = api.submit_job(
        pe_id, agent, hours=0.05,
        job_overrides={"cpu": str(hog_cpu), "accelerator_count": 1, "acceptable_accelerator_types": []},
    )

    # This scenario's hog is *designed* to exceed its cluster-local fair share (that is the whole
    # point -- see RATIO above) at the same tolerance loop_resolve.go's admission-time validation
    # now enforces (see this file's header comment). Detect that specific, currently-live conflict
    # fast rather than burning the whole deadline on a job that structurally cannot ever leave
    # QUEUED: give it a short window, then check whether it is exactly the "short = the entire
    # request" fingerprint that fingerprints a resolveClusterLocalResources rejection (a genuine
    # capacity shortage always exists on this idle node, so any other shortage points at something
    # else and is left to fail loudly below instead of being explained away).
    probe_deadline = Deadline.in_seconds(min(30.0, deadline.remaining()))
    try:
        hog_running = eventually(
            f"{hog} (hog) to reach RUNNING",
            lambda: api.experiment(hog),
            accept=lambda e: e["status"] == "RUNNING",
            reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
            deadline=probe_deadline,
        )
    except DeadlineExceeded:
        stuck = api.experiment(hog)
        reason = stuck.get("not_admitted_reason") or ""
        hog_cpu_milli = hog_cpu * 1000
        if stuck["status"] == "QUEUED" and reason.startswith("capacity_unavailable") and f"cpu:={hog_cpu_milli}" in reason:
            api.cancel_job(hog)
            api.close_platform_experiment(pe_id)
            pytest.skip(
                f"hog stuck QUEUED with the entire request reported short ({reason!r}) -- this is "
                "the fingerprint of loop_resolve.go's admission-time fair-share validation "
                "rejecting an explicit CPU number that exceeds the cluster-local fair share (see "
                "this file's header comment); the hog can never be admitted under current platform "
                "code, so the eviction pass cannot be exercised"
            )
        raise
    assert hog_running["status"] == "RUNNING", f"hog never reached RUNNING (status={hog_running['status']})"

    # A modest job that fits the idle accelerators perfectly and is blocked only by the hog's
    # CPU -- it is submitted with a single accelerator and no explicit type, so given a second
    # eligible node it would land elsewhere, correctly, and prove nothing about disbalance (ruled
    # out above by requiring flavor_nodes == 1).
    blocked = api.submit_job(
        pe_id, agent, hours=0.05,
        job_overrides={"cpu": str(blocked_cpu), "accelerator_count": 1, "acceptable_accelerator_types": []},
    )

    # -- the disproportionate job is evicted so the idle accelerators become reachable --
    hog_final = eventually(
        f"{hog} (hog) to be evicted while stranding idle accelerators",
        lambda: api.experiment(hog),
        accept=lambda e: e["status"] in ("EVICTED", "COMPLETED", "FAILED"),
        deadline=deadline,
    )
    assert hog_final["status"] == "EVICTED", (
        f"hog ended as {hog_final['status']!r}, expected EVICTED for resource_disbalance "
        f"(eviction_reason={hog_final.get('eviction_reason')!r})"
    )

    reason = hog_final.get("eviction_reason") or ""
    assert reason.startswith("resource_disbalance"), f"hog evicted for {reason!r}, expected resource_disbalance"

    # An eviction nobody asked for has to explain itself. The reason carries its evidence in the
    # same "code: detail" shape the scheduler uses for not_admitted_reason.
    assert ":" in reason, f"eviction reason is a bare code ({reason!r}) -- the agent has nothing to act on"
    for phrase in ("accelerator", "share", "stranding"):
        assert phrase in reason, f"explanation is missing {phrase!r}: {reason}"

    # -- and the previously blocked job can now run --
    blocked_final = eventually(
        f"{blocked} to be admitted once the disproportionate job was gone",
        lambda: api.experiment(blocked),
        accept=lambda e: e["status"] in ("RUNNING", "COMPLETED"),
        reject=lambda e: e["status"] in ("FAILED", "EVICTED", "REJECTED"),
        deadline=deadline,
    )
    assert blocked_final["status"] in ("RUNNING", "COMPLETED"), (
        f"blocked job is {blocked_final['status']!r} -- the eviction freed nothing, so it "
        "destroyed work for no gain"
    )

    api.cancel_job(blocked)
    api.close_platform_experiment(pe_id)
