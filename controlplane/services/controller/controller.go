package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Store is the persistence interface required by the controller.
// Note: silence detection and observed-cost accounting are pure GreptimeDB queries (see
// isAlive/observedElapsedHours in this package) — nothing about "how long has this job run" is
// ever cached in this process's memory.
// Cluster and job observations are queried from metrics storage and never cached here.
type Store interface {
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
	ListExperimentsWithStatus(ctx context.Context, status domain.ExperimentStatus) ([]*domain.Experiment, error)
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	// GetAgentQueuedExperiments returns QUEUED and SUBMITTED experiments for an agent.
	// Both are pre-run: their reservations must be returned when the budget is exhausted.
	GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	// TransitionStatus atomically updates status only when current status matches from.
	// Returns false (no error) if the row was already in a different state — used to
	// prevent double-refunds when eviction and natural completion race.
	TransitionStatus(ctx context.Context, id string, from, to domain.ExperimentStatus) (bool, error)
	// ListActiveByPlatformExperiment returns all non-terminal jobs (QUEUED, SUBMITTED,
	// ADMITTED, RUNNING) for a platform experiment. Used by close-eviction reconciliation.
	ListActiveByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error)
	// TransitionTerminal atomically transitions status and records the reason in one DB
	// transaction — see db.Store.TransitionTerminal. Does not write usage; the caller settles
	// separately (see Controller.settleAndMark) so that write can be retried independently.
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	// MarkQuotaSettled records that a terminal experiment's final observed usage has been
	// durably written — see services/settlement. Only called after that write succeeds.
	MarkQuotaSettled(ctx context.Context, id string) error
}

// QuotaService reads agent quota state. Refunds no longer go through here — every
// early-termination path settles observed cost via Controller.settleAndMark instead.
type QuotaService interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
}

// QuotaSettler durably writes a terminal experiment's final observed usage — see
// services/settlement.Settler. Idempotent and safe to retry.
type QuotaSettler interface {
	Settle(ctx context.Context, exp *domain.Experiment) error
}

// SchedulerLoop is notified after evictions so it can refill freed capacity.
type SchedulerLoop interface {
	Trigger()
}

// Controller reconciles running experiments against their metric contracts.
type Controller struct {
	store             Store
	quota             QuotaService
	settler           QuotaSettler  // durably settles final usage after every termination
	loop              SchedulerLoop // notified after evictions
	reconcileInterval time.Duration
	logger            *zap.Logger

	// Stage ladder support (docs/stages.md). Optional — boundaries are skipped if nil.
	stagesStore  StagesStore
	metricsDBURL string

	silenceMultiplier     float64
	defaultReportInterval time.Duration
	minSilenceWindow      time.Duration
}

// New constructs an unwired Controller. Start validates every production dependency and
// operational value after the explicit With... configuration has been applied.
func New(store Store, quota QuotaService, logger *zap.Logger) *Controller {
	return &Controller{
		store:  store,
		quota:  quota,
		logger: logger,
	}
}

// WithSettler attaches the durable settler used to write final observed usage after every
// termination this Controller performs.
func (c *Controller) WithSettler(s QuotaSettler) *Controller {
	c.settler = s
	return c
}

// settleAndMark durably writes exp's final observed usage and marks it settled on success. Safe
// to call unconditionally after any successful terminal transition: on failure, exp is left
// unsettled for services/settlement.Reconciler to retry — this is a best-effort fast path, not
// the only chance to settle.
func (c *Controller) settleAndMark(ctx context.Context, exp *domain.Experiment) {
	if c.settler == nil {
		return
	}
	if err := c.settler.Settle(ctx, exp); err != nil {
		c.logger.Warn("controller: settle quota", zap.String("id", exp.ID), zap.Error(err))
		return
	}
	if err := c.store.MarkQuotaSettled(ctx, exp.ID); err != nil {
		c.logger.Error("controller: mark quota settled", zap.String("id", exp.ID), zap.Error(err))
	}
}

func (c *Controller) WithSilenceMultiplier(m float64) *Controller {
	c.silenceMultiplier = m
	return c
}

func (c *Controller) WithDefaultReportInterval(d time.Duration) *Controller {
	c.defaultReportInterval = d
	return c
}

// WithMinSilenceWindow floors the silence window (silenceMultiplier * report interval) —
// see MinSilenceWindowSeconds's doc comment in shared/config for why.
func (c *Controller) WithMinSilenceWindow(d time.Duration) *Controller {
	c.minSilenceWindow = d
	return c
}

// WithSchedulerLoop wires the scheduler loop to be triggered after evictions.
func (c *Controller) WithSchedulerLoop(l SchedulerLoop) *Controller {
	c.loop = l
	return c
}

// WithStagesStore enables the stage ladder. The metricsDBURL is used to rank agents at each
// stage boundary. The ladder itself is per-platform-experiment config, not a controller knob.
func (c *Controller) WithStagesStore(s StagesStore, metricsDBURL string) *Controller {
	c.stagesStore = s
	c.metricsDBURL = metricsDBURL
	return c
}

// WithReconcileInterval overrides the default reconcile interval (useful for tests).
func (c *Controller) WithReconcileInterval(d time.Duration) *Controller {
	c.reconcileInterval = d
	return c
}

// Start launches the reconcile loop in a goroutine; it stops when ctx is
// cancelled and returns the first non-context error (if any).
func (c *Controller) Start(ctx context.Context) error {
	if c.store == nil || c.quota == nil || c.settler == nil || c.stagesStore == nil || c.logger == nil {
		return fmt.Errorf("controller: store, quota, settler, stages store, and logger are required")
	}
	if c.metricsDBURL == "" {
		return fmt.Errorf("controller: metrics DB URL is required")
	}
	if c.reconcileInterval <= 0 ||
		c.defaultReportInterval <= 0 || c.minSilenceWindow <= 0 ||
		c.silenceMultiplier <= 0 {
		return fmt.Errorf("controller: all timing and multiplier values must be explicitly valid")
	}
	go func() {
		ticker := time.NewTicker(c.reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Reconcile(ctx); err != nil {
					c.logger.Error("reconcile error", zap.Error(err))
				}
			}
		}
	}()

	return nil
}
