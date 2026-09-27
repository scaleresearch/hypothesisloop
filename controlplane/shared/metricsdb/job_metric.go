package metricsdb

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// PopulateJobMetrics sets Metrics on every job of exps: per declared metric of its platform
// experiment that the job reported, the one value the job is read by, which the metric's role
// decides (domain.MetricDefinition.JobValueRule), raw basis before any other. Metric is the one
// on the first ranking metric, the value the standings rank with (BestPerAgentOnMetric). One
// query per rule among the declared metrics of each platform experiment among exps, whatever the
// number of jobs. definitions returns the declared metrics of one.
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
		if len(defs) == 0 {
			continue
		}
		primary, _ := domain.PrimaryRankingMetric(defs)
		namesByRule := map[string][]string{}
		for _, def := range defs {
			namesByRule[def.JobValueRule()] = append(namesByRule[def.JobValueRule()], regexp.QuoteMeta(def.Key))
		}
		ids := make([]string, len(jobs))
		oldest := jobs[0].CreatedAt
		for i, job := range jobs {
			ids[i] = regexp.QuoteMeta(job.ID)
			if job.CreatedAt.Before(oldest) {
				oldest = job.CreatedAt
			}
		}
		chosen := map[[2]string]domain.JobMetric{}
		for _, rule := range []string{domain.JobValueMax, domain.JobValueMin, domain.JobValueLast} {
			names := namesByRule[rule]
			if len(names) == 0 {
				continue
			}
			// Each (job, metric, basis) is one series, so the outer aggregation only drops labels.
			promQL := fmt.Sprintf(`max by (job_id, metric_name, metric_basis) (%s_over_time(%s{platform_experiment_id=%q, metric_name=~%q, job_id=~%q}[%s]))`,
				rule, ExperimentMetricValue, id, strings.Join(names, "|"), strings.Join(ids, "|"), ObservedLookback(oldest))
			samples, err := QueryVector(ctx, dbURL, promQL)
			if err != nil {
				return fmt.Errorf("metricsdb.PopulateJobMetrics: %w", err)
			}
			for _, sm := range samples {
				got := domain.JobMetric{Name: sm.Labels["metric_name"], Value: sm.Value, Basis: sm.Labels["metric_basis"]}
				if got.Basis == "" {
					got.Basis = "raw"
				}
				key := [2]string{sm.Labels["job_id"], got.Name}
				if cur, ok := chosen[key]; ok && (cur.Basis == "raw" || got.Basis != "raw" && cur.Basis < got.Basis) {
					continue
				}
				chosen[key] = got
			}
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
