package metricsdb

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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

// allCompleted is every job these tests name as one that ran to a successful end.
var allCompleted = map[string]bool{"job-low": true, "job-high": true, "job-mid": true, "job-only": true, "job-1": true, "job-2": true, "job-a": true, "job-z": true}

func TestBestPerAgentOnMetricPicksTheBestAcrossMultipleJobsOfOneAgent(t *testing.T) {
	// One agent, three distinct job_id series (see BestPerAgentOnMetric's grouping-by-job_id
	// doc comment) — the exact shape confirmed against live GreptimeDB data, where a single
	// agent had seven concurrent job series for one metric.
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent_id":"a1","job_id":"job-low","metric_basis":"raw"},"value":[1,"0.10"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-high","metric_basis":"raw"},"value":[1,"0.90"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-mid","metric_basis":"raw"},"value":[1,"0.50"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-failed","metric_basis":"raw"},"value":[1,"0.99"]},` +
		`{"metric":{"agent_id":"a3","job_id":"job-still-running","metric_basis":"raw"},"value":[1,"0.95"]},` +
		`{"metric":{"agent_id":"a2","job_id":"job-only","metric_basis":"raw"},"value":[1,"0.30"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, nonRaw, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe, metric, allCompleted)
	if err != nil {
		t.Fatalf("BestPerAgentOnMetric: %v", err)
	}
	if len(nonRaw) != 0 {
		t.Fatalf("nonRaw = %v, want none", nonRaw)
	}
	if got := best["a1"]; got.Value != 0.90 || got.ExperimentID != "job-high" {
		t.Fatalf("a1 best = %+v, want value=0.90 job=job-high (its winner across its 3 completed jobs, the failed one left out)", got)
	}
	if _, ranked := best["a3"]; ranked {
		t.Fatal("a3 is ranked on a job that has not completed")
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
		`{"metric":{"agent_id":"a1","job_id":"job-z","metric_basis":"raw"},"value":[1,"0.87"]},` +
		`{"metric":{"agent_id":"a1","job_id":"job-a","metric_basis":"raw"},"value":[1,"0.87"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, _, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe, metric, allCompleted)
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
		`{"metric":{"agent_id":"a2","job_id":"job-2","metric_basis":"raw"},"value":[1,"0.5"]}` +
		`]}}`
	server, gotQuery := vectorServer(t, body)
	defer server.Close()

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "auroc", Direction: "maximize"}

	best, nonRaw, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe, metric, allCompleted)
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

// A job that ran to the end and broke the gate exits 0 and is COMPLETED. Judged per agent, the
// bf16 job below ranked first for a1 with a number its own gate value voids, because a1's fp16
// job held the gate. A constraint is judged per job.
func TestAJobThatBreaksAConstraintOrReportsNoneRanksNowhere(t *testing.T) {
	vector := func(samples ...string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if query := r.Form.Get("query"); strings.Contains(query, `metric_name="fp32_agreement"`) {
			if !strings.Contains(query, "by (job_id, metric_basis) (last_over_time(") {
				t.Errorf("the constraint is read as %q, want each job's last value", query)
			}
			_, _ = w.Write([]byte(vector(
				`{"metric":{"job_id":"job-fp16","metric_basis":"raw"},"value":[1,"0.998"]}`,
				`{"metric":{"job_id":"job-bf16","metric_basis":"raw"},"value":[1,"0.9844"]}`,
				`{"metric":{"job_id":"job-ok","metric_basis":"raw"},"value":[1,"1"]}`,
				`{"metric":{"job_id":"job-rescaled","metric_basis":"per_batch"},"value":[1,"1"]}`,
				`{"metric":{"job_id":"job-broken","metric_basis":"raw"},"value":[1,"0.5"]}`)))
			return
		}
		_, _ = w.Write([]byte(vector(
			`{"metric":{"agent_id":"a1","metric_basis":"raw","job_id":"job-fp16"},"value":[1,"10284"]}`,
			`{"metric":{"agent_id":"a1","metric_basis":"raw","job_id":"job-bf16"},"value":[1,"15000"]}`,
			`{"metric":{"agent_id":"a2","metric_basis":"raw","job_id":"job-ok"},"value":[1,"9000"]}`,
			`{"metric":{"agent_id":"a2","metric_basis":"raw","job_id":"job-silent"},"value":[1,"20000"]}`,
			`{"metric":{"agent_id":"a2","metric_basis":"raw","job_id":"job-rescaled"},"value":[1,"30000"]}`,
			`{"metric":{"agent_id":"a3","metric_basis":"raw","job_id":"job-broken"},"value":[1,"99999"]}`)))
	}))
	defer server.Close()

	bound := 0.99
	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "images_per_sec", Direction: "maximize"},
		{Key: "fp32_agreement", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}
	completed := map[string]bool{"job-fp16": true, "job-bf16": true, "job-ok": true, "job-silent": true, "job-rescaled": true, "job-broken": true}
	best, _, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe, pe.Metrics[0], completed)
	if err != nil {
		t.Fatal(err)
	}
	if got := best["a1"]; got.ExperimentID != "job-fp16" || got.Value != 10284 {
		t.Errorf("a1 best = %+v, want job-fp16 at 10284: its job-bf16 broke the gate", got)
	}
	if got := best["a2"]; got.ExperimentID != "job-ok" || got.Value != 9000 {
		t.Errorf("a2 best = %+v, want job-ok at 9000: one job reported no gate value and one only a rescaled one", got)
	}
	if got, ranked := best["a3"]; ranked {
		t.Errorf("a3 best = %+v, want none: an agent with no rankable job has no standing", got)
	}
	if len(completed) != 6 {
		t.Errorf("the completed jobs were narrowed in place to %v", completed)
	}
	why, err := New(server.URL).UnrankableJobs(context.Background(), pe, completed)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{
		"job-bf16":     "constraint fp32_agreement is 0.9844, and must be at least 0.99",
		"job-broken":   "constraint fp32_agreement is 0.5, and must be at least 0.99",
		"job-silent":   "constraint fp32_agreement was not reported, and must be at least 0.99",
		"job-rescaled": "constraint fp32_agreement was not reported, and must be at least 0.99",
	}; !maps.Equal(why, want) {
		t.Errorf("unrankable = %v, want %v", why, want)
	}
	met, err := New(server.URL).ConstraintsMet(context.Background(), pe, completed)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{
		"job-fp16": "true", "job-ok": "true", "job-bf16": "false", "job-broken": "false", "job-silent": "unknown", "job-rescaled": "unknown",
	}; !maps.Equal(met, want) {
		t.Errorf("constraints met = %v, want %v", met, want)
	}
	// Every completed job's value stands, whatever it held: a reference is listed even where it
	// broke a constraint, so nobody reads it as never measured.
	values, err := New(server.URL).LastPerJobOnMetric(context.Background(), pe, pe.Metrics[0], map[string]bool{"job-bf16": true, "job-silent": true, "job-broken": true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []JobValue{{"a1", "job-bf16", 15000}, {"a2", "job-silent", 20000}, {"a3", "job-broken", 99999}}; !reflect.DeepEqual(values, want) {
		t.Errorf("values = %+v, want %+v", values, want)
	}
}

// Every metric is read by each job's last value, a ranking one too: read by its best point, a
// job ranked on the luckiest evaluation of its curve. `last` is a range function only:
// `last by (...)` is no PromQL, and the metrics store refused the whole results read on it.
func TestBestPerAgentOnAnyMetricReadsEachJobsLastValue(t *testing.T) {
	bound := 0.0
	for _, metric := range []domain.MetricDefinition{
		{Key: "val_accuracy", Direction: "maximize"},
		{Key: "val_loss", Direction: "minimize", Role: domain.MetricRoleRanking},
		{Key: "digest_mismatches", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
		{Key: "invalid_outputs", Direction: "minimize", Role: domain.MetricRoleAttribute},
	} {
		var captured []string
		server := promqlMockServer(t, &captured)
		pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
		if _, _, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe, metric, nil); err != nil {
			t.Fatalf("%s: %v (sent %v)", metric.Key, err, captured)
		}
		server.Close()
		want := `max by (agent_id, metric_basis, job_id) (last_over_time(experiment_metric_value{platform_experiment_id="pe-1", metric_name="` + metric.Key + `"}[`
		if len(captured) != 1 || !strings.HasPrefix(captured[0], want) {
			t.Fatalf("%s: sent %v, want one query starting %s", metric.Key, captured, want)
		}
	}

	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"agent_id":"a1","metric_basis":"raw","job_id":"job-1"},"value":[1,"3"]},` +
		`{"metric":{"agent_id":"a1","metric_basis":"raw","job_id":"job-2"},"value":[1,"0"]}]}}`
	server, _ := vectorServer(t, body)
	defer server.Close()
	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().Add(-time.Hour)}
	best, _, err := New(server.URL).BestPerAgentOnMetric(context.Background(), pe,
		domain.MetricDefinition{Key: "invalid_outputs", Direction: "minimize", Role: domain.MetricRoleAttribute}, allCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if got := best["a1"]; got.Value != 0 || got.ExperimentID != "job-2" {
		t.Fatalf("a1 best = %+v, want job-2 at 0: the lowest of its jobs' last values", got)
	}
}

// The read of a run listed every prior run's best with queries per prior run. Their bests come
// from two queries whatever their number, judged and ranked per run as one run's are.
func TestTheBestsOfManyRunsAreReadInTwoQueries(t *testing.T) {
	vector := func(samples ...string) string {
		return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
	}
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		query := r.Form.Get("query")
		queries = append(queries, query)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(query, "by (platform_experiment_id, metric_name, job_id, metric_basis)") {
			_, _ = w.Write([]byte(vector(
				`{"metric":{"platform_experiment_id":"pe-a","metric_name":"agreement","job_id":"a-broken","metric_basis":"raw"},"value":[1,"0.5"]}`,
				`{"metric":{"platform_experiment_id":"pe-a","metric_name":"agreement","job_id":"a-ok","metric_basis":"raw"},"value":[1,"1"]}`)))
			return
		}
		_, _ = w.Write([]byte(vector(
			`{"metric":{"platform_experiment_id":"pe-a","metric_name":"auroc","agent_id":"r1","metric_basis":"raw","job_id":"a-broken"},"value":[1,"0.99"]}`,
			`{"metric":{"platform_experiment_id":"pe-a","metric_name":"auroc","agent_id":"r1","metric_basis":"raw","job_id":"a-ok"},"value":[1,"0.8"]}`,
			`{"metric":{"platform_experiment_id":"pe-a","metric_name":"loss","agent_id":"r1","metric_basis":"raw","job_id":"a-ok"},"value":[1,"0.01"]}`,
			`{"metric":{"platform_experiment_id":"pe-b","metric_name":"loss","agent_id":"r2","metric_basis":"raw","job_id":"b-1"},"value":[1,"3"]}`,
			`{"metric":{"platform_experiment_id":"pe-b","metric_name":"loss","agent_id":"r2","metric_basis":"raw","job_id":"b-2"},"value":[1,"2"]}`)))
	}))
	defer server.Close()

	bound := 0.9
	a := &domain.PlatformExperiment{ID: "pe-a", CreatedAt: time.Now().Add(-2 * time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "auroc", Direction: "maximize"},
		{Key: "agreement", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}
	b := &domain.PlatformExperiment{ID: "pe-b", CreatedAt: time.Now().Add(-time.Hour), Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}}
	best, err := New(server.URL).BestPerAgentOnMetrics(context.Background(), []Ranking{
		{PE: a, Metric: a.Metrics[0], Completed: map[string]bool{"a-broken": true, "a-ok": true}},
		{PE: b, Metric: b.Metrics[0], Completed: map[string]bool{"b-1": true, "b-2": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("%d queries, want 2 for any number of runs: %q", len(queries), queries)
	}
	for _, query := range queries {
		if !strings.Contains(query, `platform_experiment_id=~"pe-a|pe-b"`) {
			t.Errorf("query %q does not read both runs", query)
		}
	}
	want := map[string]map[string]AgentBest{
		"pe-a": {"r1": {Value: 0.8, ExperimentID: "a-ok"}},
		"pe-b": {"r2": {Value: 2, ExperimentID: "b-2"}},
	}
	if !reflect.DeepEqual(best, want) {
		t.Errorf("bests = %v, want %v: the job that broke pe-a's constraint and pe-a's other metric rank nowhere", best, want)
	}
}
