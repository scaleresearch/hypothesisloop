package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// jobMetricServer answers a query that reads each job's last value with samples, and any other
// with none.
func jobMetricServer(t *testing.T, samples string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query := r.Form.Get("query")
		queries = append(queries, query)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(query, "(last_over_time(") {
			samples = ""
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + samples + `]}}`))
	}))
	return server, &queries
}

// A job is read by one value on each declared metric, the last it reported. Read by its best
// point where the metric ranks, a job stood at the luckiest evaluation of a noisy curve, and a
// baseline declared from one stood higher than its code measures.
func TestPopulateJobMetricsSetsTheLastValueOnEveryDeclaredMetric(t *testing.T) {
	sample := func(job, metric, basis, value string) string {
		return `{"metric":{"job_id":"` + job + `","metric_name":"` + metric + `","metric_basis":"` + basis + `"},"value":[1,"` + value + `"]}`
	}
	server, queries := jobMetricServer(t, strings.Join([]string{sample("job-a", "auroc", "raw", "0.90"),
		sample("job-b", "auroc", "rescaled", "0.99"), sample("job-b", "auroc", "raw", "0.50"), sample("job-other", "auroc", "raw", "0.10"),
		sample("job-a", "loss", "raw", "0.20"),
		sample("job-a", "train_seconds", "raw", "7.88"), sample("job-a", "kind", "raw", "2"),
		sample("job-b", "kind", "per_token", "3"), sample("job-b", "kind", "bucketed", "4")}, ","))
	defer server.Close()

	created := time.Now().Add(-time.Hour)
	jobs := []*domain.Experiment{
		{ID: "job-a", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job-b", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job.silent", PlatformExperimentID: "pe-1", CreatedAt: created},
		{ID: "job-unscoped", CreatedAt: created},
	}
	bound := 60.0
	definitions := func(context.Context, string) ([]domain.MetricDefinition, bool, error) {
		return []domain.MetricDefinition{
			{Key: "train_seconds", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
			{Key: "auroc", Direction: "maximize"},
			{Key: "loss", Direction: "minimize", Role: domain.MetricRoleRanking},
			{Key: "kind", Direction: "maximize", Role: domain.MetricRoleAttribute},
		}, true, nil
	}
	if err := New(server.URL).PopulateJobMetrics(context.Background(), definitions, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	auroc := domain.JobMetric{Name: "auroc", Value: 0.90, Basis: "raw"}
	if got := jobs[0]; got.Metric == nil || *got.Metric != auroc || !slices.Equal(got.Metrics, []domain.JobMetric{
		{Name: "train_seconds", Value: 7.88, Basis: "raw"}, auroc, {Name: "loss", Value: 0.20, Basis: "raw"}, {Name: "kind", Value: 2, Basis: "raw"}}) {
		t.Fatalf("job-a: metric = %+v, metrics = %+v, want the last value of each, in declared order", got.Metric, got.Metrics)
	}
	if got := jobs[1]; got.Metric == nil || *got.Metric != (domain.JobMetric{Name: "auroc", Value: 0.50, Basis: "raw"}) ||
		!slices.Equal(got.Metrics, []domain.JobMetric{*got.Metric, {Name: "kind", Value: 4, Basis: "bucketed"}}) {
		t.Fatalf("job-b: metric = %+v, metrics = %+v, want its raw 0.50 ahead of the rescaled 0.99, and of two other bases the first by name", got.Metric, got.Metrics)
	}
	if jobs[2].Metrics != nil || jobs[3].Metrics != nil {
		t.Fatalf("a job that reported nothing got metrics: %+v %+v", jobs[2].Metrics, jobs[3].Metrics)
	}
	if len(*queries) != 1 {
		t.Fatalf("queries = %q, want one whatever the number of jobs and metrics", *queries)
	}
	query := (*queries)[0]
	assertGroupedBy(t, query, "job_id", "metric_name", "metric_basis")
	for _, want := range []string{"(last_over_time(", `metric_name=~"train_seconds|auroc|loss|kind"`, `job_id=~"job-a|job-b|job\\.silent"`} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q does not contain %q", query, want)
		}
	}
}

func TestPopulateJobMetricsAsksNothingWithoutDeclaredMetrics(t *testing.T) {
	server, gotQuery := vectorServer(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	defer server.Close()
	jobs := []*domain.Experiment{{ID: "job-a", PlatformExperimentID: "pe-gone"}}
	none := func(context.Context, string) ([]domain.MetricDefinition, bool, error) { return nil, false, nil }
	if err := New(server.URL).PopulateJobMetrics(context.Background(), none, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	if *gotQuery != "" || jobs[0].Metric != nil {
		t.Fatalf("query %q, metric %+v: want neither", *gotQuery, jobs[0].Metric)
	}
}
