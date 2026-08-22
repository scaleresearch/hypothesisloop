package quota

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// DonationError distinguishes an expected, client-caused rejection (bad donation state, self-
// donation, insufficient quota) from a genuine server-side failure — same pattern as
// scheduler.AdmissionError — so the HTTP handler can return the right 4xx instead of a blanket
// 500 for outcomes the caller can reasonably trigger (e.g. fulfilling an already-fulfilled or
// cancelled request).
type DonationError struct {
	Reason  string
	Message string
}

func (e *DonationError) Error() string {
	return fmt.Sprintf("donation rejected [%s]: %s", e.Reason, e.Message)
}

const (
	DonationReasonNotFound          = "not_found"
	DonationReasonInvalidState      = "invalid_state"
	DonationReasonSelfDonation      = "self_donation"
	DonationReasonInsufficientQuota = "insufficient_quota"
)

// FulfillDonation transfers AccH from donor to recipient within a platform experiment.
// Debits donor's guaranteed_accelerator_hours and credits recipient's guaranteed_accelerator_hours.
// The donation must have a platform_experiment_id; the donor must have sufficient available quota.
func (s *PlatformExperimentsService) FulfillDonation(ctx context.Context, donationID, donorAgentID string) error {
	req, err := s.store.GetDonationRequest(ctx, donationID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donation: %w", err)
	}
	if req == nil {
		return &DonationError{Reason: DonationReasonNotFound, Message: fmt.Sprintf("donation %s not found", donationID)}
	}
	if req.Status != "open" {
		return &DonationError{Reason: DonationReasonInvalidState, Message: fmt.Sprintf("donation is %s, not open", req.Status)}
	}
	if req.AgentID == donorAgentID {
		return &DonationError{Reason: DonationReasonSelfDonation, Message: "donor and recipient must be different agents"}
	}
	if req.PlatformExperimentID == "" {
		return &DonationError{Reason: DonationReasonInvalidState, Message: "donation has no platform_experiment_id"}
	}

	// Read the donor's current guaranteed usage (reservations) from the metrics store — the sole
	// source of consumption — so the atomic transfer below can check headroom against the live
	// reserved total. GetQuota merges used_* in from GreptimeDB on top of the Postgres allocation.
	donorQuota, err := s.GetQuota(ctx, donorAgentID, req.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donor quota: %w", err)
	}
	if donorQuota == nil || donorQuota.AvailableGuaranteed() < req.CreditsWant {
		avail := 0.0
		if donorQuota != nil {
			avail = donorQuota.AvailableGuaranteed()
		}
		return &DonationError{Reason: DonationReasonInsufficientQuota, Message: fmt.Sprintf("donor has %.2f AccH available, need %.2f", avail, req.CreditsWant)}
	}

	// One atomic, idempotent transaction locks the donation + both quota rows, re-verifies the
	// donation is still open and the donor has headroom, moves the amount and marks it fulfilled.
	// Gating on "open" inside the tx makes a retry after a partial failure a safe no-op instead of
	// a double transfer. The headroom re-check inside the tx (against the metrics-store reserved
	// total passed here) is the authoritative guard; an error from it means a concurrent donation
	// raced this one past the donor's balance. Donations are accelerator-hours only today.
	// The matching burst headroom moves with the guaranteed hours, since burst is derived from
	// guaranteed (domain.AllocateQuota) — moving one without the other leaves the donor with burst
	// for hours it gave away and the recipient with none for hours it received.
	fulfilled, err := s.store.FulfillDonationTx(ctx, donationID, donorAgentID, req.AgentID, req.PlatformExperimentID,
		domain.ResourceAcceleratorHours, req.CreditsWant, req.CreditsWant*s.cfg.BurstFraction,
		func(ctx context.Context) (*domain.AgentQuota, error) {
			return s.GetQuota(ctx, donorAgentID, req.PlatformExperimentID)
		})
	if err != nil {
		return fmt.Errorf("FulfillDonation: transfer: %w", err)
	}
	if !fulfilled {
		// Another fulfillment already closed this donation (or it was cancelled) between the read
		// above and the locked check — not open anymore.
		return &DonationError{Reason: DonationReasonInvalidState, Message: "donation is no longer open"}
	}

	s.logger.Info("FulfillDonation: quota transferred",
		zap.String("donationID", donationID),
		zap.String("donor", donorAgentID),
		zap.String("recipient", req.AgentID),
		zap.String("platformExpID", req.PlatformExperimentID),
		zap.Float64("amount", req.CreditsWant),
	)
	return nil
}
