package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
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
	// TransitionAndRefund atomically transitions status, records the reason, and refunds every
	// resource dimension in one DB transaction — see db.Store.TransitionAndRefund.
	TransitionAndRefund(ctx context.Context, id string, from, to domain.ExperimentStatus, reason, agentID, platformExpID string, refunds []db.ResourceRefund) (bool, error)
}

// QuotaService reads agent quota state. Refunds no longer go through here — every
// early-termination path writes observed cost via Store.TransitionAndRefund instead (see
// resourceRefunds).
type QuotaService interface {
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
}

// resourceRefunds builds observedFraction × each non-zero estimated resource dimension exp
// reserved (GPU-hours, plus CPU/RAM/storage when the submission set them) — the *observed* cost
// for that dimension, not an amount to subtract. TransitionAndRefund writes this value as the
// job's final recorded usage for each dimension (see db.ResourceRefund), overwriting its
// reservation outright rather than decrementing it, so replaying the same termination event
// twice (e.g. after a crash) is a no-op instead of a double refund.
// resourceRefunds builds the final observed-cost write for each resource dimension. GPU-hours is
// gpuCost — an absolute dollar amount already computed per accelerator type actually used (see
// metricsdb.ObservedGPUCost), because a single flat rate over observedFraction would mischarge
// any job that ran on more than one accelerator type. CPU/RAM/storage have no per-type rate
// tiers, so they're still the simpler observedFraction × estimate.
func resourceRefunds(exp *domain.Experiment, observedFraction, gpuCost float64) []db.ResourceRefund {
	dims := []struct {
		resourceType domain.ResourceType
		amount       float64
	}{
		{domain.ResourceGPUHours, gpuCost},
		{domain.ResourceCPUCoreHours, observedFraction * exp.EstimatedCPUCoreHours},
		{domain.ResourceRAMGBHours, observedFraction * exp.EstimatedRAMGBHours},
		{domain.ResourceStorageGBHours, observedFraction * exp.EstimatedStorageGBHours},
	}
	refunds := make([]db.ResourceRefund, 0, len(dims))
	for _, d := range dims {
		if d.amount <= 0 {
			continue
		}
		refunds = append(refunds, db.ResourceRefund{
			ExperimentID: exp.ID,
			ResourceType: d.resourceType,
			Tier:         exp.CapacityTier,
			Amount:       d.amount,
		})
	}
	return refunds
}

// SchedulerLoop is notified after evictions so it can refill freed capacity.
type SchedulerLoop interface {
	Trigger()
}

