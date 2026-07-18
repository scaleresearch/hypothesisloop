package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
)

// phase2BoundaryFraction is the fixed fraction of total budget at which phase 2 triggers.
// Hardcoded backend constant — not configurable per experiment.
const phase2BoundaryFraction = domain.Phase1ExploreFraction

// Phase2Store is the persistence interface for phase-2 transition operations.
type Phase2Store interface {
	// Platform experiment queries.
	ListPlatformExperiments(ctx context.Context, statusFilter string) ([]*domain.PlatformExperiment, error)

	// Phase transition — one-way, atomic.
	TriggerPhase2(ctx context.Context, platformExpID string, heldAgentIDs []string) (bool, error)
	ListPhase2HeldAgents(ctx context.Context, platformExpID string) ([]string, error)
	IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error)

	// Quota redistribution. GPU-hours is the primary/always-populated dimension driving the
	// phase-2 trigger itself (GetTotalConsumedT4H); CPU/RAM/storage redistribute alongside it
	// for any platform experiment that also tracks them (see redistributeResource).
	ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error)
	// RedistributePhase2Quota atomically applies every zero/add op and claims completion in one
	// transaction — see db.PlatformExperimentsStore.RedistributePhase2Quota. Returns (false, nil)
	// if this platform experiment's redistribution was already committed by an earlier call.
	RedistributePhase2Quota(ctx context.Context, platformExpID string, zeros []db.Phase2ZeroOp, adds []db.Phase2AddOp) (bool, error)

	// Job control for held agents.
	GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// TransitionTerminal atomically transitions status and records the reason in one DB
	// transaction — see db.Store.TransitionTerminal. Does not write usage; the caller settles
	// separately (see Controller.settleAndMark).
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	// MarkQuotaSettled records that a terminal experiment's final observed usage has been
	// durably written — see services/settlement.
	MarkQuotaSettled(ctx context.Context, id string) error
}

// checkPhase2Transition checks whether a running platform experiment has consumed ≥ phase2_boundary
// fraction of its budget, and if so, triggers the phase-2 transition atomically.
// Returns without error if phase 2 is already active or if the boundary has not been reached.
func (c *Controller) checkPhase2Transition(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment) error {
	if pe.Phase != 1 {
		// Already triggered — retry any hold application a prior crash left incomplete.
		return c.reconcilePhase2Hold(ctx, pe, runningExps)
	}

	// Compute total consumed: committed (from the metrics DB) + in-flight (from running experiments).
	committed, err := metricsdb.TotalConsumedT4H(ctx, c.metricsDBURL, pe.ID)
	if err != nil {
		return fmt.Errorf("phase2: TotalConsumedT4H: %w", err)
	}
	// committed already includes running jobs' estimated costs (debited at submission).
	// Only add the overrun (actual − estimated) to avoid double-counting.
	var inFlight float64
	now := time.Now().UTC()
	for _, exp := range runningExps {
		if exp.PlatformExperimentID != pe.ID {
			continue
		}
		actual, err := c.observedGPUCost(ctx, exp.ID, exp.GPUCount, now)
		if err != nil {
			c.logger.Error("checkPhase2Transition: observed GPU cost", zap.String("experiment", exp.ID), zap.Error(err))
			continue
		}
		if delta := actual - exp.EstimatedCostT4H; delta > 0 {
			inFlight += delta
		}
	}
	totalConsumed := committed + inFlight

	boundary := c.phase2BoundaryFrac
	if boundary <= 0 {
		boundary = phase2BoundaryFraction
	}
	if totalConsumed < boundary*pe.BudgetT4Hours {
		return nil // boundary not yet reached
	}

	c.logger.Info("phase 2 boundary reached",
		zap.String("platform_experiment", pe.ID),
		zap.Float64("consumed", totalConsumed),
		zap.Float64("boundary_t4h", boundary*pe.BudgetT4Hours),
	)

	// Compute 75th percentile thresholds and determine which agents are active.
	activeAgentIDs, heldAgentIDs, err := c.computePhase2Admission(ctx, pe, runningExps)
	if errors.Is(err, ErrPhase2MetricsUnavailable) {
		// Fail open: postpone the transition rather than commit every agent to held with
		// no active agent left to receive the redistributed budget. The next reconcile
		// pass retries once metric data is available again.
		c.logger.Warn("phase2: postponing transition, metric data unavailable",
			zap.String("platform_experiment", pe.ID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("phase2: computeAdmission: %w", err)
	}

	// Atomic transition — returns false if already done.
	triggered, err := c.phase2Store.TriggerPhase2(ctx, pe.ID, heldAgentIDs)
	if err != nil {
		return fmt.Errorf("phase2: TriggerPhase2: %w", err)
	}
	if !triggered {
		return nil // beaten to it
	}

	c.logger.Info("phase 2 triggered",
		zap.String("platform_experiment", pe.ID),
		zap.Strings("active_agents", activeAgentIDs),
		zap.Strings("held_agents", heldAgentIDs),
		zap.Time("triggered_at", time.Now().UTC()),
	)

	// Stop held agents' jobs and redistribute quota.
	if err := c.applyPhase2Hold(ctx, pe, heldAgentIDs, activeAgentIDs, runningExps); err != nil {
		c.logger.Error("phase2: applyHold", zap.String("pe", pe.ID), zap.Error(err))
	}

	return nil
}

// reconcilePhase2Hold retries hold application for a platform experiment already past the
// trigger: heldAgentIDs is read back from the durable experiment_phase2_holds table, so it
// survives a crash no matter how far applyPhase2Hold got last time. activeAgentIDs isn't
// recomputed — redistribution runs at most once (RedistributePhase2Quota), so a retry never
// needs it.
func (c *Controller) reconcilePhase2Hold(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment) error {
	heldAgentIDs, err := c.phase2Store.ListPhase2HeldAgents(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("phase2: reconcile list held agents: %w", err)
	}
	if len(heldAgentIDs) == 0 {
		return nil
	}
	if err := c.applyPhase2Hold(ctx, pe, heldAgentIDs, nil, runningExps); err != nil {
		c.logger.Error("phase2: reconcile applyHold", zap.String("pe", pe.ID), zap.Error(err))
	}
	return nil
}
