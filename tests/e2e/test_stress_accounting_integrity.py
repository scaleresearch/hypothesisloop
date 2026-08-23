"""Stress + integrity coverage for the accounting-safety fixes (SYSTEM_EVALUATION_MERGED P0s),
ported 1:1 from tests/scenarios/stress-accounting-integrity.sh:
  - P0-e: a donation transfers exactly once even under a burst of concurrent fulfillments of the
          SAME donation -- the atomic, status-gated FulfillDonationTx must not double-debit the
          donor no matter how many callers race.
  - P0-f: malformed / out-of-range / unknown-target metric samples are rejected at ingestion
          (4xx), never written to the metrics store where they'd poison rankings.
  - P0-d: a job may not reference a hypothesis registered under a different platform experiment
          (cross-program scope contamination).
API-only, parallel-safe (its own agents/platform experiments).
"""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from threading import Barrier

import pytest

from conftest import make_agent

pytestmark = pytest.mark.parallel

CREDITS_WANT = 0.05
CONCURRENT_FULFILLERS = 12


def test_concurrent_fulfillment_of_one_donation_transfers_exactly_once(api, experiment, run_id):
    requester = make_agent(api, run_id, "agent-stress-req")
    donor = make_agent(api, run_id, "agent-stress-donor")
    pe_id = experiment("stress-integrity", [requester, donor], budget=4.0, max_agents=2)

    donor_before = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")
    req_before = api.quota_field(pe_id, requester, "guaranteed_accelerator_hours")

    donation_id = api.create_donation(requester, pe_id, CREDITS_WANT, "concurrent-fulfill stress")

    # Fire N fulfillments of the SAME donation at once, synchronized on a barrier so they actually
    # overlap rather than merely being issued in quick succession.
    barrier = Barrier(CONCURRENT_FULFILLERS)

    def _fulfill(_i: int) -> int:
        barrier.wait()
        return api.fulfill_donation(donation_id, donor).status_code

    with ThreadPoolExecutor(max_workers=CONCURRENT_FULFILLERS) as pool:
        codes = list(pool.map(_fulfill, range(CONCURRENT_FULFILLERS)))

    successes = sum(1 for c in codes if 200 <= c < 300)
    assert successes == 1, (
        f"expected exactly 1 concurrent fulfillment to succeed, got {successes} (codes={codes}) "
        "-- atomic gate leaked"
    )

    donor_after = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")
    req_after = api.quota_field(pe_id, requester, "guaranteed_accelerator_hours")
    donor_delta = round(donor_after - donor_before, 6)
    req_delta = round(req_after - req_before, 6)
    assert donor_delta == -CREDITS_WANT, (
        f"donor debited {donor_delta}, expected -{CREDITS_WANT} -- double transfer under concurrency"
    )
    assert req_delta == CREDITS_WANT, (
        f"requester credited {req_delta}, expected +{CREDITS_WANT} -- conservation broken"
    )


def test_metric_fraction_complete_above_one_rejected(api, run_id):
    unknown_id = f"job-does-not-exist-{run_id}"
    r = api.post_metric(unknown_id, fraction_complete=1.5, metric_value=0.5)
    assert 400 <= r.status_code < 500, (
        f"fraction_complete>1 should be a 4xx rejection, got HTTP {r.status_code}"
    )


def test_metric_fraction_complete_negative_rejected(api, run_id):
    unknown_id = f"job-does-not-exist-{run_id}"
    r = api.post_metric(unknown_id, fraction_complete=-0.1, metric_value=0.5)
    assert 400 <= r.status_code < 500, (
        f"negative fraction_complete should be a 4xx rejection, got HTTP {r.status_code}"
    )


def test_metric_for_unknown_experiment_rejected(api, run_id):
    unknown_id = f"job-does-not-exist-{run_id}"
    r = api.post_metric(unknown_id, fraction_complete=0.5, metric_value=0.5)
    assert 400 <= r.status_code < 500, (
        f"metric for a nonexistent experiment should be a 4xx rejection, got HTTP {r.status_code}"
    )


def test_job_cannot_reference_hypothesis_from_different_platform_experiment(api, experiment, run_id):
    donor = make_agent(api, run_id, "agent-stress-donor")
    pe_id = experiment("stress-integrity-x", [donor], budget=4.0, max_agents=2)
    pe_other = experiment("stress-integrity-other-x", [donor], budget=4.0, max_agents=2)

    # Register a hypothesis scoped to pe_other, then try to submit a job under pe_id referencing
    # it. donor is already signed up to pe_id (above), so a submit under pe_id passes the signup
    # gate and fails specifically on the cross-PE hypothesis scope check -- which is what we want
    # to assert.
    foreign_hyp = api.register_hypothesis(pe_other, donor)["id"]
    code, _job_id = api.submit_job_expect(pe_id, donor, hours=0.02, hyp_id=foreign_hyp)
    assert 400 <= code < 500, (
        f"cross-PE hypothesis reference should be a 4xx rejection, got HTTP {code}"
    )
