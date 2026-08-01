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

// ErrStageMetricsUnavailable signals that no configured metric query returned usable data, so
// the cut cannot be trusted this cycle. Callers must postpone the boundary rather than cut.
var ErrStageMetricsUnavailable = errors.New("stages: no metric produced usable ranking data")

// minSurvivorsForCut is the roster size below which a boundary cuts nobody, and
// minSurvivorsAfterCut the floor a cut may never take the field below. Guardrails: a ladder
// that eliminates its way down to one agent has stopped comparing anything.
const (
	minSurvivorsForCut   = 5
	minSurvivorsAfterCut = 2
)

// computeCut ranks survivors on each metric independently — worst-first by their best value,
// bottom k = floor(evict% × survivors) cut — and returns (survivors, cut). Surviving on any one
// metric survives the boundary, so specialists live and no cross-metric normalization is needed.
func (c *Controller) computeCut(ctx context.Context, pe *domain.PlatformExperiment, stage domain.Stage) ([]string, []string, error) {
	survivors, err := c.currentSurvivors(ctx, pe)
	if err != nil {
		return nil, nil, err
	}

	// A stage that cuts nobody, an unrankable field (no metrics), and a field already at the
	// guardrail floor all mean the same thing here: advance, cut no one.
	if stage.EvictPct == 0 || len(pe.Metrics) == 0 || len(survivors) < minSurvivorsForCut {
		return survivors, nil, nil
	}

	// cutCount[agent] counts the metrics on which the agent fell below the line. An agent is
	// cut only if that is every metric that produced usable data.
	cutCount := make(map[string]int, len(survivors))
	healthyMetrics := 0
	for _, metric := range pe.Metrics {
		cutOnMetric, hadData, err := c.cutOnMetric(ctx, pe.ID, metric, stage, survivors)
		if err != nil {
			c.logger.Warn("stages: metric ranking query failed, skipping metric",
				zap.String("metric", metric.Key), zap.Error(err))
			continue
		}
		if !hadData {
			continue
		}
		healthyMetrics++
		for _, agentID := range cutOnMetric {
			cutCount[agentID]++
		}
	}
	if healthyMetrics == 0 {
		return nil, nil, ErrStageMetricsUnavailable
	}

	var kept, cut []string
	for _, agentID := range survivors {
		if cutCount[agentID] == healthyMetrics {
			cut = append(cut, agentID)
		} else {
			kept = append(kept, agentID)
		}
	}
	return kept, cut, nil
}

// currentSurvivors lists the agents still standing in a platform experiment: everyone with a
// quota, minus everyone cut at an earlier boundary. Sorted for deterministic ranking.
func (c *Controller) currentSurvivors(ctx context.Context, pe *domain.PlatformExperiment) ([]string, error) {
	quotas, err := c.stagesStore.ListAgentQuotas(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("stages: list agent quotas: %w", err)
	}
	cuts, err := c.stagesStore.ListCutAgents(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("stages: list cut agents: %w", err)
	}
	isCut := make(map[string]bool, len(cuts))
	for _, cut := range cuts {
		isCut[cut.AgentID] = true
	}
	survivors := make([]string, 0, len(quotas))
	for _, q := range quotas {
		if !isCut[q.AgentID] {
			survivors = append(survivors, q.AgentID)
		}
	}
	sort.Strings(survivors)
	return survivors, nil
}

// ranked is one survivor's standing on a single metric. hasData=false ranks below every agent
// with data, and all such agents tie with each other.
type ranked struct {
	agentID string
	value   float64
	hasData bool
}

// cutOnMetric returns the agents cut on one metric, and whether the metric produced usable data.
func (c *Controller) cutOnMetric(ctx context.Context, platformExpID string, metric domain.MetricDefinition, stage domain.Stage, survivors []string) ([]string, bool, error) {
	if c.metricsDBURL == "" {
		return nil, false, nil
	}

	agg := "max"
	if metric.Direction == "minimize" {
		agg = "min"
	}
	// max_over_time/min_over_time are needed because Pushgateway stores only the latest pushed
	// value per label set; the Prometheus timeseries tracks the scrape history, so a range query
	// recovers the historical best.
	promQL := fmt.Sprintf(`%s by (agent_id) (%s_over_time(experiment_metric_value{platform_experiment_id=%q, metric_name=%q}[24h]))`,
		agg, agg, platformExpID, metric.Key)
	best, err := metricsdb.QueryAgentValues(ctx, c.metricsDBURL, promQL)
	if err != nil {
		return nil, false, fmt.Errorf("prometheus query: %w", err)
	}
	if len(best) == 0 {
		return nil, false, nil
	}

	order := make([]ranked, 0, len(survivors))
	for _, agentID := range survivors {
		v, ok := best[agentID]
		order = append(order, ranked{agentID: agentID, value: v, hasData: ok})
	}
	return cutBottom(order, metric.Direction, stage.EvictPct), true, nil
}

// cutBottom ranks survivors worst-first and returns the bottom evictPct of them.
func cutBottom(order []ranked, direction string, evictPct float64) []string {
	// Worst-first: no-data agents, then by value — ascending for a maximize metric, descending
	// for a minimize one. Ties keep the input order so the ranking is stable.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.hasData != b.hasData {
			return !a.hasData
		}
		if !a.hasData {
			return false
		}
		if direction == "minimize" {
			return a.value > b.value
		}
		return a.value < b.value
	})

	k := int(evictPct / 100.0 * float64(len(order)))
	// Never take the field below the floor, whatever the configured share.
	if max := len(order) - minSurvivorsAfterCut; k > max {
		k = max
	}
	// A tie group straddling the cut line is kept whole: the cut takes fewer than k, never more,
	// and no agent is ever eliminated by an arbitrary tiebreak.
	for k > 0 && k < len(order) && tied(order[k-1], order[k]) {
		k--
	}
	if k <= 0 {
		return nil
	}

	cut := make([]string, 0, k)
	for _, r := range order[:k] {
		cut = append(cut, r.agentID)
	}
	return cut
}

// tied reports whether two survivors are indistinguishable on a metric. Two agents with no data
// are as tied as two agents with the same value — neither pair can be ordered by this metric.
func tied(a, b ranked) bool {
	if a.hasData != b.hasData {
		return false
	}
	if !a.hasData {
		return true
	}
	return a.value == b.value
}
