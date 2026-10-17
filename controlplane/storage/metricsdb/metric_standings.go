package metricsdb

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AgentBest is one agent's best value on one metric, and the job that produced it.
type AgentBest struct {
	Value        float64
	ExperimentID string
}

// RankableJobs narrows completed to the jobs whose values rank: those that hold every constraint
// metric of pe (UnrankableJobs).
func (s *Store) RankableJobs(ctx context.Context, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]bool, error) {
	unrankable, err := s.UnrankableJobs(ctx, pe, completed)
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
func (s *Store) UnrankableJobs(ctx context.Context, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]string, error) {
	unrankable, _, err := s.judgeConstraints(ctx, pe, completed)
	return unrankable, err
}

// Whether a job held every constraint metric of its platform experiment (ConstraintsMet).
const (
	ConstraintsHeld    = "true"
	ConstraintsBroken  = "false"
	ConstraintsUnknown = "unknown"
)

// ConstraintsMet says of each job of completed whether it held every constraint metric of pe:
// ConstraintsBroken when one is violated, ConstraintsUnknown when none is and one was not
// reported, ConstraintsHeld otherwise.
func (s *Store) ConstraintsMet(ctx context.Context, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]string, error) {
	unrankable, broken, err := s.judgeConstraints(ctx, pe, completed)
	if err != nil {
		return nil, err
	}
	met := make(map[string]string, len(completed))
	for job := range completed {
		switch {
		case broken[job]:
			met[job] = ConstraintsBroken
		case unrankable[job] != "":
			met[job] = ConstraintsUnknown
		default:
			met[job] = ConstraintsHeld
		}
	}
	return met, nil
}

