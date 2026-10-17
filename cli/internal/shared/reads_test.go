package shared

import (
	"bytes"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestQuota(t *testing.T) {
	got := clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		row := map[string]any{"id": "q", "agent_id": "a", "platform_experiment_id": "pe-1",
			"share_accelerator_hours": 0.8, "used_guaranteed_acch": 0.1, "remaining_burst_accelerator_hours": 0, "created_at": "t"}
		if strings.HasSuffix(r.Path, "/quotas") {
			return []any{row, row}
		}
		return row
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/a"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Quota, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	// What the platform answers is printed as it is, a remaining of 0 included: hl computes no
	// figure of its own that could differ from what a submit counts.
	if want := `{"agent_id":"a","remaining_burst_accelerator_hours":0,"share_accelerator_hours":0.8,"used_guaranteed_acch":0.1}` + "\n"; (*got)[0].Path != "/platform-experiments/pe-1/quotas/a" || stdout.String() != want {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
	stdout.Reset()
	if code := clitest.Run(Quota, []string{"--all"}, &stdout, &stderr); code != 0 || (*got)[1].Path != "/platform-experiments/pe-1/quotas" || len(clitest.Lines(stdout.String())) != 2 {
		t.Fatalf("--all: exit %d, request %+v, stdout %q", code, (*got)[1], stdout.String())
	}
}

func TestResultsFlattensStandings(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"status": "running", "metrics": []any{map[string]any{"metric": "acc", "direction": "maximize",
			"standings": []any{map[string]any{"rank": 1, "agent_id": "a", "best": 0.9, "env": map[string]any{"LR": "0.3"}}, map[string]any{"rank": 2, "agent_id": "b", "best": 0.8}}}}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Results, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"metric":"acc","direction":"maximize","agent_id":"a","best":0.9,"rank":1,"job":{"env":{"LR":"0.3"}}}`,
		`{"metric":"acc","direction":"maximize","agent_id":"b","best":0.8,"rank":2,"job":{"env":{}}}`,
	}
	if (*got)[0].Path != "/platform-experiments/pe-1/results" || !reflect.DeepEqual(clitest.Lines(stdout.String()), want) {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

// The bar the standings are set against prints first, and each standing says whether it beats it.
func TestResultsPrintTheBaselineBeforeTheStandings(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"status": "running", "metrics": []any{map[string]any{"metric": "acc", "direction": "maximize", "baseline": 0.85, "baseline_job_id": "job-b",
			"standings": []any{map[string]any{"rank": 1, "agent_id": "a", "best": 0.9, "beats_baseline": true}}}}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Results, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"metric":"acc","direction":"maximize","baseline":0.85,"baseline_job_id":"job-b"}`,
		`{"metric":"acc","direction":"maximize","agent_id":"a","beats_baseline":true,"best":0.9,"rank":1,"job":{"env":{}}}`,
	}
	if !reflect.DeepEqual(clitest.Lines(stdout.String()), want) {
		t.Fatalf("stdout %q, want %q", stdout.String(), want)
	}
}

