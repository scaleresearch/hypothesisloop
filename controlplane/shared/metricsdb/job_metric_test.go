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

func TestPopulateJobMetricsSetsBestAndLastOfEveryDeclaredMetricInThreeQueries(t *testing.T) {
	sample := func(job, metric, basis, value string) string {
		return `{"metric":{"job_id":"` + job + `","metric_name":"` + metric + `","metric_basis":"` + basis + `"},"value":[1,"` + value + `"]}`
	}
	server, queries := jobMetricServer(t, map[string]string{
		"max": strings.Join([]string{sample("job-a", "auroc", "raw", "0.90"), sample("job-a", "loss", "raw", "0.70"),
			sample("job-b", "auroc", "rescaled", "0.99"), sample("job-b", "auroc", "raw", "0.50"), sample("job-other", "auroc", "raw", "0.10")}, ","),
		"min": strings.Join([]string{sample("job-a", "auroc", "raw", "0.60"), sample("job-a", "loss", "raw", "0.20"),
			sample("job-b", "auroc", "rescaled", "0.80"), sample("job-b", "auroc", "raw", "0.40"), sample("job-other", "auroc", "raw", "0.10")}, ","),
		"last": strings.Join([]string{sample("job-a", "auroc", "raw", "0.85"), sample("job-a", "loss", "raw", "0.25"),
			sample("job-b", "auroc", "rescaled", "0.95"), sample("job-b", "auroc", "raw", "0.45"), sample("job-other", "auroc", "raw", "0.10")}, ","),
	})
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
			{Key: "loss", Direction: "minimize", Role: domain.MetricRoleAttribute},
			{Key: "auroc", Direction: "maximize"},
		}, true, nil
	}
	if err := PopulateJobMetrics(context.Background(), server.URL, definitions, jobs); err != nil {
		t.Fatalf("PopulateJobMetrics: %v", err)
	}
	auroc := domain.JobMetric{Name: "auroc", Best: 0.90, Last: 0.85, Basis: "raw"}
	if got := jobs[0]; got.Metric == nil || *got.Metric != auroc ||
		!slices.Equal(got.Metrics, []domain.JobMetric{{Name: "loss", Best: 0.20, Last: 0.25, Basis: "raw"}, auroc}) {
		t.Fatalf("job-a: metric = %+v, metrics = %+v, want the lowest loss and the highest auroc, each with its last, in declared order", got.Metric, got.Metrics)
	}
	if got := jobs[1].Metric; got == nil || *got != (domain.JobMetric{Name: "auroc", Best: 0.50, Last: 0.45, Basis: "raw"}) {
		t.Fatalf("job-b metric = %+v, want its raw 0.50 ahead of the rescaled 0.99", got)
	}
	if jobs[2].Metrics != nil || jobs[3].Metrics != nil {
		t.Fatalf("a job that reported nothing got metrics: %+v %+v", jobs[2].Metrics, jobs[3].Metrics)
	}
	if len(*queries) != 3 {
		t.Fatalf("queries = %d, want 3 whatever the number of jobs", len(*queries))
	}
	for _, query := range *queries {
		assertGroupedBy(t, query, "job_id", "metric_name", "metric_basis")
		for _, want := range []string{`metric_name=~"loss|auroc"`, `job_id=~"job-a|job-b|job\\.silent"`} {
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
