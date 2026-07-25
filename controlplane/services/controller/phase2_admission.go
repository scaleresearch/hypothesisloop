package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// ErrPhase2MetricsUnavailable signals that no configured metric query returned usable data,
// so the phase-2 admission decision cannot be trusted this cycle. Callers must postpone the
// transition rather than treat every agent as held.
var ErrPhase2MetricsUnavailable = errors.New("phase2: no metric produced usable admission data")

// computePhase2Admission queries Prometheus to determine which agents clear the
// configured percentile on at least one metric. Returns (activeAgentIDs, heldAgentIDs, error).
// Returns ErrPhase2MetricsUnavailable if metrics are configured but every query errored or
// returned no data — the threshold is always drawn from an actual observed value, so any
// query that does return data guarantees at least one agent clears; an empty active set can
// therefore only happen when data was unavailable, never as a legitimate all-held outcome.
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

	var anyMetricHealthy bool
	for _, metric := range pe.Metrics {
		hadData, err := c.applyMetricAdmission(ctx, pe.ID, metric, agentClears)
		if err != nil {
			c.logger.Warn("phase2: metric admission query failed, skipping metric",
				zap.String("metric", metric.Key), zap.Error(err))
			continue
		}
		if hadData {
			anyMetricHealthy = true
		}
	}

	// Metrics are configured but none returned usable data — postpone rather than hold everyone.
	if len(pe.Metrics) > 0 && !anyMetricHealthy {
		return nil, nil, ErrPhase2MetricsUnavailable
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
// Returns hadData=true only if the query succeeded and returned at least one agent value.
func (c *Controller) applyMetricAdmission(ctx context.Context, platformExpID string, metric domain.MetricDefinition, agentClears map[string]bool) (bool, error) {
	if c.metricsDBURL == "" {
		return false, nil
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
		return false, fmt.Errorf("prometheus query: %w", err)
	}
	if len(agentBest) == 0 {
		return false, nil
	}

	vals := make([]float64, 0, len(agentBest))
	for _, v := range agentBest {
		vals = append(vals, v)
	}
	sort.Float64s(vals)

	// A metric on which every agent scores identically cannot rank anyone: the threshold is
	// drawn from the observed spread, so a zero spread admits the whole field on every sweep.
	// That is indistinguishable from a healthy all-clear here, so say so — it is almost always
	// a saturated/clamped metric definition rather than a real tie (see the normalized
	// val_accuracy ceiling removed from tests/workloads/tenstorrent/train.py).
	if len(vals) > 1 && vals[0] == vals[len(vals)-1] {
		c.logger.Warn("phase2: metric has zero spread across agents, cannot rank — check for a saturated or clamped metric definition",
			zap.String("platform_experiment_id", platformExpID),
			zap.String("metric", metric.Key),
			zap.Float64("value", vals[0]),
			zap.Int("agents", len(vals)))
	}

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
	return true, nil
}

// queryPrometheusAgentValues executes a PromQL instant query and returns a map of
// agent_id → float64 value from the result vector.
func (c *Controller) queryPrometheusAgentValues(ctx context.Context, promQL string) (map[string]float64, error) {
	return metricsdb.QueryAgentValues(ctx, c.metricsDBURL, promQL)
}
