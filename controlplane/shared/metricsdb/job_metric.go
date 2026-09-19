package metricsdb

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// PopulateJobMetrics sets Metrics on every job of exps: per declared metric of its platform
// experiment that the job reported, the best and the last value, raw basis before any other.
// Metric is the one on the first ranking metric, whose best is the aggregation the standings
// rank with (BestPerAgentOnMetric). Three queries per platform experiment among exps, whatever
// the number of jobs. definitions returns the declared metrics of one.
func PopulateJobMetrics(ctx context.Context, dbURL string,
	definitions func(ctx context.Context, platformExperimentID string) ([]domain.MetricDefinition, bool, error),
	exps []*domain.Experiment) error {
	byPlatformExperiment := map[string][]*domain.Experiment{}
	for _, exp := range exps {
		if exp.PlatformExperimentID != "" {
			byPlatformExperiment[exp.PlatformExperimentID] = append(byPlatformExperiment[exp.PlatformExperimentID], exp)
		}
	}
	type series struct{ job, metric, basis string }
	for id, jobs := range byPlatformExperiment {
		defs, _, err := definitions(ctx, id)
		if err != nil {
			return fmt.Errorf("metricsdb.PopulateJobMetrics: %w", err)
		}
		if len(defs) == 0 {
			continue
		}
		primary, _ := domain.PrimaryRankingMetric(defs)
		names := make([]string, len(defs))
		for i, def := range defs {
			names[i] = regexp.QuoteMeta(def.Key)
		}
		ids := make([]string, len(jobs))
		oldest := jobs[0].CreatedAt
		for i, job := range jobs {
			ids[i] = regexp.QuoteMeta(job.ID)
			if job.CreatedAt.Before(oldest) {
				oldest = job.CreatedAt
			}
		}
		// Each (job, metric, basis) is one series, so the outer aggregation only drops labels.
		read := func(outer, over string) (map[series]float64, error) {
			promQL := fmt.Sprintf(`%s by (job_id, metric_name, metric_basis) (%s_over_time(%s{platform_experiment_id=%q, metric_name=~%q, job_id=~%q}[%s]))`,
				outer, over, ExperimentMetricValue, id, strings.Join(names, "|"), strings.Join(ids, "|"), ObservedLookback(oldest))
			samples, err := QueryVector(ctx, dbURL, promQL)
			if err != nil {
				return nil, fmt.Errorf("metricsdb.PopulateJobMetrics: %w", err)
			}
			values := make(map[series]float64, len(samples))
			for _, sm := range samples {
				basis := sm.Labels["metric_basis"]
				if basis == "" {
					basis = "raw"
				}
				values[series{sm.Labels["job_id"], sm.Labels["metric_name"], basis}] = sm.Value
			}
			return values, nil
		}
		highest, err := read("max", "max")
		if err != nil {
			return err
		}
		lowest, err := read("min", "min")
		if err != nil {
			return err
		}
		last, err := read("max", "last")
		if err != nil {
			return err
		}
		minimize := map[string]bool{}
		for _, def := range defs {
			minimize[def.Key] = def.Direction == "minimize"
		}
		chosen := map[[2]string]domain.JobMetric{}
		for s, value := range last {
			got := domain.JobMetric{Name: s.metric, Best: highest[s], Last: value, Basis: s.basis}
			if minimize[s.metric] {
				got.Best = lowest[s]
			}
			if cur, ok := chosen[[2]string{s.job, s.metric}]; ok {
				better := got.Best > cur.Best
				if minimize[s.metric] {
					better = got.Best < cur.Best
				}
				if cur.Basis == "raw" || got.Basis != "raw" && !better {
					continue
				}
			}
			chosen[[2]string{s.job, s.metric}] = got
		}
		for _, job := range jobs {
			for _, def := range defs {
				got, ok := chosen[[2]string{job.ID, def.Key}]
				if !ok {
					continue
				}
				job.Metrics = append(job.Metrics, got)
				if def.Key == primary.Key {
					job.Metric = &got
				}
			}
		}
	}
	return nil
}
