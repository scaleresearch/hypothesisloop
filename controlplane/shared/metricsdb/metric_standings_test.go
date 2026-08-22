package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// vectorServer answers every instant query with body and records the PromQL sent, so a test can
// pin the query shape (the by-clause grouping) as well as how the response is interpreted — a
// fake that only replayed fixed series would keep passing even if the query stopped grouping by
// job_id or metric_basis.
func vectorServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotQuery = r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return server, &gotQuery
}

func TestBestPerAgentOnMetricPicksTheBestAcrossMultipleJobsOfOneAgent(t *testing.T) {
	// One agent, three distinct job_id series (see BestPerAgentOnMetric's grouping-by-job_id
	// doc comment) — the exact shape confirmed against live GreptimeDB data, where a single
	// agent had seven concurrent job series for one metric.
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent_id":"a1","job_id":"job-low"},"value":[1,"0.10"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-high"},"value":[1,"0.90"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-mid"},"value":[1,"0.50"]},` +
		`{"metric":{"agent_id":"a2","job_id":"job-only"},"value":[1,"0.30"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, nonRaw, err := BestPerAgentOnMetric(context.Background(), server.URL, pe, metric)
	if err != nil {
		t.Fatalf("BestPerAgentOnMetric: %v", err)
	}
	if len(nonRaw) != 0 {
		t.Fatalf("nonRaw = %v, want none", nonRaw)
	}
	if got := best["a1"]; got.Value != 0.90 || got.ExperimentID != "job-high" {
		t.Fatalf("a1 best = %+v, want value=0.90 job=job-high (its winner across 3 jobs)", got)
	}
	if got := best["a2"]; got.Value != 0.30 || got.ExperimentID != "job-only" {
		t.Fatalf("a2 best = %+v, want value=0.30 job=job-only", got)
	}
	assertGroupedBy(t, *gotQuery, "agent_id", "metric_basis", "job_id")
}

func TestBestPerAgentOnMetricPicksTheLowerJobIDOnAnExactTie(t *testing.T) {
	// Two jobs of one agent hit the identical best value — the winner must be deterministic
	// (see betterOn's doc comment), not dependent on GreptimeDB's own result ordering.
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent_id":"a1","job_id":"job-z"},"value":[1,"0.87"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-a"},"value":[1,"0.87"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, _, err := BestPerAgentOnMetric(context.Background(), server.URL, pe, metric)
	if err != nil {
		t.Fatalf("BestPerAgentOnMetric: %v", err)
	}
	if got := best["a1"].ExperimentID; got != "job-a" {
		t.Fatalf("tie winner job = %q, want job-a (lower id, independent of result order)", got)
	}
	assertGroupedBy(t, *gotQuery, "agent_id", "metric_basis", "job_id")
}

func TestBestPerAgentOnMetricExcludesNonRawBasisFromRanking(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent_id":"a1","job_id":"job-1","metric_basis":"rescaled"},"value":[1,"999"]},` +
		`{"metric":{"agent_id":"a2","job_id":"job-2"},"value":[1,"0.5"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, nonRaw, err := BestPerAgentOnMetric(context.Background(), server.URL, pe, metric)
	if err != nil {
		t.Fatalf("BestPerAgentOnMetric: %v", err)
	}
	if _, ranked := best["a1"]; ranked {
		t.Fatalf("a1 was ranked with value %+v despite reporting only a non-raw basis sample", best["a1"])
	}
	if len(nonRaw) != 1 || nonRaw[0] != "a1" {
		t.Fatalf("nonRaw = %v, want [a1]", nonRaw)
	}
	if got := best["a2"].Value; got != 0.5 {
		t.Fatalf("a2 best = %v, want 0.5", got)
	}
	assertGroupedBy(t, *gotQuery, "agent_id", "metric_basis", "job_id")
}

// assertGroupedBy fails the test unless the query's `by (...)` clause names exactly the given
// labels — the query shape BestPerAgentOnMetric's own doc comment claims to send. Response-driven
// assertions alone would keep passing even if the production code stopped asking GreptimeDB to
// aggregate per label at all.
func assertGroupedBy(t *testing.T, query string, labels ...string) {
	t.Helper()
	want := "by (" + strings.Join(labels, ", ") + ")"
	if !strings.Contains(query, want) {
		t.Fatalf("query %q does not contain %q — grouping changed underneath the merge logic", query, want)
	}
}