// judgeConstraints gives UnrankableJobs' reasons, and the jobs of completed that violate a
// constraint whatever their first reason is.
func (s *Store) judgeConstraints(ctx context.Context, pe *domain.PlatformExperiment, completed map[string]bool) (map[string]string, map[string]bool, error) {
	unrankable, broken := map[string]string{}, map[string]bool{}
	for _, metric := range pe.Metrics {
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		promQL := fmt.Sprintf(`max by (job_id, metric_basis) (last_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
			ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
		samples, err := s.QueryVector(ctx, promQL)
		if err != nil {
			return nil, nil, fmt.Errorf("metricsdb.UnrankableJobs: %s: %w", metric.Key, err)
		}
		judgeConstraint(metric, samples, completed, unrankable, broken)
	}
	return unrankable, broken, nil
}

// judgeConstraint judges each job of completed on one constraint metric from its samples, one
// per job and basis, into unrankable and broken. A job that has a reason already keeps its first.
func judgeConstraint(metric domain.MetricDefinition, samples []VectorSample, completed map[string]bool, unrankable map[string]string, broken map[string]bool) {
	reported, violated := map[string]bool{}, map[string]float64{}
	for _, sm := range samples {
		if basis := sm.Labels["metric_basis"]; basis != "" && basis != platformapi.BasisRaw {
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
		value, broke := violated[job]
		if broke {
			broken[job] = true
		}
		if _, judged := unrankable[job]; judged {
			continue
		}
		if broke {
			unrankable[job] = fmt.Sprintf("constraint %s is %g, and must be %s %g", metric.Key, value, bound, *metric.Bound)
		} else if !reported[job] {
			unrankable[job] = fmt.Sprintf("constraint %s was not reported, and must be %s %g", metric.Key, bound, *metric.Bound)
		}
	}
}

// JobValue is one job's last "raw"-basis value on one metric.
type JobValue struct {
	AgentID      string
	ExperimentID string
	Value        float64
}

// LastPerJobOnMetric is the last "raw"-basis value on metric of every job of completed that
// reported it, whatever constraints the job held, ordered by agent and job.
func (s *Store) LastPerJobOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, completed map[string]bool) ([]JobValue, error) {
	promQL := fmt.Sprintf(`max by (agent_id, metric_basis, job_id) (last_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
		ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
	samples, err := s.QueryVector(ctx, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LastPerJobOnMetric: %s: %w", metric.Key, err)
	}
	var out []JobValue
	seen := map[string]bool{}
	for _, sm := range samples {
		job := sm.Labels["job_id"]
		// A job's samples from before metric_basis existed carry none: one value per job stands.
		if basis := sm.Labels["metric_basis"]; !completed[job] || seen[job] || (basis != "" && basis != platformapi.BasisRaw) {
			continue
		}
		seen[job] = true
		out = append(out, JobValue{AgentID: sm.Labels["agent_id"], ExperimentID: job, Value: sm.Value})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentID != out[j].AgentID {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].ExperimentID < out[j].ExperimentID
	})
	return out, nil
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
func (s *Store) BestPerAgentOnMetric(ctx context.Context, pe *domain.PlatformExperiment, metric domain.MetricDefinition, completed map[string]bool) (map[string]AgentBest, []string, error) {
	rankable, err := s.RankableJobs(ctx, pe, completed)
	if err != nil {
		return nil, nil, err
	}
	// last is a range function and no aggregation operator. Each (agent, basis, job) is one
	// series, so the outer aggregation only drops labels.
	promQL := fmt.Sprintf(`max by (agent_id, metric_basis, job_id) (last_over_time(%s{platform_experiment_id=%q, metric_name=%q}[%s]))`,
		ExperimentMetricValue, pe.ID, metric.Key, ObservedLookback(pe.CreatedAt))
	samples, err := s.QueryVector(ctx, promQL)
	if err != nil {
		return nil, nil, fmt.Errorf("metricsdb.BestPerAgentOnMetric: %s: %w", metric.Key, err)
	}
	return bestOf(metric, samples, rankable)
}

// bestOf is each agent's best on metric over the rankable jobs among samples, one per agent,
// basis and job, and the agents that reported it only on another basis.
func bestOf(metric domain.MetricDefinition, samples []VectorSample, rankable map[string]bool) (map[string]AgentBest, []string, error) {
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
		if basis := sm.Labels["metric_basis"]; basis != "" && basis != platformapi.BasisRaw {
			nonRaw[agentID] = true
			continue
		}
		cand := AgentBest{Value: sm.Value, ExperimentID: sm.Labels["job_id"]}
		cur, exists := best[agentID]
		if !exists || betterOn(metric, cand, cur) {
			best[agentID] = cand
		}
	}
	return best, sortedKeys(nonRaw), nil
}

// Ranking is what one platform experiment ranks by: one of its metrics over its COMPLETED jobs.
type Ranking struct {
	PE        *domain.PlatformExperiment
	Metric    domain.MetricDefinition
	Completed map[string]bool
}

// BestPerAgentOnMetrics is BestPerAgentOnMetric of each of runs, keyed by platform experiment, in
// two queries whatever their number: one for every constraint metric of them all and one for
// every metric they rank by, each grouped by platform experiment and metric besides and judged
// and ranked per run as BestPerAgentOnMetric does.
func (s *Store) BestPerAgentOnMetrics(ctx context.Context, runs []Ranking) (map[string]map[string]AgentBest, error) {
	out := make(map[string]map[string]AgentBest, len(runs))
	if len(runs) == 0 {
		return out, nil
	}
	var ids, ranked, constraints []string
	oldest := runs[0].PE.CreatedAt
	for _, run := range runs {
		ids, ranked = append(ids, run.PE.ID), append(ranked, run.Metric.Key)
		for _, metric := range run.PE.Metrics {
			if metric.EffectiveRole() == domain.MetricRoleConstraint {
				constraints = append(constraints, metric.Key)
			}
		}
		if run.PE.CreatedAt.Before(oldest) {
			oldest = run.PE.CreatedAt
		}
	}
	byRunAndMetric := func(names []string, by string) (map[[2]string][]VectorSample, error) {
		if len(names) == 0 {
			return nil, nil
		}
		promQL := fmt.Sprintf(`max by (platform_experiment_id, metric_name, %s) (last_over_time(%s{platform_experiment_id=~%q, metric_name=~%q}[%s]))`,
			by, ExperimentMetricValue, anyOf(ids), anyOf(names), ObservedLookback(oldest))
		samples, err := s.QueryVector(ctx, promQL)
		if err != nil {
			return nil, err
		}
		grouped := map[[2]string][]VectorSample{}
		for _, sm := range samples {
			key := [2]string{sm.Labels["platform_experiment_id"], sm.Labels["metric_name"]}
			grouped[key] = append(grouped[key], sm)
		}
		return grouped, nil
	}
	constraintSamples, err := byRunAndMetric(constraints, "job_id, metric_basis")
	if err != nil {
		return nil, fmt.Errorf("metricsdb.BestPerAgentOnMetrics: constraints: %w", err)
	}
	rankedSamples, err := byRunAndMetric(ranked, "agent_id, metric_basis, job_id")
	if err != nil {
		return nil, fmt.Errorf("metricsdb.BestPerAgentOnMetrics: %w", err)
	}
	for _, run := range runs {
		unrankable, broken := map[string]string{}, map[string]bool{}
		for _, metric := range run.PE.Metrics {
			if metric.EffectiveRole() == domain.MetricRoleConstraint {
				judgeConstraint(metric, constraintSamples[[2]string{run.PE.ID, metric.Key}], run.Completed, unrankable, broken)
			}
		}
		rankable := maps.Clone(run.Completed)
		for job := range unrankable {
			delete(rankable, job)
		}
		best, _, err := bestOf(run.Metric, rankedSamples[[2]string{run.PE.ID, run.Metric.Key}], rankable)
		if err != nil {
			return nil, fmt.Errorf("metricsdb.BestPerAgentOnMetrics: %s: %w", run.PE.ID, err)
		}
		out[run.PE.ID] = best
	}
	return out, nil
}

// anyOf is a regular expression matching exactly one of values, as PromQL anchors it.
func anyOf(values []string) string {
	quoted := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			quoted = append(quoted, regexp.QuoteMeta(v))
		}
	}
	return strings.Join(quoted, "|")
}

// betterOn reports whether a beats b on metric. Exact ties fall
// back to the lower job id: two jobs of one agent can legitimately hit the same best value, and
// without a tiebreak the winning value's reported job — and the code ref shown alongside it —
// would be whichever sample the backend listed first, changing between identical reads.
//
// Every sample reaching here already passed QueryVector's own finite-value check (see
// metricsdb.go), so a and b are never NaN/Inf — a malformed sample fails the whole query with an
// error long before it could reach a per-agent comparison.
func betterOn(metric domain.MetricDefinition, a, b AgentBest) bool {
	if a.Value == b.Value {
		return a.ExperimentID < b.ExperimentID
	}
	return metric.Better(a.Value, b.Value)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
