package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
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
	ZeroAgentGuaranteedQuota(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType) error
	AddToAgentGuaranteedQuota(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType, delta float64) error

	// Job control for held agents.
	GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	UpdateExperimentStatus(ctx context.Context, id string, status domain.ExperimentStatus) error
	UpdateEvictionReason(ctx context.Context, id, reason string) error
	// TransitionAndRefund atomically transitions status, records the reason, and refunds every
	// resource dimension in one DB transaction — see db.Store.TransitionAndRefund.
	TransitionAndRefund(ctx context.Context, id string, from, to domain.ExperimentStatus, reason, agentID, platformExpID string, refunds []db.ResourceRefund) (bool, error)
}

// checkPhase2Transition checks whether a running platform experiment has consumed ≥ phase2_boundary
// fraction of its budget, and if so, triggers the phase-2 transition atomically.
// Returns without error if phase 2 is already active or if the boundary has not been reached.
func (c *Controller) checkPhase2Transition(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment) error {
	if pe.Phase != 1 {
		return nil // already in phase 2
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

// computePhase2Admission queries Prometheus to determine which agents clear the
// configured percentile on at least one metric. Returns (activeAgentIDs, heldAgentIDs, error).
func (c *Controller) computePhase2Admission(ctx context.Context, pe *domain.PlatformExperiment, runningExps []*domain.Experiment) ([]string, []string, error) {
	// Collect all signed-up agents from quotas.
	quotas, err := c.phase2Store.ListAgentQuotas(ctx, pe.ID)
	if err != nil {
		return nil, nil, err
	}
	allAgents := make([]string, 0, len(quotas))
	for _, q := range quotas {
		allAgents = append(allAgents, q.AgentID)
	}

	// agentClears[agentID] = true if they cleared the admission threshold on any metric.
	agentClears := make(map[string]bool, len(allAgents))

	for _, metric := range pe.Metrics {
		if err := c.applyMetricAdmission(ctx, pe.ID, metric, agentClears); err != nil {
			c.logger.Warn("phase2: metric admission query failed, skipping metric",
				zap.String("metric", metric.Key), zap.Error(err))
		}
	}

	// If no metrics configured or Prometheus returned nothing, all agents stay active.
	var activeAgentIDs, heldAgentIDs []string
	for _, agentID := range allAgents {
		if agentClears[agentID] || len(pe.Metrics) == 0 {
			activeAgentIDs = append(activeAgentIDs, agentID)
		} else {
			heldAgentIDs = append(heldAgentIDs, agentID)
		}
	}
	return activeAgentIDs, heldAgentIDs, nil
}

// applyMetricAdmission queries Prometheus for each agent's best (max or min) value on the
// given metric and marks agents that clear the configured admission percentile.
// Direction-aware: maximize keeps agents ≥ threshold; minimize keeps agents < threshold.
func (c *Controller) applyMetricAdmission(ctx context.Context, platformExpID string, metric domain.MetricDefinition, agentClears map[string]bool) error {
	if c.metricsDBURL == "" {
		return nil
	}

	minimize := metric.Direction == "minimize"
	agg := "max"
	if minimize {
		agg = "min"
	}

	pctl := c.phase2AdmissionPercentile
	if pctl <= 0 || pctl >= 1 {
		pctl = 0.75
	}
	if minimize {
		// vals is sorted ascending below, so index pctl*len always lands on the "top pctl%"
		// from the low end. For minimize metrics "top" means lowest values, i.e. we still want
		// the low end of the ascending array — but at the (1-pctl) index instead of pctl, so
		// the same top-quartile-admits framing works for both directions without a second sort.
		pctl = 1.0 - pctl
	}

	// Query Prometheus for each agent's best value on this metric.
	// max_over_time/min_over_time are needed because Pushgateway stores only the latest
	// pushed value per label set; the Prometheus timeseries tracks the scrape history,
	// so a range query recovers the historical best.
	promQL := fmt.Sprintf(`%s by (agent_id) (%s_over_time(experiment_metric_value{platform_experiment_id=%q, metric_name=%q}[24h]))`,
		agg, agg, platformExpID, metric.Key)
	agentBest, err := c.queryPrometheusAgentValues(ctx, promQL)
	if err != nil {
		return fmt.Errorf("prometheus query: %w", err)
	}
	if len(agentBest) == 0 {
		return nil
	}

	vals := make([]float64, 0, len(agentBest))
	for _, v := range agentBest {
		vals = append(vals, v)
	}
	sort.Float64s(vals)
	idx := int(pctl * float64(len(vals)))
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	threshold := vals[idx]

	for agentID, best := range agentBest {
		clears := best >= threshold
		if minimize {
			clears = best <= threshold
		}
		if clears {
			agentClears[agentID] = true
		}
	}
	return nil
}

// queryPrometheusAgentValues executes a PromQL instant query and returns a map of
// agent_id → float64 value from the result vector.
func (c *Controller) queryPrometheusAgentValues(ctx context.Context, promQL string) (map[string]float64, error) {
	return metricsdb.QueryAgentValues(ctx, c.metricsDBURL, promQL)
}

// applyPhase2Hold stops held agents' jobs, returns their reservations, and redistributes
// their remaining quota equally across active agents.
func (c *Controller) applyPhase2Hold(ctx context.Context, pe *domain.PlatformExperiment, heldAgentIDs, activeAgentIDs []string, runningExps []*domain.Experiment) error {
	// 1. Collect held agents' remaining quota to redistribute — one query for all.
	allQuotas, err := c.phase2Store.ListAgentQuotas(ctx, pe.ID)
	if err != nil {
		c.logger.Error("phase2: list agent quotas", zap.String("pe", pe.ID), zap.Error(err))
		allQuotas = nil
	}
	if err := metricsdb.PopulateUsage(ctx, c.metricsDBURL, pe.ID, allQuotas); err != nil {
		c.logger.Error("phase2: populate usage", zap.String("pe", pe.ID), zap.Error(err))
	}
	quotaByAgent := make(map[string]*domain.AgentQuota, len(allQuotas))
	for _, q := range allQuotas {
		q := q
		quotaByAgent[q.AgentID] = q
	}

	// The 60% of the budget withheld at experiment start is released to active agents here.
	// Only real (guaranteed) hours are redistributed — burst is a virtual overcommit limit,
	// not physical compute. Adding it would inflate active agents' allocations beyond the
	// actual remaining budget. GPU-hours drives the phase-2 trigger itself and always
	// redistributes; CPU/RAM/storage redistribute too, for any platform experiment that
	// also tracks them (0 budget = skipped, same "not tracked" convention as elsewhere).
	boundary := c.phase2BoundaryFrac
	if boundary <= 0 {
		boundary = phase2BoundaryFraction
	}

	c.redistributeResource(ctx, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceGPUHours, pe.BudgetT4Hours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedT4Hours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedT4H },
	)
	c.redistributeResource(ctx, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceCPUCoreHours, pe.BudgetCPUCoreHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedCPUCoreHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedCPUCoreH },
	)
	c.redistributeResource(ctx, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceRAMGBHours, pe.BudgetRAMGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedRAMGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedRAMGBH },
	)
	c.redistributeResource(ctx, pe, heldAgentIDs, activeAgentIDs, quotaByAgent, boundary,
		domain.ResourceStorageGBHours, pe.BudgetStorageGBHours,
		func(q *domain.AgentQuota) float64 { return q.GuaranteedStorageGBHours },
		func(q *domain.AgentQuota) float64 { return q.UsedGuaranteedStorageGBH },
	)

	// 2. Stop running jobs for held agents (once, not per-dimension).
	for _, agentID := range heldAgentIDs {
		if err := c.stopHeldAgentJobs(ctx, agentID, pe.ID, runningExps); err != nil {
			c.logger.Error("phase2: stop jobs", zap.String("agent", agentID), zap.Error(err))
		}
	}

	return nil
}

