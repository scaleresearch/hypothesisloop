package metricsdb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestPopulateJobMetricsSetsEachJobsBestRankingValueInOneQuery(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"job_id":"job-a"},"value":[1,"0.90"]},` +
		`{"metric":{"job_id":"job-b","metric_basis":"rescaled"},"value":[1,"0.99"]},` +
		`{"metric":{"job_id":"job-b","metric_basis":"raw"},"value":[1,"0.50"]},` +
		`{"metric":{"job_id":"job-other"},"value":[1,"0.10"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	created := time.Now().Add(-time.Hour)
	jobs := []*domain.Experiment{
		{ID: "job-a", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job-b", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job.silent", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job-unscoped", CreatedAt: created},
	}
	definitions := func(context.Context, string) ([]domain.MetricDefinition, bool, error) {
		return []domain.MetricDefinition{
			{Key: "params", Direction: "minimize", Role: domain.MetricRoleAttribute},
			{Key: "auroc", Direction: "maximize"},
		}, true, nil
	}
	if err := PopulateJobMetrics(context.Background(), server.URL, definitions, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	if got := jobs[0].Metric; got == nil || *got != (domain.JobMetric{Name: "auroc", Best: 0.90, Basis: "raw"}) {
		t.Fatalf("job-a metric = %+v, want auroc 0.90 raw", got)
	}
	if got := jobs[1].Metric; got == nil || *got != (domain.JobMetric{Name: "auroc", Best: 0.50, Basis: "raw"}) {
		t.Fatalf("job-b metric = %+v, want its raw 0.50 ahead of the rescaled 0.99", got)
	}
	if jobs[2].Metric != nil || jobs[3].Metric != nil {
		t.Fatalf("a job that reported nothing got a metric: %+v %+v", jobs[2].Metric, jobs[3].Metric)
	}
	assertGroupedBy(t, *gotQuery, "job_id", "metric_basis")
	for _, want := range []string{`max_over_time(`, `metric_name="auroc"`, `job_id=~"job-a|job-b|job\\.silent"`} {
		if !strings.Contains(*gotQuery, want) {
			t.Fatalf("query %q does not contain %q", *gotQuery, want)
		}
	}
}

func TestPopulateJobMetricsAsksNothingWithoutARankingMetric(t *testing.T) {
	server, gotQuery := vectorServer(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	defer server.Close()
	jobs := []*domain.Experiment{{ID: "job-a", PlatformExperimentID: "pe-gone"}}
	none := func(context.Context, string) ([]domain.MetricDefinition, bool, error) { return nil, false, nil }
	if err := PopulateJobMetrics(context.Background(), server.URL, none, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	if *gotQuery != "" || jobs[0].Metric != nil {
		t.Fatalf("query %q, metric %+v: want neither", *gotQuery, jobs[0].Metric)
	}
}
