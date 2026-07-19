package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"go.uber.org/zap"
)

// Create creates a new platform experiment in open status (accepting signups immediately).
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	metrics := req.Metrics
	if metrics == nil {
		metrics = []domain.MetricDefinition{}
	}
	reportInterval := req.ReportIntervalSeconds
	if reportInterval <= 0 {
		reportInterval = 30
	}
	pe := &domain.PlatformExperiment{
		ID:                    "pe-" + uuid.New().String()[:8],
		Name:                  req.Name,
		Description:           req.Description,
		BudgetAcceleratorHours:         req.BudgetAcceleratorHours,
		BudgetCPUCoreHours:    req.BudgetCPUCoreHours,
		BudgetRAMGBHours:      req.BudgetRAMGBHours,
		BudgetStorageGBHours:  req.BudgetStorageGBHours,
		MaxAgents:             req.MaxAgents,
		Metrics:               metrics,
		ReportIntervalSeconds: reportInterval,
		StartsAt:              req.StartsAt,
		EndsAt:                req.EndsAt,
		Status:                domain.PlatformExpOpen,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if pe.MaxAgents <= 0 {
		pe.MaxAgents = 100
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name))
	return pe, nil
}

// Update applies editable field changes to an existing platform experiment.
func (s *PlatformExperimentsService) Update(ctx context.Context, id string, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil, fmt.Errorf("experiment_not_editable: can only edit open experiments (status: %s)", pe.Status)
	}
	if req.Name != "" {
		pe.Name = req.Name
	}
	pe.Description = req.Description
	if req.BudgetAcceleratorHours > 0 {
		pe.BudgetAcceleratorHours = req.BudgetAcceleratorHours
	}
	if req.BudgetCPUCoreHours > 0 {
		pe.BudgetCPUCoreHours = req.BudgetCPUCoreHours
	}
	if req.BudgetRAMGBHours > 0 {
		pe.BudgetRAMGBHours = req.BudgetRAMGBHours
	}
	if req.BudgetStorageGBHours > 0 {
		pe.BudgetStorageGBHours = req.BudgetStorageGBHours
	}
	if req.MaxAgents > 0 {
		pe.MaxAgents = req.MaxAgents
	}
	if req.Metrics != nil {
		pe.Metrics = req.Metrics
	}
	if req.ReportIntervalSeconds > 0 {
		pe.ReportIntervalSeconds = req.ReportIntervalSeconds
	}
	if !req.StartsAt.IsZero() {
		pe.StartsAt = req.StartsAt
	}
	if !req.EndsAt.IsZero() {
		pe.EndsAt = req.EndsAt
	}
	if err := s.store.UpdatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	return pe, nil
}

// Signup registers an agent for a platform experiment.
func (s *PlatformExperimentsService) Signup(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return err
	}
	if pe == nil {
		return fmt.Errorf("experiment_not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return fmt.Errorf("signup_closed: experiment is %s", pe.Status)
	}

	count, err := s.store.CountSignups(ctx, platformExpID)
	if err != nil {
		return err
	}
	if count >= pe.MaxAgents {
		return fmt.Errorf("max_agents_reached: limit is %d", pe.MaxAgents)
	}

	if err := s.store.Signup(ctx, platformExpID, agentID); err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	return nil
}

// Start transitions an experiment to running and allocates per-agent quotas.
func (s *PlatformExperimentsService) Start(ctx context.Context, id string) ([]*domain.AgentQuota, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil, fmt.Errorf("invalid_transition: experiment is %s, expected open", pe.Status)
	}

	signedUp, err := s.store.ListSignups(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(signedUp) == 0 {
		return nil, fmt.Errorf("no_agents: cannot start with zero sign-ups")
	}

	// Pass 1: resolve agents and compute each agent's bonus fraction.
	// Unknown agents are skipped; only resolved agents participate in quota.
	type agentBonus struct {
		id    string
		bonus float64
	}
	resolved := make([]agentBonus, 0, len(signedUp))
	totalBonusFraction := 0.0
	for _, agentID := range signedUp {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			s.logger.Warn("start: agent not found, skipping", zap.String("agentID", agentID))
			continue
		}
		hasTop3, _ := s.store.HasTop3History(ctx, agentID)
		bonus := 0.0
		if hasTop3 {
			bonus += s.cfg.Top3BonusFraction
		}
		resolved = append(resolved, agentBonus{id: agentID, bonus: bonus})
		totalBonusFraction += bonus
	}

	quotas := make([]*domain.AgentQuota, 0, len(resolved))
	now := time.Now().UTC()

	// Cap initial allocation to the explore window so no agent can exhaust the
	// full budget before phase-2 eviction kicks in. Applied uniformly to every resource
	// dimension the platform experiment tracks — Accelerator is always populated; CPU/RAM/storage
	// budgets of 0 correctly allocate 0 (AllocateQuota(0, ...) returns 0,0), which is exactly
	// "not tracked."
	exploreFrac := s.cfg.Phase1ExploreFraction
	if exploreFrac <= 0 {
		exploreFrac = domain.Phase1ExploreFraction
	}

	for _, ab := range resolved {
		acceleratorGuaranteed, acceleratorBurst := domain.AllocateQuota(
			pe.BudgetAcceleratorHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		cpuGuaranteed, cpuBurst := domain.AllocateQuota(
			pe.BudgetCPUCoreHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		ramGuaranteed, ramBurst := domain.AllocateQuota(
			pe.BudgetRAMGBHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		storageGuaranteed, storageBurst := domain.AllocateQuota(
			pe.BudgetStorageGBHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)

		aq := &domain.AgentQuota{
			ID:                       uuid.New().String(),
			AgentID:                  ab.id,
			PlatformExperimentID:     id,
			GuaranteedAcceleratorHours:        acceleratorGuaranteed,
			BurstAcceleratorHours:             acceleratorBurst,
			GuaranteedCPUCoreHours:   cpuGuaranteed,
			BurstCPUCoreHours:        cpuBurst,
			GuaranteedRAMGBHours:     ramGuaranteed,
			BurstRAMGBHours:          ramBurst,
			GuaranteedStorageGBHours: storageGuaranteed,
			BurstStorageGBHours:      storageBurst,
			CreatedAt:                now,
		}
		if err := s.store.UpsertAgentQuota(ctx, aq); err != nil {
			return nil, fmt.Errorf("platform_experiments.Start: upsert quota for %s: %w", ab.id, err)
		}
		quotas = append(quotas, aq)

		s.logger.Info("quota allocated",
			zap.String("agentID", ab.id),
			zap.Float64("guaranteed_accelerator_hours", acceleratorGuaranteed),
			zap.Float64("burst_accelerator_hours", acceleratorBurst),
		)
	}

	if err := s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpRunning); err != nil {
		return nil, err
	}
	return quotas, nil
}

// Close transitions a running experiment to closed and records top-3 placements.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries).
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, topResults []AgentResult) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil {
		return fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpRunning && pe.Status != domain.PlatformExpOpen {
		return fmt.Errorf("invalid_transition: experiment is %s", pe.Status)
	}

	// Record top-3 placements and increment periods_active.
	for i, r := range topResults {
		if i >= 3 {
			break
		}
		if err := s.store.RecordTop3(ctx, id, r.AgentID, r.FinalMetric); err != nil {
			s.logger.Warn("close: record top3 failed", zap.String("agentID", r.AgentID), zap.Error(err))
		}
	}

	return s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpClosed)
}