// stopHeldAgentJobs terminates all non-terminal jobs for a held agent.
func (c *Controller) stopHeldAgentJobs(ctx context.Context, agentID, platformExpID string, runningExps []*domain.Experiment) error {
	// Stop running jobs (refund unused T4h).
	running, err := c.phase2Store.GetAgentRunningExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get running: %w", err)
	}
	now := time.Now().UTC()
	for _, exp := range running {
		var refunds []db.ResourceRefund
		if exp.EstimatedDurationHours > 0 && exp.PlatformExperimentID != "" {
			hours, err := c.observedElapsedHours(ctx, exp.ID, now)
			if err != nil {
				c.logger.Error("stopHeldAgentJobs: observed elapsed hours", zap.String("id", exp.ID), zap.Error(err))
			} else {
				gpuCost, err := c.observedGPUCost(ctx, exp.ID, exp.GPUCount, now)
				if err != nil {
					c.logger.Error("stopHeldAgentJobs: observed GPU cost", zap.String("id", exp.ID), zap.Error(err))
				} else {
					// resourceRefunds writes the final *observed* cost (fraction actually
					// consumed), not the unused remainder — a phase2 hold still owes the
					// researcher whatever they genuinely ran, same as every other eviction path.
					refunds = resourceRefunds(exp, hours/exp.EstimatedDurationHours, gpuCost)
				}
			}
		}
		updated, err := c.phase2Store.TransitionAndRefund(ctx, exp.ID, domain.StatusRunning, domain.StatusEvicted,
			string(domain.EvictionPhase2Hold), agentID, platformExpID, refunds)
		if err != nil {
			c.logger.Error("phase2 hold: evict running", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionPhase2Hold)).Inc()
		// Status is already EVICTED above — the cluster-agent's next reconcile pass removes
		// the Job on its own.
		c.logger.Info("phase2 hold: stopped running job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	// Cancel queued/submitted jobs (return reservations).
	preRun, err := c.phase2Store.GetAgentQueuedExperiments(ctx, agentID, platformExpID)
	if err != nil {
		return fmt.Errorf("get queued: %w", err)
	}
	for _, exp := range preRun {
		var refunds []db.ResourceRefund
		if exp.PlatformExperimentID != "" {
			// Never reached RUNNING: observed cost is 0 across every dimension, not the estimate
			// — same as job_watcher.go's onStuckPending path for the same situation.
			refunds = resourceRefunds(exp, 0, 0)
		}
		updated, err := c.phase2Store.TransitionAndRefund(ctx, exp.ID, exp.Status, domain.StatusRejected,
			string(domain.EvictionPhase2Hold), agentID, platformExpID, refunds)
		if err != nil {
			c.logger.Error("phase2 hold: reject queued", zap.String("id", exp.ID), zap.Error(err))
			continue
		}
		if !updated {
			continue
		}
		obsmetrics.EvictedExperimentsTotal.WithLabelValues(string(domain.EvictionPhase2Hold)).Inc()
		// Status is already updated above (away from QUEUED/SUBMITTED) — if a Job existed
		// for this experiment, it disappears on the cluster-agent's next reconcile pass.
		c.logger.Info("phase2 hold: cancelled pre-run job", zap.String("exp", exp.ID), zap.String("agent", agentID))
	}

	if c.loop != nil {
		c.loop.Trigger()
	}
	return nil
}

// redistributeResource zeroes out held agents' guaranteed allocation for one resource
// dimension and redistributes the released amount equally across active agents. No-op if
// budget is 0 (platform experiment doesn't track this dimension).
func (c *Controller) redistributeResource(
	ctx context.Context,
	pe *domain.PlatformExperiment,
	heldAgentIDs, activeAgentIDs []string,
	quotaByAgent map[string]*domain.AgentQuota,
	boundary float64,
	resourceType domain.ResourceType,
	budget float64,
	guaranteedOf, usedOf func(*domain.AgentQuota) float64,
) {
	if budget <= 0 {
		return
	}
	totalRemaining := budget * (1.0 - boundary)
	for _, agentID := range heldAgentIDs {
		if q, ok := quotaByAgent[agentID]; ok {
			if rem := guaranteedOf(q) - usedOf(q); rem > 0 {
				totalRemaining += rem
			}
		}
		if err := c.phase2Store.ZeroAgentGuaranteedQuota(ctx, agentID, pe.ID, resourceType); err != nil {
			c.logger.Error("phase2: zero quota", zap.String("agent", agentID), zap.String("resource", string(resourceType)), zap.Error(err))
		}
	}

	if len(activeAgentIDs) == 0 || totalRemaining <= 0 {
		return
	}
	perAgent := totalRemaining / float64(len(activeAgentIDs))
	sort.Strings(activeAgentIDs) // deterministic order
	for _, agentID := range activeAgentIDs {
		if err := c.phase2Store.AddToAgentGuaranteedQuota(ctx, agentID, pe.ID, resourceType, perAgent); err != nil {
			c.logger.Error("phase2: redistribute quota", zap.String("agent", agentID), zap.String("resource", string(resourceType)), zap.Error(err))
		}
	}
	c.logger.Info("phase2: quota redistributed",
		zap.String("pe", pe.ID),
		zap.String("resource", string(resourceType)),
		zap.Float64("total_redistributed", totalRemaining),
		zap.Float64("per_active_agent", perAgent),
		zap.Int("active_count", len(activeAgentIDs)),
	)
}
