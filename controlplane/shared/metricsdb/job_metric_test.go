package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// jobMetricServer answers the max, min and last query each with its own samples.
func jobMetricServer(t *testing.T, bodies map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query := r.Form.Get("query")
		queries = append(queries, query)
		w.Header().Set("Content-Type", "application/json")
		for over, samples := range bodies {
			if strings.Contains(query, over+"_over_time(") {
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + samples + `]}}`))
			}
		}
	}))
	return server, &queries
}

// A job is read by one value on each declared metric, which the metric's role decides: its best
// in the metric's direction where it ranks, and its last where a bound is held against it or
// nothing judges it. A timer a job reports as it runs is a constraint to minimise whose lowest
// value is its first tick, and that was once shown as the job's best.
func TestPopulateJobMetricsSetsTheOneValueItsRoleDecidesOnEveryDeclaredMetric(t *testing.T) {
	sample := func(job, metric, basis, value string) string {
		return `{"metric":{"job_id":"` + job + `","metric_name":"` + metric + `","metric_basis":"` + basis + `"},"value":[1,"` + value + `"]}`
	}
	server, queries := jobMetricServer(t, map[string]string{
		"max": strings.Join([]string{sample("job-a", "auroc", "raw", "0.90"),
			sample("job-b", "auroc", "rescaled", "0.99"), sample("job-b", "auroc", "raw", "0.50"), sample("job-other", "auroc", "raw", "0.10")}, ","),
		"min": sample("job-a", "loss", "raw", "0.20"),
		"last": strings.Join([]string{sample("job-a", "train_seconds", "raw", "7.88"), sample("job-a", "kind", "", "2"),
			sample("job-b", "kind", "per_token", "3"), sample("job-b", "kind", "bucketed", "4")}, ","),
	})
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
	if err := PopulateJobMetrics(context.Background(), server.URL, definitions, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	auroc := domain.JobMetric{Name: "auroc", Value: 0.90, Basis: "raw"}
	if got := jobs[0]; got.Metric == nil || *got.Metric != auroc || !slices.Equal(got.Metrics, []domain.JobMetric{
		{Name: "train_seconds", Value: 7.88, Basis: "raw"}, auroc, {Name: "loss", Value: 0.20, Basis: "raw"}, {Name: "kind", Value: 2, Basis: "raw"}}) {
		t.Fatalf("job-a: metric = %+v, metrics = %+v, want the last train_seconds, the highest auroc, the lowest loss and the last kind, in declared order", got.Metric, got.Metrics)
	}
	if got := jobs[1]; got.Metric == nil || *got.Metric != (domain.JobMetric{Name: "auroc", Value: 0.50, Basis: "raw"}) ||
		!slices.Equal(got.Metrics, []domain.JobMetric{*got.Metric, {Name: "kind", Value: 4, Basis: "bucketed"}}) {
		t.Fatalf("job-b: metric = %+v, metrics = %+v, want its raw 0.50 ahead of the rescaled 0.99, and of two other bases the first by name", got.Metric, got.Metrics)
	}
	if jobs[2].Metrics != nil || jobs[3].Metrics != nil {
		t.Fatalf("a job that reported nothing got metrics: %+v %+v", jobs[2].Metrics, jobs[3].Metrics)
	}
	if len(*queries) != 3 {
		t.Fatalf("queries = %d, want one per rule whatever the number of jobs", len(*queries))
	}
	for over, names := range map[string]string{"max": "auroc", "min": "loss", "last": "train_seconds|kind"} {
		i := slices.IndexFunc(*queries, func(q string) bool { return strings.Contains(q, over+"_over_time(") })
		if i < 0 {
			t.Fatalf("no %s_over_time query among %q", over, *queries)
		}
		query := (*queries)[i]
		assertGroupedBy(t, query, "job_id", "metric_name", "metric_basis")
		for _, want := range []string{`metric_name=~"` + names + `"`, `job_id=~"job-a|job-b|job\\.silent"`} {
			if !strings.Contains(query, want) {
				t.Fatalf("query %q does not contain %q", query, want)
			}
		}
	}
}

func TestPopulateJobMetricsAsksNothingWithoutDeclaredMetrics(t *testing.T) {
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
