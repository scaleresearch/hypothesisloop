package quota

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
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

// FulfillDonation transfers AccH from donor to recipient within a platform experiment. Debits
// the donor's guaranteed_accelerator_hours; credits the recipient's guaranteed_accelerator_hours
// if the recipient's resolved quota tier is guaranteed, or burst_accelerator_hours if it's
// burst-only (see domain.ResolveQuotaTier / domain.ApplyQuotaTier — only a donor's own guaranteed
// quota is ever debited, since only a guaranteed-tier participant can have any). The donation
// must have a platform_experiment_id; the donor must have sufficient available quota.
func (s *PlatformExperimentsService) FulfillDonation(ctx context.Context, donationID, donorAgentID string) error {
	req, err := s.store.GetDonationRequest(ctx, donationID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donation: %w", err)
	}
	if req == nil {
		return &DonationError{Reason: DonationReasonNotFound, Message: fmt.Sprintf("donation %s not found", donationID)}
	}
	if req.AgentID == donorAgentID {
		return &DonationError{Reason: DonationReasonSelfDonation, Message: "donor and recipient must be different agents"}
	}
	if req.PlatformExperimentID == "" {
		return &DonationError{Reason: DonationReasonInvalidState, Message: "donation has no platform_experiment_id"}
	}

	// One atomic, idempotent transaction locks the donation + both quota rows, verifies the
	// donation is open and the donor has headroom, moves the amount and marks it fulfilled.
	// Gating on "open" inside the tx makes a retry after a partial failure a safe no-op instead of
	// a double transfer. Headroom is checked against the donor's usage as read inside the tx,
	// merged from the metrics store on top of the Postgres allocation. Donations are
	// accelerator-hours only today.
	// The matching burst headroom moves with the guaranteed hours, since burst is derived from
	// guaranteed (domain.AllocateQuota) — moving one without the other leaves the donor with burst
	// for hours it gave away and the recipient with none for hours it received.
	fulfilled, err := s.store.FulfillDonationTx(ctx, donationID, donorAgentID, req.AgentID, req.PlatformExperimentID,
		domain.ResourceAcceleratorHours, req.CreditsWant, req.CreditsWant*s.cfg.BurstFraction,
		func(ctx context.Context) (*domain.AgentQuota, error) {
			return s.GetAgentQuota(ctx, donorAgentID, req.PlatformExperimentID)
		})
	if errors.Is(err, db.ErrDonorInsufficientQuota) {
		return &DonationError{Reason: DonationReasonInsufficientQuota, Message: fmt.Sprintf("donor has less than %.2f AccH available", req.CreditsWant)}
	}
	if err != nil {
		return fmt.Errorf("FulfillDonation: transfer: %w", err)
	}
	if !fulfilled {
		return &DonationError{Reason: DonationReasonInvalidState, Message: "donation is not open"}
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
