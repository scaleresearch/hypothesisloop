package metricsdb

import (
	"context"
	"fmt"
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// AgentBest is one agent's best value on one metric, and the job that produced it.
type AgentBest struct {
	Value        float64
	ExperimentID string
}

// BestPerAgentOnMetric returns, for every agent in platformExpID, its best "raw"-basis value on
// one metric, plus the agents that only ever reported it on some other basis.
//
// This is the single definition of "who is ahead". The stage ladder cuts with it and the final
// standings rank with it, and the two ran as separately-built copies of the same PromQL — a
// divergence hazard both copies carried a comment warning about. A run's mid-run cuts and its
// final results disagreeing about who is winning is the exact failure that motivated folding them
// into one function.
//
// Three things the query has to do, none of them incidental:
//
//   - The window is the platform experiment's own lifetime, never a constant. A fixed bound made
//     an agent whose best result was recorded earlier read as "no data", which sorts to the bottom
//     of the cut order — eliminated for having peaked early.
//   - min/max_over_time, not a bare selector: Pushgateway stores only the latest pushed value per
//     label set, so recovering the historical best needs the range.
//   - Grouped by metric_basis, never collapsed out. A rescaled/non-"raw" value blended into the
//     same aggregation as a raw one is the scale mismatch that made a ~20% "win" actually ~2x
//     worse. Non-raw agents are reported separately, never ranked.
//
// job_id is in the grouping so the winning value carries the job that produced it (and with it the
// code ref). It does not change who wins: the per-agent best is still the same direction-aware
// aggregation, just taken over each agent's per-job bests.
func BestPerAgentOnMetric(ctx context.Context, dbURL string, pe *domain.PlatformExperiment, metric domain.MetricDefinition) (map[string]AgentBest, []string, error) {
	agg := "max"
	if metric.Direction == "minimize" {
		agg = "min"
	}
	promQL := fmt.Sprintf(`%s by (agent_id, metric_basis, job_id) (%s_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
		agg, agg, ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, nil, fmt.Errorf("metricsdb.BestPerAgentOnMetric: %s: %w", metric.Key, err)
	}

	best := make(map[string]AgentBest, len(samples))
	nonRaw := make(map[string]bool)
	for _, sm := range samples {
		agentID := sm.Labels["agent_id"]
		if agentID == "" {
			return nil, nil, fmt.Errorf("metricsdb.BestPerAgentOnMetric: %s: result missing agent_id", metric.Key)
		}
		// Two label-distinct series can both mean "raw": samples written before this label
		// existed carry no metric_basis at all ("") alongside samples written after that
		// explicitly say "raw". Both are eligible and must be merged with the same
		// direction-aware aggregation as everything else, not treated as a conflict.
		if basis := sm.Labels["metric_basis"]; basis != "" && basis != "raw" {
			nonRaw[agentID] = true
			continue
		}
		cand := AgentBest{Value: sm.Value, ExperimentID: sm.Labels["job_id"]}
		cur, exists := best[agentID]
		if !exists || betterOn(metric.Direction, cand, cur) {
			best[agentID] = cand
		}
	}
	return best, sortedKeys(nonRaw), nil
}

// betterOn reports whether a beats b for a metric ranked in the given direction. Exact ties fall
// back to the lower job id: two jobs of one agent can legitimately hit the same best value, and
// without a tiebreak the winning value's reported job — and the code ref shown alongside it —
// would be whichever sample the backend listed first, changing between identical reads.
//
// Every sample reaching here already passed QueryVector's own finite-value check (see
// metricsdb.go), so a and b are never NaN/Inf — a malformed sample fails the whole query with an
// error long before it could reach a per-agent comparison.
func betterOn(direction string, a, b AgentBest) bool {
	if a.Value == b.Value {
		return a.ExperimentID < b.ExperimentID
	}
	if direction == "minimize" {
		return a.Value < b.Value
	}
	return a.Value > b.Value
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
