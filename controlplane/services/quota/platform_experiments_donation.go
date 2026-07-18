package quota

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"go.uber.org/zap"
)

// FulfillDonation transfers T4h from donor to recipient within a platform experiment.
// Debits donor's guaranteed_t4_hours and credits recipient's guaranteed_t4_hours.
// The donation must have a platform_experiment_id; the donor must have sufficient available quota.
func (s *PlatformExperimentsService) FulfillDonation(ctx context.Context, donationID, donorAgentID string) error {
	req, err := s.store.GetDonationRequest(ctx, donationID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donation: %w", err)
	}
	if req == nil {
		return fmt.Errorf("FulfillDonation: donation %s not found", donationID)
	}
	if req.Status != "open" {
		return fmt.Errorf("FulfillDonation: donation is %s, not open", req.Status)
	}
	if req.AgentID == donorAgentID {
		return fmt.Errorf("FulfillDonation: donor and recipient must be different agents")
	}
	if req.PlatformExperimentID == "" {
		return fmt.Errorf("FulfillDonation: donation has no platform_experiment_id")
	}

	donorQuota, err := s.GetQuota(ctx, donorAgentID, req.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donor quota: %w", err)
	}
	if donorQuota == nil || donorQuota.AvailableGuaranteed() < req.CreditsWant {
		avail := 0.0
		if donorQuota != nil {
			avail = donorQuota.AvailableGuaranteed()
		}
		return fmt.Errorf("insufficient_quota: donor has %.2f T4h available, need %.2f", avail, req.CreditsWant)
	}

	// Debit donor's allocation. Donations are GPU-hours only today.
	if err := s.store.AddToAgentGuaranteedQuota(ctx, donorAgentID, req.PlatformExperimentID, domain.ResourceGPUHours, -req.CreditsWant); err != nil {
		return fmt.Errorf("FulfillDonation: debit donor: %w", err)
	}
	// Credit recipient's allocation.
	if err := s.store.AddToAgentGuaranteedQuota(ctx, req.AgentID, req.PlatformExperimentID, domain.ResourceGPUHours, req.CreditsWant); err != nil {
		_ = s.store.AddToAgentGuaranteedQuota(ctx, donorAgentID, req.PlatformExperimentID, domain.ResourceGPUHours, req.CreditsWant) // rollback
		return fmt.Errorf("FulfillDonation: credit recipient: %w", err)
	}

	if err := s.store.UpdateDonationStatus(ctx, donationID, "fulfilled"); err != nil {
		return fmt.Errorf("FulfillDonation: update status: %w", err)
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
