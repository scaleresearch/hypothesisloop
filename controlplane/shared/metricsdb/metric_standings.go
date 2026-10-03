package metricsdb

import (
	"context"
	"fmt"
	"maps"
	"sort"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// AgentBest is one agent's best value on one metric, and the job that produced it.
type AgentBest struct {
	Value        float64
	ExperimentID string
}

// RankableJobs narrows completed to the jobs whose values rank: those that hold every constraint
// metric of pe (UnrankableJobs).
func RankableJobs(ctx context.Context, dbURL string, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]bool, error) {
	unrankable, err := UnrankableJobs(ctx, dbURL, pe, completed)
	if err != nil {
		return nil, err
	}
	rankable := maps.Clone(completed)
	for job := range unrankable {
		delete(rankable, job)
	}
	return rankable, nil
}

// UnrankableJobs gives, for each job of completed that ranks nowhere, the first constraint
// metric of pe it does not hold and how. A constraint is judged per job, on the "raw"-basis
// value the job is read by on it (domain.JobMetric), its last, the one the proposal check reads
// of a baseline. A job that reported none, or whose value violates the bound, ran to the end and
// found its result wrong: it stays a completed trial and none of its values rank.
//
// Judged per agent, on the agent's best value of the constraint, a job that broke the gate with
// the best number of the run ranked first as long as another job of its agent held the gate. So
// a harness had to fail such a job to keep it out, and a trial that found the gate broken was
// no completed trial.
func UnrankableJobs(ctx context.Context, dbURL string, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]string, error) {
	unrankable := map[string]string{}
	for _, metric := range pe.Metrics {
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		promQL := fmt.Sprintf(`max by (job_id, metric_basis) (last_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
			ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
		samples, err := QueryVector(ctx, dbURL, promQL)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.UnrankableJobs: %s: %w", metric.Key, err)
		}
		reported, violated := map[string]bool{}, map[string]float64{}
		for _, sm := range samples {
			if basis := sm.Labels["metric_basis"]; basis != "" && basis != "raw" {
				continue
			}
			job := sm.Labels["job_id"]
			reported[job] = true
			if !metric.Holds(sm.Value) {
				violated[job] = sm.Value
			}
		}
		bound := "at least"
		if metric.Direction == "minimize" {
			bound = "at most"
		}
		for job := range completed {
			if _, judged := unrankable[job]; judged {
				continue
			}
			if value, broke := violated[job]; broke {
				unrankable[job] = fmt.Sprintf("constraint %s is %g, and must be %s %g", metric.Key, value, bound, *metric.Bound)
			} else if !reported[job] {
				unrankable[job] = fmt.Sprintf("constraint %s was not reported, and must be %s %g", metric.Key, bound, *metric.Bound)
			}
		}
	}
	return unrankable, nil
}

// BestPerAgentOnMetric returns, for every agent in platformExpID, its best "raw"-basis value on
// one metric over the rankable jobs in completed, plus the agents that only ever reported it on
// some other basis. completed is the platform experiment's COMPLETED jobs as the relational
// store has them: a job that failed or was stopped reported numbers and proved nothing, so none
// of its values rank. Of those, only the ones that hold every constraint rank (RankableJobs),
// which is read here so that no ranking can leave it out: an agent has a best exactly when it
// has a rankable job.
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
//   - A range function, not a bare selector: a job that ended long ago reports nothing now, so
//     its last value is read over the range.
//   - Grouped by metric_basis, never collapsed out. A rescaled/non-"raw" value blended into the
//     same aggregation as a raw one is the scale mismatch that made a ~20% "win" actually ~2x
//     worse. Non-raw agents are reported separately, never ranked.
//
// job_id is in the grouping so the winning value carries the job that produced it (and with it the
// code ref). Each job is read by the last value it reported (domain.JobMetric), and the agent's
// best is the best of those in the metric's direction.
func BestPerAgentOnMetric(ctx context.Context, dbURL string, pe *domain.PlatformExperiment, metric domain.MetricDefinition, completed map[string]bool) (map[string]AgentBest, []string, error) {
	rankable, err := RankableJobs(ctx, dbURL, pe, completed)
	if err != nil {
		return nil, nil, err
	}
	// last is a range function and no aggregation operator. Each (agent, basis, job) is one
	// series, so the outer aggregation only drops labels.
	promQL := fmt.Sprintf(`max by (agent_id, metric_basis, job_id) (last_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
		ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, nil, fmt.Errorf("metricsdb.BestPerAgentOnMetric: %s: %w", metric.Key, err)
	}

	best := make(map[string]AgentBest, len(samples))
	nonRaw := make(map[string]bool)
	for _, sm := range samples {
		if !rankable[sm.Labels["job_id"]] {
			continue
		}
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
