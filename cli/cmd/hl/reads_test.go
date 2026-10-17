package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestCapacityNamesTheOrgItIsToldAndAsksNothingElse(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"clusters": []any{}} })
	t.Setenv("HL_ORG", "")
	for _, tc := range []struct {
		env  string
		args []string
		want string
	}{
		{"", []string{"capacity"}, ""},
		{"org-env", []string{"capacity"}, "org_id=org-env"},
		{"org-env", []string{"capacity", "--org", "org flag"}, "org_id=org+flag"},
	} {
		t.Setenv("HL_ORG", tc.env)
		before := len(*got)
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("hl %v: exit %d, stderr %s", tc.args, code, stderr.String())
		}
		if made := (*got)[before:]; len(made) != 1 || made[0].Path != "/resource-catalog/capacity" || made[0].Query != tc.want {
			t.Fatalf("hl %v with HL_ORG=%q made %+v, want one GET of capacity with query %q", tc.args, tc.env, made, tc.want)
		}
	}
}

// A coordinator's script read d['env'] off hl job status after reading it off hl results and
// died on a KeyError: a job's env is in one place in everything hl prints of a job.
func TestEveryCommandThatPrintsAJobHasItsEnvUnderJob(t *testing.T) {
	job := map[string]any{"id": "job-1", "status": "COMPLETED", "job": map[string]any{"env": map[string]any{"LR": "0.3"}}}
	clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		switch r.Path {
		case "/experiments":
			return []any{job}
		case "/experiments/job-1":
			return job
		case "/hypotheses/hyp-1":
			return map[string]any{"id": "hyp-1", "jobs": []any{job}, "job_count": 1}
		}
		return map[string]any{"metrics": []any{map[string]any{"metric": "acc", "direction": "maximize",
			"standings": []any{map[string]any{"rank": 1, "experiment_id": "job-1", "env": map[string]any{"LR": "0.3"}}}}}}
	})
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	for _, command := range [][]string{{"job", "status", "job-1"}, {"job", "list"}, {"results"}, {"hypothesis", "get", "hyp-1"}, {"hypothesis", "get", "hyp-1", "--full"}} {
		var stdout, stderr bytes.Buffer
		if code := run(command, &stdout, &stderr); code != 0 {
			t.Fatalf("hl %s: exit %d, stderr %s", strings.Join(command, " "), code, stderr.String())
		}
		var printed struct {
			Env  any `json:"env"`
			Job  jobEnv
			Jobs []struct{ Job jobEnv }
		}
		if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil {
			t.Fatalf("hl %s printed %q: %v", strings.Join(command, " "), stdout.String(), err)
		}
		if len(printed.Jobs) == 1 {
			printed.Job = printed.Jobs[0].Job
		}
		if printed.Job.Env["LR"] != "0.3" || printed.Env != nil {
			t.Errorf("hl %s printed %s, want the env under job.env and nowhere else", strings.Join(command, " "), stdout.String())
		}
	}
}

type jobEnv struct {
	Env map[string]string `json:"env"`
}

func TestAListSaysHowManyRowsThePageLeftOut(t *testing.T) {
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Total-Count", "39")
		_, _ = w.Write([]byte(`[{"id":"x-1"},{"id":"x-2"}]`))
	})
	clitest.SetAPIURL(t, url)
	for _, tc := range []struct{ what, want string }{
		{"job", "hl: job list: 2 of 39 shown: --offset 32 for the next page\n"},
		{"hypothesis", "hl: hypothesis list: 2 of 39 shown: --offset 32 for the next page\n"},
		{"org-members", "hl: org members list: 2 of 39 shown: --offset 32 for the next page\n"},
		{"org-invites", "hl: org invites list: 2 of 39 shown: --offset 32 for the next page\n"},
		{"platform-experiments", "hl: platform-experiments list: 2 of 39 shown: --offset 32 for the next page\n"},
	} {
		args := map[string][]string{
			"org-members":          {"org", "members", "list", "--org", "org-1"},
			"org-invites":          {"org", "invites", "list", "--org", "org-1"},
			"platform-experiments": {"platform-experiments", "list"},
		}[tc.what]
		if args == nil {
			args = []string{tc.what, "list", "--platform-experiment", "pe-1"}
		}
		var stdout, stderr bytes.Buffer
		code := run(append(args, "--offset", "30"), &stdout, &stderr)
		if code != 0 || len(clitest.Lines(stdout.String())) != 2 || stderr.String() != tc.want {
			t.Fatalf("%s list: exit %d, stdout %q, stderr %q", tc.what, code, stdout.String(), stderr.String())
		}
	}
}
