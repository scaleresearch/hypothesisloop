package metricsdb

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// PopulateJobMetrics sets Metric on every job of exps that reported its platform experiment's
// first ranking metric: the job's best value, the aggregation the standings rank with
// (BestPerAgentOnMetric), raw basis before any other. One query per platform experiment among
// exps. definitions returns the declared metrics of one.
func PopulateJobMetrics(ctx context.Context, dbURL string,
	definitions func(ctx context.Context, platformExperimentID string) ([]domain.MetricDefinition, bool, error),
	exps []*domain.Experiment) error {
	byPlatformExperiment := map[string][]*domain.Experiment{}
	for _, exp := range exps {
		if exp.PlatformExperimentID != "" {
			byPlatformExperiment[exp.PlatformExperimentID] = append(byPlatformExperiment[exp.PlatformExperimentID], exp)
		}
	}
	for id, jobs := range byPlatformExperiment {
		defs, _, err := definitions(ctx, id)
		if err != nil {
			return fmt.Errorf("metricsdb.PopulateJobMetrics: %w", err)
		}
		metric, ok := domain.PrimaryRankingMetric(defs)
		if !ok {
			continue
		}
		agg := "max"
		if metric.Direction == "minimize" {
			agg = "min"
		}
		byID := make(map[string]*domain.Experiment, len(jobs))
		ids := make([]string, len(jobs))
		oldest := jobs[0].CreatedAt
		for i, job := range jobs {
			byID[job.ID] = job
			ids[i] = regexp.QuoteMeta(job.ID)
			if job.CreatedAt.Before(oldest) {
				oldest = job.CreatedAt
			}
		}
		promQL := fmt.Sprintf(`%s by (job_id, metric_basis) (%s_over_time(%s{platform_experiment_id=%q, metric_name=%q, job_id=~%q}[%s]))`,
			agg, agg, ExperimentMetricValue, id, metric.Key, strings.Join(ids, "|"), ObservedLookback(oldest))
		samples, err := QueryVector(ctx, dbURL, promQL)
		if err != nil {
			return fmt.Errorf("metricsdb.PopulateJobMetrics: %s: %w", metric.Key, err)
		}
		for _, sm := range samples {
			job := byID[sm.Labels["job_id"]]
			if job == nil {
				continue
			}
			basis := sm.Labels["metric_basis"]
			if basis == "" {
				basis = "raw"
			}
			got := &domain.JobMetric{Name: metric.Key, Best: sm.Value, Basis: basis}
			if cur := job.Metric; cur != nil {
				better := got.Best > cur.Best
				if metric.Direction == "minimize" {
					better = got.Best < cur.Best
				}
				if raw, curRaw := got.Basis == "raw", cur.Basis == "raw"; raw != curRaw && curRaw || raw == curRaw && !better {
					continue
				}
			}
			job.Metric = got
		}
	}
	return nil
}