// Controller reconciles running experiments against their metric contracts.
type Controller struct {
	store             Store
	quota             QuotaService
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

// observedGapCap is the longest gap between two real observations that still counts as
// continuous presence — reused by every stateless metricsdb query below so silence detection and
// observed-cost accounting agree on what "still alive" means.
func (c *Controller) observedGapCap() time.Duration {
	return time.Duration(c.silenceMultiplier * float64(c.defaultReportInterval))
}

// observedStep is the grid resolution used to evaluate ObservedElapsedHours — fine enough to
// land inside every real report interval, coarse enough not to make the query expensive over a
// multi-hour experiment.
func (c *Controller) observedStep() time.Duration {
	if c.defaultReportInterval > 0 {
		return c.defaultReportInterval
	}
	return 10 * time.Second
}

// isAlive reports whether experimentID has a real observation (a node-agent heartbeat or a
// job-reported metric) within `window` of now — a stateless GreptimeDB query, not an in-memory
// last-seen map, so every process asking gets the same answer and nothing needs warming up after
// a restart.
func (c *Controller) isAlive(ctx context.Context, experimentID string, window time.Duration) (bool, error) {
	return metricsdb.IsAlive(ctx, c.metricsDBURL, experimentID, window)
}

// observedMaxLookback bounds how far back ObservedElapsedHours/FirstObserved scan to find a job's
// first sample — not a trusted clock, just a search-window ceiling no real job's runtime could
// exceed, so the range query stays cheap.
func (c *Controller) observedMaxLookback() time.Duration {
	return 14 * 24 * time.Hour
}

// observedElapsedHours returns how long experimentID has been confirmed alive, in hours — see
// metricsdb.ObservedElapsedHours for the full correctness argument (multi-node, clock-skew,
// gap-capping, and why no stored StartedAt is needed: the start boundary is derived from the
// metrics themselves). GreptimeDB is the sole source of truth with no fallback: a query error is
// returned to the caller to skip this write and retry on the next reconcile pass, never papered
// over with a wall-clock guess.
func (c *Controller) observedElapsedHours(ctx context.Context, experimentID string, now time.Time) (float64, error) {
	return metricsdb.ObservedElapsedHours(ctx, c.metricsDBURL, experimentID, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
}

// observedGPUCost returns experimentID's true GPU cost, billed per accelerator type it actually
// ran on — see metricsdb.ObservedGPUCost. This replaces elapsedHours × gpuCount × exp.GPUType.Cost()
// everywhere a job's real GPU spend is computed, so a job rescheduled onto a different type
// mid-run is billed correctly for each segment instead of one flat rate for its whole lifetime.
func (c *Controller) observedGPUCost(ctx context.Context, experimentID string, gpuCount int, now time.Time) (float64, error) {
	return metricsdb.ObservedGPUCost(ctx, c.metricsDBURL, experimentID, gpuCount, now, c.observedMaxLookback(), c.observedGapCap(), c.observedStep())
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

// sweepStaleDesiredState flags (log + gauge, never mutates) SUBMITTED/ADMITTED/RUNNING
// experiments with no recent cluster job report — orphaned desired-state that survived an
// extended cluster-agent outage combined with a control-plane bug/crash. cluster-agent's own
// ~2-3s reconcile is already GC-like on the cluster side; this is the rarer control-plane-side
// case that reactive cleanup alone doesn't catch. Alert-only by design — an operator decides
// what to do with a flagged experiment, this never auto-evicts or auto-corrects it.
func (c *Controller) sweepStaleDesiredState(ctx context.Context) error {
	stale, err := c.store.ListStaleDesiredState(ctx, c.staleDesiredStateThreshold)
	if err != nil {
		return fmt.Errorf("sweepStaleDesiredState: %w", err)
	}
	obsmetrics.StaleDesiredStateExperiments.Set(float64(len(stale)))
	for _, exp := range stale {
		c.logger.Warn("stale desired-state: no recent cluster job report",
			zap.String("id", exp.ID),
			zap.String("status", string(exp.Status)),
			zap.String("cluster", exp.ClusterName),
		)
	}
	return nil
}

// Reconcile runs one full reconciliation pass over all running experiments.
func (c *Controller) Reconcile(ctx context.Context) error {
	exps, err := c.store.ListRunningExperiments(ctx)
	if err != nil {
		return fmt.Errorf("controller.Reconcile: list: %w", err)
	}

	c.logger.Info("reconcile tick", zap.Int("running_experiments", len(exps)))

	now := time.Now().UTC()

	// Build PE report-interval map and metric-direction map; run phase-2 checks in one pass.
	reportIntervalByPE := map[string]time.Duration{}
	// metricDirectionByName: metricName → "maximize"|"minimize", derived from any PE's metric definitions.
	metricDirectionByName := map[string]string{}
	if c.phase2Store != nil {
		pes, err := c.phase2Store.ListPlatformExperiments(ctx, "running")
		if err != nil {
			c.logger.Error("phase2: list platform experiments", zap.Error(err))
		} else {
			for _, pe := range pes {
				if pe.ReportIntervalSeconds > 0 {
					reportIntervalByPE[pe.ID] = time.Duration(pe.ReportIntervalSeconds) * time.Second
				}
				for _, md := range pe.Metrics {
					metricDirectionByName[md.Key] = md.Direction
				}
				if err := c.checkPhase2Transition(ctx, pe, exps); err != nil {
					c.logger.Error("phase2: check transition", zap.String("pe", pe.ID), zap.Error(err))
				}
			}
		}
	}

	// Evict active jobs belonging to closed platform experiments (spec op 7c).
	// This runs on every tick, so it self-heals if the Close() call only updated DB
	// status but pod termination or refunds were not yet completed.
	if c.phase2Store != nil {
		if err := c.reconcileClosedExperiments(ctx); err != nil {
			c.logger.Error("reconcile closed experiments", zap.Error(err))
		}
	}

	// Quota exhaustion check: per unique (agentID, platformExpID) pair.
	checked := map[string]bool{}
	for _, exp := range exps {
		if exp.PlatformExperimentID == "" {
			continue
		}
		key := exp.AgentID + "/" + exp.PlatformExperimentID
		if checked[key] {
			continue
		}
		checked[key] = true
		if err := c.checkQuotaExhaustion(ctx, exp.AgentID, exp.PlatformExperimentID, now); err != nil {
			c.logger.Error("quota exhaustion check", zap.String("agent", exp.AgentID), zap.Error(err))
		}
	}

	for _, exp := range exps {
		if err := c.reconcileOne(ctx, exp, now, reportIntervalByPE, metricDirectionByName); err != nil {
			c.logger.Error("reconcile experiment",
				zap.String("id", exp.ID),
				zap.Error(err),
			)
		}
	}
	return nil
}

// checkQuotaExhaustion evicts all running jobs for an agent when actual T4h consumed
// reaches their tier quota. No refund — budget genuinely exhausted.
func (c *Controller) checkQuotaExhaustion(ctx context.Context, agentID, platformExpID string, now time.Time) error {
	aq, err := c.quota.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil || aq == nil {
		return err
	}

	running, err := c.store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}

	// UsedGuaranteedT4H = Σ(estimated_cost of running jobs) + Σ(actual_cost of completed jobs).
	// We want Σ(actual_running) + Σ(actual_completed) per the spec.
	// Replace running-job estimates with actual elapsed cost by adding the per-job delta.
	// The old code added Σ(actual_running) on top of UsedGuaranteedT4H which double-counted
	// running jobs (their estimate was already in UsedGuaranteedT4H).
	var deltaGuaranteed, deltaBurst float64
	for _, exp := range running {
		actual, err := c.observedGPUCost(ctx, exp.ID, exp.GPUCount, now)
		if err != nil {
			c.logger.Error("checkQuotaExhaustion: observed GPU cost", zap.String("experiment", exp.ID), zap.Error(err))
			continue
		}
		delta := actual - exp.EstimatedCostT4H // negative when under-budget, positive on overrun
		if exp.CapacityTier == domain.CapacityGuaranteed {
			deltaGuaranteed += delta
		} else {
			deltaBurst += delta
		}
	}
	// 0.99 not 1.0: floating-point accumulation across many debit/refund calls means "exactly
	// exhausted" may never compare equal/greater than the raw budget — a 1% margin avoids a
	// budget that's genuinely spent sitting just under the threshold forever.
	guaranteedExhausted := (aq.UsedGuaranteedT4H + deltaGuaranteed) >= aq.GuaranteedT4Hours*0.99
	burstExhausted := (aq.UsedBurstT4H + deltaBurst) >= aq.BurstT4Hours*0.99

	if !guaranteedExhausted && !burstExhausted {
		return nil
	}

	for _, exp := range running {
		tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
			(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
		if !tierExhausted {
			continue
		}
		updated, err := c.store.TransitionAndRefund(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionQuotaExhaustion), agentID, platformExpID, nil) // no refund: budget genuinely exhausted
		if err != nil {
			c.logger.Error("quota exhaustion evict", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
		// Status is already EVICTED above — that's what removes it from the cluster-agent's
		// desired-running set; the Job disappears on the agent's next reconcile pass.
		c.logger.Info("quota exhaustion eviction",
			zap.String("agent", agentID),
			zap.String("exp", exp.ID),
			zap.Float64("actual_guaranteed", aq.UsedGuaranteedT4H+deltaGuaranteed),
			zap.Float64("actual_burst", aq.UsedBurstT4H+deltaBurst),
			zap.Float64("quota_guaranteed", aq.GuaranteedT4Hours),
			zap.Float64("quota_burst", aq.BurstT4Hours),
		)
	}

	// Cancel QUEUED and SUBMITTED jobs and return their reservations — spec 7b.
	// No refund for running jobs (budget genuinely exhausted), but pre-run reservations are returned.
	cancelPreRun := func(exps []*domain.Experiment, finalStatus domain.ExperimentStatus) {
		for _, exp := range exps {
			tierExhausted := (guaranteedExhausted && exp.CapacityTier == domain.CapacityGuaranteed) ||
				(burstExhausted && exp.CapacityTier == domain.CapacityBurst)
			if !tierExhausted {
				continue
			}
			// Never started, so nothing was ever observed running: 0 observed cost.
			updated, err := c.store.TransitionAndRefund(ctx, exp.ID, exp.Status, finalStatus,
				string(domain.EvictionQuotaExhaustion), agentID, platformExpID, resourceRefunds(exp, 0, 0))
			if err != nil {
				c.logger.Error("quota exhaustion cancel pre-run", zap.String("id", exp.ID), zap.Error(err))
				continue
			}
			if !updated {
				continue
			}
			obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionQuotaExhaustion)).Inc()
			// Status is already updated above (away from SUBMITTED) — if a Job was created
			// for this experiment, it disappears on the cluster-agent's next reconcile pass.
			c.logger.Info("quota exhaustion: cancelled pre-run job",
				zap.String("agent", agentID),
				zap.String("exp", exp.ID),
				zap.String("status", string(exp.Status)),
			)
		}
	}

	// GetAgentQueuedExperiments returns both QUEUED and SUBMITTED experiments.
	// SUBMITTED jobs have a backend workload created but not yet admitted — handled by cancelPreRun.
	preRun, err := c.store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		c.logger.Error("quota exhaustion: list pre-run jobs", zap.String("agent", agentID), zap.Error(err))
	} else {
		cancelPreRun(preRun, domain.StatusRejected)
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

func (c *Controller) reconcileOne(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration, metricDirectionByName map[string]string) error {
	// 1. Silence check.
	if evict, reason := c.checkSilence(ctx, exp, now, reportIntervalByPE); evict {
		return c.evict(ctx, exp, reason, now)
	}

	// 2. Overrun — only evict if the researcher has no remaining capacity.
	// If they still have quota headroom, let the job continue past the 1.5× estimate.
	if evict, reason := c.checkOverrun(ctx, exp, now); evict {
		if !c.researcherHasCapacity(ctx, exp, now) {
			return c.evict(ctx, exp, reason, now)
		}
	}

	// 3. Metric decline: evict if all observed metrics have been monotonically declining
	// for longer than metricDeclineFraction × estimated_duration_hours.
	if evict, reason := c.checkMetricDecline(ctx, exp, now, metricDirectionByName); evict {
		return c.evict(ctx, exp, reason, now)
	}

	return nil
}

// checkMetricDecline returns true when every metric this experiment has reported shows no
// improvement for a continuous duration >= metricDeclineFraction × estimated_duration_hours.
// Recomputed from GreptimeDB's own stored samples every call — no in-memory window, so there is
// nothing here that a controller restart could lose or two replicas could disagree about.
func (c *Controller) checkMetricDecline(ctx context.Context, exp *domain.Experiment, now time.Time, metricDirectionByName map[string]string) (bool, domain.EvictionReason) {
	if exp.StartedAt == nil || exp.EstimatedDurationHours <= 0 || c.metricDeclineFraction <= 0 {
		return false, ""
	}
	declineWindow := time.Duration(c.metricDeclineFraction * exp.EstimatedDurationHours * float64(time.Hour))

	// ~50 grid points across the job's whole lifetime so far, regardless of how long that is —
	// coarse enough to keep the query cheap over a multi-hour run, fine enough to resolve a
	// genuine trend shift within declineWindow.
	step := now.Sub(*exp.StartedAt) / 50
	if step < c.observedStep() {
		step = c.observedStep()
	}

	promQL := fmt.Sprintf(`experiment_metric_value{job_id=%q}`, exp.ID)
	series, err := metricsdb.QueryRange(ctx, c.metricsDBURL, promQL, *exp.StartedAt, now, step)
	if err != nil {
		c.logger.Error("metric decline query failed", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}

	foundAny := false
	for _, s := range series {
		if len(s.Points) < 2 {
			continue
		}
		minimize := metricDirectionByName[s.Labels["metric_name"]] == "minimize"

		lastImproved := *exp.StartedAt
		for i := 1; i < len(s.Points); i++ {
			improved := s.Points[i].Value > s.Points[i-1].Value
			if minimize {
				improved = s.Points[i].Value < s.Points[i-1].Value
			}
			if improved && s.Points[i].Time.After(lastImproved) {
				lastImproved = s.Points[i].Time
			}
		}

		// Non-improving streak itself must span >= declineWindow to evict.
		if now.Sub(lastImproved) < declineWindow {
			return false, ""
		}
		foundAny = true
	}

	if !foundAny {
		return false, ""
	}
	return true, domain.EvictionMetricDecline
}

// checkSilence returns true when no real observation (node-agent heartbeat or job-reported
// metric) exists within max(minSilenceWindow, 3× the PE's report interval) — a stateless
// GreptimeDB query every time, so a controller restart or a second replica never has a
// "haven't seen anything yet" warm-up gap that looks like silence. Falls back to
// defaultReportInterval when the PE interval is unknown.
//
// Before evicting, this also checks the cluster-agent's latest job report: a job that's
// between pods (a node-death reschedule in progress, same self-heal path scenario 1 exercises)
// is expected to be quiet — that's not the "training process silently died" case this reason is
// for. Only a report showing the pod actually Running, with no metrics despite that, is real
// silence. No report yet, or a query error, means "can't tell" — skip and let the next
// reconcile pass (or ListStaleDesiredState, for a job that never got a report at all) decide.
func (c *Controller) checkSilence(ctx context.Context, exp *domain.Experiment, now time.Time, reportIntervalByPE map[string]time.Duration) (bool, domain.EvictionReason) {
	if exp.StartedAt == nil {
		return false, ""
	}
	reportInterval := c.defaultReportInterval
	if d, ok := reportIntervalByPE[exp.PlatformExperimentID]; ok && d > 0 {
		reportInterval = d
	}
	window := time.Duration(c.silenceMultiplier * float64(reportInterval))
	if window < c.minSilenceWindow {
		window = c.minSilenceWindow
	}
	if now.Sub(*exp.StartedAt) < window {
		return false, "" // hasn't even had one full window to report in yet
	}
	alive, err := c.isAlive(ctx, exp.ID, window)
	if err != nil {
		// GreptimeDB unreachable: don't evict on a query failure indistinguishable from
		// genuine silence — wait for the next reconcile pass to try again.
		c.logger.Error("silence check query failed", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}
	if !alive {
		report, err := c.store.GetJobReport(ctx, exp.ID)
		if err != nil {
			c.logger.Error("silence check: job report query failed", zap.String("experiment", exp.ID), zap.Error(err))
			return false, ""
		}
		if report != nil && workload.ParseJobPhase(report.Phase) != workload.JobPhaseRunning {
			// Pod isn't up right now (Pending/recreating/gone) — quiet is expected here,
			// not evidence of a hung process. Let the reschedule finish; next tick re-checks.
			return false, ""
		}
		return true, domain.EvictionSilent
	}
	return false, ""
}

// researcherHasCapacity returns true when the researcher still has unconsumed quota in the
// experiment's capacity tier — used to suppress overrun evictions when budget remains.
// It accounts for the overrunning experiment's actual elapsed cost beyond its estimate so
// that an already-exhausted agent is not given a false reprieve.
func (c *Controller) researcherHasCapacity(ctx context.Context, exp *domain.Experiment, now time.Time) bool {
	if exp.PlatformExperimentID == "" {
		return false
	}
	aq, err := c.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil || aq == nil {
		return false
	}
	// Aggregate overrun deltas across all running jobs in the same tier, mirroring
	// checkQuotaExhaustion (7b). Evaluating only the current job's overrun understates
	// true consumption when multiple jobs are simultaneously overrunning.
	running, err := c.store.GetAgentRunningExperiments(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return false
	}
	var deltaGuaranteed, deltaBurst float64
	for _, r := range running {
		actual, err := c.observedGPUCost(ctx, r.ID, r.GPUCount, now)
		if err != nil {
			c.logger.Error("researcherHasCapacity: observed GPU cost", zap.String("experiment", r.ID), zap.Error(err))
			continue
		}
		d := actual - r.EstimatedCostT4H
		if d < 0 {
			d = 0
		}
		if r.CapacityTier == domain.CapacityGuaranteed {
			deltaGuaranteed += d
		} else {
			deltaBurst += d
		}
	}
	switch exp.CapacityTier {
	case domain.CapacityGuaranteed:
		return (aq.UsedGuaranteedT4H + deltaGuaranteed) < aq.GuaranteedT4Hours*0.99
	case domain.CapacityBurst:
		return (aq.UsedBurstT4H + deltaBurst) < aq.BurstT4Hours*0.99
	}
	return false
}

// checkOverrun returns true when the experiment has been confirmed running (via
// observedElapsedHours, not wall-clock StartedAt) longer than overrunMultiplier× estimated — a
// job stuck in a reschedule/node-death gap for most of its wall-clock life isn't actually
// overrunning its budget. A query error is logged and treated as "not overrunning" for this
// pass: the next reconcile tick tries again, same as every other observed-time check in this
// file.
func (c *Controller) checkOverrun(ctx context.Context, exp *domain.Experiment, now time.Time) (bool, domain.EvictionReason) {
	if exp.EstimatedDurationHours <= 0 {
		return false, ""
	}
	hours, err := c.observedElapsedHours(ctx, exp.ID, now)
	if err != nil {
		c.logger.Error("checkOverrun: observed elapsed hours", zap.String("experiment", exp.ID), zap.Error(err))
		return false, ""
	}
	if hours > exp.EstimatedDurationHours*c.overrunMultiplier {
		return true, domain.EvictionOverrun
	}
	return false, ""
}

// evict marks the experiment EVICTED, terminates the backend workload, and refunds unused GPU
// hours — status transition, eviction reason, and every resource-dimension refund happen in one
// DB transaction (TransitionAndRefund), so a crash mid-eviction can never leave a durably
// EVICTED experiment with a partial refund.
func (c *Controller) evict(ctx context.Context, exp *domain.Experiment, reason domain.EvictionReason, now time.Time) error {
	// observedFraction is the share of the estimate actually confirmed running by real metrics
	// (0 for a job that never started or never reported; >1 on a genuine overrun) — always
	// computed, never gated on being positive, because 0 and >1 are both meaningful values that
	// must be written, not skipped.
	var observedFraction, gpuCost float64
	if exp.PlatformExperimentID != "" && exp.EstimatedDurationHours > 0 {
		hours, err := c.observedElapsedHours(ctx, exp.ID, now)
		if err != nil {
			return fmt.Errorf("evict: observed elapsed hours: %w", err)
		}
		observedFraction = hours / exp.EstimatedDurationHours
		if gpuCost, err = c.observedGPUCost(ctx, exp.ID, exp.GPUCount, now); err != nil {
			return fmt.Errorf("evict: observed GPU cost: %w", err)
		}
	}
	refunds := resourceRefunds(exp, observedFraction, gpuCost)

	updated, err := c.store.TransitionAndRefund(ctx, exp.ID, exp.Status, domain.StatusEvicted,
		string(reason), exp.AgentID, exp.PlatformExperimentID, refunds)
	if err != nil {
		return fmt.Errorf("evict TransitionAndRefund: %w", err)
	}
	if !updated {
		// Job already left exp.Status (completed or cancelled concurrently) — skip refund.
		return nil
	}
	obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(reason)).Inc()

	// Status is already EVICTED above — the cluster-agent's next reconcile pass removes the
	// Job on its own; nothing further to do here.
	c.logger.Info("experiment evicted",
		zap.String("id", exp.ID),
		zap.String("reason", string(reason)),
	)

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

// reconcileClosedExperiments evicts any jobs still active for closed platform experiments.
// Self-healing complement to Close(): if close succeeded in the DB but pod termination or
// refunds failed, the next reconcile tick finishes the cleanup automatically.
func (c *Controller) reconcileClosedExperiments(ctx context.Context) error {
	closedPEs, err := c.phase2Store.ListPlatformExperiments(ctx, "closed")
	if err != nil {
		return fmt.Errorf("reconcileClosedExperiments: list closed PEs: %w", err)
	}
	if len(closedPEs) == 0 {
		return nil
	}

	now := time.Now().UTC()
	for _, pe := range closedPEs {
		active, err := c.store.ListActiveByPlatformExperiment(ctx, pe.ID)
		if err != nil {
			c.logger.Error("reconcileClosedExperiments: list active jobs",
				zap.String("pe", pe.ID), zap.Error(err))
			continue
		}
		for _, exp := range active {
			if exp.Status == domain.StatusRunning || exp.Status == domain.StatusAdmitted {
				if err := c.evict(ctx, exp, domain.EvictionExperimentClosed, now); err != nil {
					c.logger.Error("reconcileClosedExperiments: evict running job",
						zap.String("exp", exp.ID), zap.Error(err))
				}
			} else {
				// QUEUED or SUBMITTED: cancel — never started, so 0 observed cost.
				var refunds []db.ResourceRefund
				if exp.PlatformExperimentID != "" {
					refunds = resourceRefunds(exp, 0, 0)
				}
				updated, err := c.store.TransitionAndRefund(ctx, exp.ID, exp.Status, domain.StatusRejected,
					string(domain.EvictionExperimentClosed), exp.AgentID, exp.PlatformExperimentID, refunds)
				if err != nil {
					c.logger.Error("reconcileClosedExperiments: cancel pre-run job",
						zap.String("exp", exp.ID), zap.Error(err))
					continue
				}
				if !updated {
					continue
				}
				obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionExperimentClosed)).Inc()
				c.logger.Info("reconcileClosedExperiments: cancelled pre-run job",
					zap.String("pe", pe.ID), zap.String("exp", exp.ID))
			}
		}
	}
	return nil
}