// A reference prints after the standings with where it was measured, then set against the
// leader, and one nobody measured says so.
func TestResultsPrintEachReferenceOrThatItWasNotMeasured(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"status": "running", "metrics": []any{}, "references": []any{
			map[string]any{"metric": "other_acc", "direction": "maximize", "values": []any{
				map[string]any{"agent_id": "base", "value": 0.7, "experiment_id": "job-1", "ran_on": map[string]any{"node": "n1"}, "constraints_met": "true"},
				map[string]any{"agent_id": "base", "value": 0.8, "experiment_id": "job-2", "constraints_met": "false"}},
				"best": 0.7, "leader_metric": "acc", "leader": 0.6, "beaten": false},
			map[string]any{"metric": "third_acc", "direction": "maximize", "values": []any{}},
		}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Results, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"reference":"other_acc","direction":"maximize","agent_id":"base","constraints_met":"true","experiment_id":"job-1","ran_on":{"node":"n1"},"value":0.7}`,
		`{"reference":"other_acc","direction":"maximize","agent_id":"base","constraints_met":"false","experiment_id":"job-2","value":0.8}`,
		`{"reference":"other_acc","direction":"maximize","beaten":false,"best":0.7,"leader":0.6,"leader_metric":"acc"}`,
		`{"reference":"third_acc","direction":"maximize","measured":"not measured"}`,
	}
	if !reflect.DeepEqual(clitest.Lines(stdout.String()), want) {
		t.Fatalf("stdout %q, want %q", stdout.String(), want)
	}
}

// A metric nobody has a result on yet is an answer: no line, a notice that is no error, exit 0.
func TestResultsWithNoStandingYetIsAnAnswer(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"status": "running", "metrics": []any{map[string]any{"metric": "images_per_sec", "direction": "maximize", "standings": []any{}}}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Results, []string{}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.String() != "hl: no results yet on images_per_sec\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestCapacityOneLinePerAcceleratorType(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"clusters": []any{map[string]any{"cluster_name": "local", "accelerators": []any{
			map[string]any{"accelerator_type": "L40", "available": 8, "total": 8},
			map[string]any{"accelerator_type": "H100", "available": 0, "total": 16},
		}}}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Capacity, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"cluster":"local","accelerator_type":"L40","available":8,"total":8}`,
		`{"cluster":"local","accelerator_type":"H100","available":0,"total":16}`,
	}
	if !reflect.DeepEqual(clitest.Lines(stdout.String()), want) {
		t.Fatalf("stdout %q", stdout.String())
	}
}

