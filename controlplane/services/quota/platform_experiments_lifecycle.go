package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	stages := req.Stages
	if len(stages) == 0 {
		stages = s.cfg.DefaultStages
	}
	// Fixed at creation, so it is rejected here rather than defended against downstream.
	if err := domain.ValidateStages(stages); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	pe := &domain.PlatformExperiment{
		ID:                     "pe-" + uuid.New().String()[:8],
		Name:                   req.Name,
		Description:            req.Description,
		BudgetAcceleratorHours: req.BudgetAcceleratorHours,
		BudgetCPUCoreHours:     req.BudgetCPUCoreHours,
		BudgetRAMGBHours:       req.BudgetRAMGBHours,
		BudgetStorageGBHours:   req.BudgetStorageGBHours,
		MaxAgents:              req.MaxAgents,
		Metrics:                metrics,
		ReportIntervalSeconds:  reportInterval,
		StartsAt:               req.StartsAt,
		EndsAt:                 req.EndsAt,
		Status:                 domain.PlatformExpOpen,
		Stages:                 stages,
		CurrentStage:           1,
		CreatedAt:              now,
		UpdatedAt:              now,
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

	// Only the first stage's share of the budget is released now; each later stage releases its
	// own share at the boundary it starts (see controller.applyCut). This is what stops one agent
	// exhausting the whole budget before the ladder has cut anyone. Applied uniformly to every
	// resource dimension the platform experiment tracks — Accelerator is always populated;
	// CPU/RAM/storage budgets of 0 correctly allocate 0 (AllocateQuota(0, ...) returns 0,0),
	// which is exactly "not tracked."
	exploreFrac := pe.Stages[0].LengthPct / 100.0

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
			ID:                         uuid.New().String(),
			AgentID:                    ab.id,
			PlatformExperimentID:       id,
			GuaranteedAcceleratorHours: acceleratorGuaranteed,
			BurstAcceleratorHours:      acceleratorBurst,
			GuaranteedCPUCoreHours:     cpuGuaranteed,
			BurstCPUCoreHours:          cpuBurst,
			GuaranteedRAMGBHours:       ramGuaranteed,
			BurstRAMGBHours:            ramBurst,
			GuaranteedStorageGBHours:   storageGuaranteed,
			BurstStorageGBHours:        storageBurst,
			CreatedAt:                  now,
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

// SweepExpired closes every Open/Running platform experiment whose EndsAt has passed. EndsAt is
// the zero value when unset (PlatformExperiment.EndsAt is not a pointer), so those are skipped —
// a platform experiment with no end time runs until someone calls Close explicitly.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	now := time.Now()
	for _, status := range []string{string(domain.PlatformExpOpen), string(domain.PlatformExpRunning)} {
		pes, err := s.store.ListPlatformExperiments(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			if pe.EndsAt.IsZero() || pe.EndsAt.After(now) {
				continue
			}
			// TODO(winners): topResults is nil here, so Close never calls RecordTop3 for an
			// auto-closed (ends_at-expired) platform experiment — the only realistic path for a
			// real deadline-bound experiment. RecordTop3/HasTop3History (the quota bonus new
			// experiments give agents with a prior top-3) is effectively dead code as a result.
			// Fix: compute topResults here from the same per-agent-best-metric ranking a stage
			// boundary already does (metricsdb.QueryAgentValues with a max_over_time/
			// min_over_time PromQL over experiment_metric_value, see
			// controller.cutOnMetric), keyed off pe.Metrics[0] as the primary metric,
			// sorted best-first, then pass the top 3 into Close. Deliberately not done here —
			// see agent/ session notes: no leaderboard/reputation use case needs it yet.
			if err := s.Close(ctx, pe.ID, nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment past ends_at",
				zap.String("platform_experiment_id", pe.ID), zap.Time("ends_at", pe.EndsAt))
		}
	}
	return nil
}

// SweepAutoStart starts every Open platform experiment whose StartsAt has passed and has at
// least one sign-up. Without this, an operator has to call POST /start by hand the moment
// starts_at arrives — StartsAt would otherwise be inert metadata, which breaks the case this
// platform is actually for: agents discovering and running a platform experiment end-to-end with
// nobody watching the clock for them. A zero StartsAt (unset) is treated as "start immediately
// on first sign-up" is *not* implied here — Start still requires an explicit call in that case,
// since there's no scheduled moment to sweep against; only a non-zero, past StartsAt auto-fires.
func (s *PlatformExperimentsService) SweepAutoStart(ctx context.Context) error {
	now := time.Now()
	pes, err := s.store.ListPlatformExperiments(ctx, string(domain.PlatformExpOpen))
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.StartsAt.IsZero() || pe.StartsAt.After(now) {
			continue
		}
		signedUp, err := s.store.ListSignups(ctx, pe.ID)
		if err != nil {
			s.logger.Error("sweep_auto_start: list signups failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		if len(signedUp) == 0 {
			continue // wait for at least one sign-up before starting, same rule Start() enforces
		}
		if _, err := s.Start(ctx, pe.ID); err != nil {
			s.logger.Error("sweep_auto_start: start failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		s.logger.Info("sweep_auto_start: started platform experiment past starts_at",
			zap.String("platform_experiment_id", pe.ID), zap.Time("starts_at", pe.StartsAt))
	}
	return nil
}

// StartExpirySweep runs SweepAutoStart and SweepExpired on a ticker until ctx is cancelled.
// Mirrors the idiomatic ticker-loop pattern used elsewhere in this codebase (see
// scheduler.JobWatcher.Start). Auto-start runs before auto-close so a platform experiment whose
// starts_at and ends_at are both already in the past (e.g. after a control-service restart) is
// briefly started and then immediately closed, rather than closed while still "open" — closing
// requires status running-or-open already, so this ordering isn't strictly required for
// correctness, but it keeps the transition history sane (open -> running -> closed).
func (s *PlatformExperimentsService) StartExpirySweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SweepAutoStart(ctx); err != nil {
				s.logger.Error("sweep_auto_start: scan", zap.Error(err))
			}
			if err := s.SweepExpired(ctx); err != nil {
				s.logger.Error("sweep_expired: scan", zap.Error(err))
			}
		}
	}
}
