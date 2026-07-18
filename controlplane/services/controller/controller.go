package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Store is the persistence interface required by the controller.
// Note: silence detection and observed-cost accounting are pure GreptimeDB queries (see
// isAlive/observedElapsedHours in this package) — nothing about "how long has this job run" is
// ever cached in this process's memory.
// CPU/GPU utilization is never stored here — it lives in-memory in the node-agent window.
type Store interface {
	ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error)
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
	// ListStaleDesiredState returns SUBMITTED/ADMITTED/RUNNING experiments with no recent
	// cluster_job_reports row — see db.ClusterQueueStore.ListStaleDesiredState.
	ListStaleDesiredState(ctx context.Context, staleAfter time.Duration) ([]*domain.Experiment, error)
	// GetJobReport returns the cluster-agent's latest pushed status for id, or (nil, nil) if
	// none yet — used by checkSilence to tell "pod is mid-reschedule, no wonder it's quiet"
	// (Phase != running) apart from a genuinely hung training process (Phase == running).
	GetJobReport(ctx context.Context, id string) (*db.JobReport, error)
	// TransitionTerminal atomically transitions status and records the reason in one DB
	// transaction — see db.Store.TransitionTerminal. Does not write usage; the caller settles
	// separately (see Controller.settleAndMark) so that write can be retried independently.
	TransitionTerminal(ctx context.Context, id string, from, to domain.ExperimentStatus, reason string) (bool, error)
	// MarkQuotaSettled records that a terminal experiment's final observed usage has been
	// durably written — see services/settlement. Only called after that write succeeds.
	MarkQuotaSettled(ctx context.Context, id string) error
	// DeletePendingReservation releases id's durable pending-capacity claim, if any — a
	// no-op if none exists. Must be called on every path that moves a SUBMITTED/ADMITTED
	// experiment to a terminal status, or tick() keeps subtracting phantom capacity for a
	// job that no longer exists (see pending_capacity_reservations' schema comment).
	DeletePendingReservation(ctx context.Context, id string) error
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

	// Control-plane-side GC sweep (item #8): low-frequency, alert-only pass flagging orphaned
	// desired-state entries. 0 disables it.
	gcSweepInterval            time.Duration
	staleDesiredStateThreshold time.Duration

	// Phase 2 support (Domain 10). Optional — phase transitions are skipped if nil.
	phase2Store               Phase2Store
	metricsDBURL              string
	phase2BoundaryFrac        float64
	phase2AdmissionPercentile float64

	metricDeclineFraction float64
	silenceMultiplier     float64
	overrunMultiplier     float64
	defaultReportInterval time.Duration
	minSilenceWindow      time.Duration
}

// New returns a Controller with default reconcile interval.
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

// WithMetricDeclineFraction sets the fraction of estimated_duration_hours after which a
// continuously declining primary metric triggers eviction. Default 0.3 (30%).
func (c *Controller) WithMetricDeclineFraction(f float64) *Controller {
	c.metricDeclineFraction = f
	return c
}

func (c *Controller) WithSilenceMultiplier(m float64) *Controller {
	c.silenceMultiplier = m
	return c
}

func (c *Controller) WithOverrunMultiplier(m float64) *Controller {
	c.overrunMultiplier = m
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

// WithPhase2Store enables Domain 10 two-phase experiment execution. The metricsDBURL
// is used to query metric percentiles at the phase boundary.
func (c *Controller) WithPhase2Store(s Phase2Store, metricsDBURL string) *Controller {
	c.phase2Store = s
	c.metricsDBURL = metricsDBURL
	return c
}

// WithPhase2Boundary sets the budget fraction at which phase 2 triggers.
// Defaults to the package-level constant if not called.
func (c *Controller) WithPhase2Boundary(frac float64) *Controller {
	c.phase2BoundaryFrac = frac
	return c
}

// WithPhase2AdmissionPercentile sets the percentile used to separate passing agents
// from held agents at the phase-2 boundary. For a maximize metric the top fraction
// above this percentile passes; for a minimize metric the bottom fraction below
// (1 - percentile) passes. Defaults to 0.75 if not called.
func (c *Controller) WithPhase2AdmissionPercentile(p float64) *Controller {
	c.phase2AdmissionPercentile = p
	return c
}

// WithReconcileInterval overrides the default reconcile interval (useful for tests).
func (c *Controller) WithReconcileInterval(d time.Duration) *Controller {
	c.reconcileInterval = d
	return c
}

// WithGCSweep enables the control-plane-side GC sweep at the given interval, flagging
// SUBMITTED/ADMITTED/RUNNING experiments whose most recent cluster job report is older than
// staleAfter. Alert-only — see sweepStaleDesiredState.
func (c *Controller) WithGCSweep(interval, staleAfter time.Duration) *Controller {
	c.gcSweepInterval = interval
	c.staleDesiredStateThreshold = staleAfter
	return c
}

// Start launches the reconcile loop in a goroutine; it stops when ctx is
// cancelled and returns the first non-context error (if any).
func (c *Controller) Start(ctx context.Context) error {
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

	if c.gcSweepInterval > 0 {
		go func() {
			ticker := time.NewTicker(c.gcSweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := c.sweepStaleDesiredState(ctx); err != nil {
						c.logger.Error("stale desired-state sweep error", zap.Error(err))
					}
				}
			}
		}()
	}

	return nil
}
