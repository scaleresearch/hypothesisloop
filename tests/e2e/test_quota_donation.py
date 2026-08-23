"""Compute-donation flow (README: "agents can donate unused quota to each other") -- ported 1:1
from tests/scenarios/quota-donation.sh. A core cross-agent feature with no prior coverage besides
this. Verifies:
  1. fulfilling a donation actually moves guaranteed AccH from donor to requester (not just flips
     a status flag) -- donor debited, requester credited, by exactly credits_want -- and burst
     headroom (a fixed multiple of guaranteed) follows the transfer, preserving each side's
     burst/guaranteed ratio.
  2. a cancelled request cannot later be fulfilled (status is terminal, not just cosmetic).
  3. a fulfilled request cannot be fulfilled again (double-fulfillment must not double-credit).

API-only, parallel-safe (its own platform experiment, its own agents).
"""
from __future__ import annotations

import pytest

from conftest import make_agent

pytestmark = pytest.mark.parallel


def burst_ratio(guaranteed: float, burst: float) -> float | None:
    return round(burst / guaranteed, 6) if guaranteed > 0 else None


def test_donation_moves_credits_and_preserves_burst_ratio(api, experiment, run_id):
    requester = make_agent(api, run_id, "donation-requester")
    donor = make_agent(api, run_id, "donation-donor")
    pe_id = experiment("quota-donation", [requester, donor], budget=4.0)

    credits_want = 0.05
    donor_guaranteed_before = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")
    requester_guaranteed_before = api.quota_field(pe_id, requester, "guaranteed_accelerator_hours")
    donor_burst_before = api.quota_field(pe_id, donor, "burst_accelerator_hours")
    requester_burst_before = api.quota_field(pe_id, requester, "burst_accelerator_hours")

    donation_id = api.create_donation(requester, pe_id, credits_want, "e2e donation flow coverage")
    assert donation_id

    fulfill_resp = api.fulfill_donation(donation_id, donor)
    assert fulfill_resp.status_code < 300, fulfill_resp.text

    donor_guaranteed_after = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")
    requester_guaranteed_after = api.quota_field(pe_id, requester, "guaranteed_accelerator_hours")
    assert donor_guaranteed_after == pytest.approx(donor_guaranteed_before - credits_want, abs=1e-6)
    assert requester_guaranteed_after == pytest.approx(requester_guaranteed_before + credits_want, abs=1e-6)

    # Burst is a fixed multiple of guaranteed (domain.AllocateQuota), so a transfer that moves only
    # guaranteed leaves the donor holding burst headroom for hours it gave away. Asserted as a
    # preserved ratio rather than an absolute figure, so this does not encode burst_fraction and
    # cannot drift when that setting changes.
    donor_burst_after = api.quota_field(pe_id, donor, "burst_accelerator_hours")
    requester_burst_after = api.quota_field(pe_id, requester, "burst_accelerator_hours")
    assert burst_ratio(donor_guaranteed_after, donor_burst_after) == burst_ratio(donor_guaranteed_before, donor_burst_before)
    assert burst_ratio(requester_guaranteed_after, requester_burst_after) == burst_ratio(
        requester_guaranteed_before, requester_burst_before
    )


def test_refulfilling_an_already_fulfilled_donation_does_not_double_credit(api, experiment, run_id):
    requester = make_agent(api, run_id, "donation-requester")
    donor = make_agent(api, run_id, "donation-donor")
    pe_id = experiment("quota-donation-refulfill", [requester, donor], budget=4.0)

    donation_id = api.create_donation(requester, pe_id, 0.05, "e2e re-fulfill coverage")
    assert api.fulfill_donation(donation_id, donor).status_code < 300
    donor_after = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")

    refulfill_resp = api.fulfill_donation(donation_id, donor)
    donor_after2 = api.quota_field(pe_id, donor, "guaranteed_accelerator_hours")
    assert 400 <= refulfill_resp.status_code < 500, refulfill_resp.text
    assert donor_after2 == pytest.approx(donor_after, abs=1e-6)


def test_cancelled_donation_cannot_be_fulfilled(api, experiment, run_id):
    canceller = make_agent(api, run_id, "donation-canceller")
    donor = make_agent(api, run_id, "donation-donor")
    pe_id = experiment("quota-donation-cancel", [canceller, donor], budget=4.0)

    donation_id = api.create_donation(canceller, pe_id, 0.02, "e2e cancel-before-fulfill coverage")
    assert api.cancel_donation(donation_id).status_code < 300

    canceller_before = api.quota_field(pe_id, canceller, "guaranteed_accelerator_hours")
    post_cancel_resp = api.fulfill_donation(donation_id, donor)
    canceller_after = api.quota_field(pe_id, canceller, "guaranteed_accelerator_hours")

    assert post_cancel_resp.status_code >= 400, post_cancel_resp.text
    assert canceller_after == pytest.approx(canceller_before, abs=1e-6)