// The CPU type counts cores, and its line says so. The line of an accelerator is as it was.
func TestCapacitySaysCoresForTheCPUType(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"clusters": []any{map[string]any{"cluster_name": "box", "accelerators": []any{
			map[string]any{"accelerator_type": "hypothesisloop.dev/cpu=core", "kind": "cpu", "available": 12, "total": 16, "per_node_available": 8, "acch_per_hour": 1},
			map[string]any{"accelerator_type": "tenstorrent.com/chipArch=blackhole", "kind": "accelerator", "available": 3, "total": 4, "acch_per_hour": 0.5},
		}}}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Capacity, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := []string{
		`{"cluster":"box","accelerator_type":"hypothesisloop.dev/cpu=core","acch_per_core_hour":1,"cores_available":12,"kind":"cpu","per_node_cores_available":8,"cores_total":16}`,
		`{"cluster":"box","accelerator_type":"tenstorrent.com/chipArch=blackhole","acch_per_hour":0.5,"available":3,"kind":"accelerator","total":4}`,
	}
	if !reflect.DeepEqual(clitest.Lines(stdout.String()), want) {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestJobStatsAndListDefaultToThePlatformExperiment(t *testing.T) {
	got := clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		if r.Path == "/experiments" {
			return []any{}
		}
		return map[string]any{"total": 3, "by_status": map[string]any{"COMPLETED": 3}, "evictions_by_reason": map[string]any{}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobStats, []string{"--agent", "a"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Path != "/experiments/stats" || (*got)[0].Query != "agent_id=a&platform_experiment_id=pe-1" || stdout.String() != `{"by_status":{"COMPLETED":3},"total":3}`+"\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
	if code := clitest.Run(JobList, []string{"--status", "RUNNING", "--limit", "50"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[1].Query != "limit=50&platform_experiment_id=pe-1&status=RUNNING" {
		t.Fatalf("job list query = %q", (*got)[1].Query)
	}
}

func TestJobLogsPrintsPlainLines(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return []string{"step 1", "step 2"} })
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobLogs, []string{"j", "--lines", "5"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Path != "/experiments/j/logs" || (*got)[0].Query != "n=5" || stdout.String() != "step 1\nstep 2\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

func TestHypothesisQueryTakesCypherFlag(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"columns": []string{"n"}, "rows": [][]any{{1}}} })
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(HypothesisQuery, []string{"--platform-experiment", "pe-1", "--cypher", "RETURN 1 AS n"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Body["cypher"] != "RETURN 1 AS n" || stdout.String() != `{"n":1}`+"\n" {
		t.Fatalf("request %+v, stdout %q", (*got)[0], stdout.String())
	}
}

func TestJobListAndHypothesisGetPrintBriefJobsWithTheirMetricsAndTheEnvTheyVaried(t *testing.T) {
	job := func(id string, env map[string]any) map[string]any {
		return map[string]any{"id": id, "agent_id": "a", "hypothesis_id": "hyp-1", "status": "COMPLETED", "hypothesis": "long text",
			"metric": map[string]any{"name": "acc", "value": 0.9, "basis": "raw"}, "phase_detail": map[string]any{}, "theory": "a > b",
			"metrics": []any{map[string]any{"name": "acc", "value": 0.9, "basis": "raw"},
				map[string]any{"name": "loss", "value": 0.2608416958176093, "basis": "per_token"}},
			"estimated_cost_acch": 0.5, "created_at": "2026-10-04T00:00:00Z", "resolved_job": map[string]any{"image": "i"},
			"job": map[string]any{"image": "i", "env": env},
			"ran_on": map[string]any{"node": "rtx4090-0", "machine": map[string]any{"instance_id": "54493630", "host_id": "16358",
				"accelerator_model": "RTX 4090", "cpu_model": "AMD Ryzen", "price_per_hour_usd": 0.43}}}
	}
	jobs := []any{job("job-1", map[string]any{"GIT_TOKEN": "secret"}), job("job-2", map[string]any{"LR": "0.3", "GIT_TOKEN": "secret"})}
	clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		if r.Path == "/experiments" {
			return jobs
		}
		return map[string]any{"id": "hyp-1", "jobs": jobs, "job_count": 5}
	})
	brief := func(id, env string) string {
		return `{"id":"` + id + `","agent_id":"a","hypothesis_id":"hyp-1","status":"COMPLETED","metrics":{"acc":{"value":0.9},"loss":{"value":0.260842,"basis":"per_token"}},` +
			`"ran_on":"host 16358 RTX 4090","theory":"a > b","estimated_cost_acch":0.5,"created_at":"2026-10-04T00:00:00Z","job":{"env":` + env + `}}`
	}
	both := []string{brief("job-1", `{}`), brief("job-2", `{"LR":"0.3"}`)}
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobList, []string{}, &stdout, &stderr); code != 0 || !reflect.DeepEqual(clitest.Lines(stdout.String()), both) || stderr.Len() != 0 {
		t.Fatalf("job list: exit %d, stdout %q, stderr %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	want := `{"id":"hyp-1","job_count":5,"jobs_omitted":3,"jobs":[` + strings.Join(both, ",") + "]}\n"
	if code := clitest.Run(HypothesisGet, []string{"hyp-1", "--full"}, &stdout, &stderr); code != 0 || stdout.String() != want {
		t.Fatalf("hypothesis get: exit %d, stdout %q, stderr %s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("hypothesis get stderr = %q, want nothing: jobs_omitted says what the page left out", stderr.String())
	}
}

func TestAWholeListSaysNothingOfPaging(t *testing.T) {
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "2")
		_, _ = w.Write([]byte(`[{"id":"x-1"},{"id":"x-2"}]`))
	}))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobList, []string{}, &stdout, &stderr); code != 0 || len(clitest.Lines(stdout.String())) != 2 || stderr.Len() != 0 {
		t.Fatalf("job list: exit %d, stdout %q, stderr %q, want two lines and no notice", code, stdout.String(), stderr.String())
	}
}

func TestUntakenAsksForTheHypothesesNoAgentWorksOn(t *testing.T) {
	var query url.Values
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hypotheses" {
			query = r.URL.Query()
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(HypothesisList, []string{"--platform-experiment", "pe-1", "--untaken", "--by", "people", "--status", "open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if query.Get("untaken") != "true" || query.Get("by") != "people" || query.Get("status") != "open" {
		t.Errorf("query = %v, want untaken, by and status together", query)
	}
}
