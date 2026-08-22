package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// StagesStore is the persistence interface for the stage ladder.
type StagesStore interface {
	// Platform experiment queries.
	ListPlatformExperiments(ctx context.Context, filter db.PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error)

	// Stage boundaries.
	// AdvanceStage atomically records the cut agents, redistributes each dimension's released
	// allocation to survivors, and claims the boundary. Returns (false, nil) if already
	// committed by an earlier call.
	AdvanceStage(ctx context.Context, platformExpID string, stageIndex int, cutAgentIDs, survivorIDs []string, dims []db.StageRedistribution) (bool, error)
	ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error)
	// ListSignupsByRole is how the ladder learns who is being ranked at all. A baseline or a
	// reviewer holds quota and runs jobs like anyone else; it is simply not one of the things
	// the ladder is comparing.
	ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error)
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)

	// Quota. Accelerator-hours drives progress itself (GetTotalConsumedAccH); CPU/RAM/storage
	// are released alongside it (see applyCut).
	ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error)
	AddDesiredQuotaUsage(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error

	// Job control for cut agents.
	GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// TransitionTerminal atomically transitions status and records the reason. Does not write
	// usage; the caller settles separately (see Controller.settleAndMark).
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	// MarkQuotaSettled records that a terminal experiment's final usage was durably written.
	MarkQuotaSettled(ctx context.Context, id string) error
}

// advanceStages advances a running platform experiment's ladder by at most one stage per tick,
// cutting the configured share of survivors at the boundary it crosses.
func (c *Controller) advanceStages(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment, obs *tickObservations, now time.Time) error {
	// Boundaries already crossed may have left job-stopping incomplete (a crash between the
	// AdvanceStage commit and the eviction sweep). Retrying it is free — it is idempotent.
	if err := c.reconcileCuts(ctx, pe, runningExps); err != nil {
		return err
	}

	// The final stage has no boundary after it: nothing left to advance to.
	if pe.CurrentStage >= len(pe.Stages) {
		return nil
	}

	progress, err := c.stageProgress(ctx, pe, runningExps, obs, now)
	if err != nil {
		return err
	}
	boundary := domain.BoundaryProgress(pe.Stages, pe.CurrentStage)
	if progress < boundary {
		return nil
	}

	stage, ok := pe.CurrentStageDef()
	if !ok {
		c.logger.Error("platform experiment has invalid current_stage, skipping stage advance",
			zap.String("platform_experiment", pe.ID),
			zap.Int("current_stage", pe.CurrentStage),
			zap.Int("stages", len(pe.Stages)))
		return nil
	}
	c.logger.Info("stage boundary reached",
		zap.String("platform_experiment", pe.ID),
		zap.Int("stage", pe.CurrentStage),
		zap.Float64("progress", progress),
		zap.Float64("boundary", boundary),
		zap.Float64("evict_pct", stage.EvictPct),
	)

	survivors, cut, err := c.computeCut(ctx, pe, stage)
	if errors.Is(err, ErrStageMetricsUnavailable) {
		// The threshold is always drawn from an observed value, so a healthy query can never
		// produce an empty survivor set — an empty set means broken data. Postpone to the next
		// reconcile tick rather than cutting anyone.
		c.logger.Warn("stages: postponing boundary, metric data unavailable",
			zap.String("platform_experiment", pe.ID), zap.Int("stage", pe.CurrentStage))
		return nil
	}
	if err != nil {
		return fmt.Errorf("stages: computeCut: %w", err)
	}

	if err := c.applyCut(ctx, pe, pe.CurrentStage, survivors, cut, runningExps); err != nil {
		return fmt.Errorf("stages: applyCut: %w", err)
	}
	return nil
}

// stageProgress evaluates domain.StageProgress for a platform experiment: settled observed
// usage plus the live in-flight cost of its running jobs, against budget and wall clock.
// Reservations never contribute — see shared/metricsdb/usage.go for why a large queued job
// must not be able to trip a boundary.
func (c *Controller) stageProgress(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment, obs *tickObservations, now time.Time) (float64, error) {
	committed, err := metricsdb.TotalObservedAccH(ctx, c.metricsDBURL, pe.CreatedAt, pe.ID)
	if err != nil {
		return 0, fmt.Errorf("stages: TotalObservedAccH: %w", err)
	}
	var inFlight float64
	for _, exp := range runningExps {
		if exp.PlatformExperimentID != pe.ID {
			continue
		}
		// The pass's own reading, not a fresh query: the boundary this feeds and the job-length
		// cap must be judging the same job by the same number.
		hours, ok := obs.hours(exp.ID)
		if !ok {
			continue
		}
		inFlight += observedAcceleratorCost(exp, hours)
	}

	return domain.StageProgress(committed+inFlight, pe.BudgetAcceleratorHours, pe.StartsAt, pe.EndsAt, now), nil
}

// reconcileCuts retries job-stopping for every agent already cut in this platform experiment.
// The cut set is read back from the durable platform_experiment_cuts table, so it survives a
// crash; stopping is idempotent, so this runs every tick.
func (c *Controller) reconcileCuts(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment) error {
	cuts, err := c.stagesStore.ListCutAgents(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("stages: list cut agents: %w", err)
	}
	for _, cut := range cuts {
		if err := c.stopCutAgentJobs(ctx, cut.AgentID, pe.ID, runningExps); err != nil {
			c.logger.Error("stages: stop cut agent jobs",
				zap.String("agent", cut.AgentID), zap.String("pe", pe.ID), zap.Error(err))
		}
	}
	return nil
}
