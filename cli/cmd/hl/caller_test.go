package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// With no agent named, a write to the hypothesis graph is the caller's own: the agent an agent's
// token is, and for a person or a key no agent. Nothing in the environment names either: AGENT_ID
// and PLATFORM_EXPERIMENT_ID were read once, and one left over from another run named its agent.
func TestAGraphWriteNamingNoAgentIsTheCallersOwn(t *testing.T) {
	commands := map[string][]string{
		"hypothesis submit":  {"hypothesis", "submit", "--text", "t", "--builds-on", "root"},
		"hypothesis link":    {"hypothesis", "link", "hyp-1", "--type", "related", "--to", "hyp-2"},
		"hypothesis comment": {"hypothesis", "comment", "hyp-1", "--text", "t"},
		"hypothesis status":  {"hypothesis", "status", "hyp-1", "inconclusive"},
	}
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-stale")
	t.Setenv("AGENT_ID", "ag-stale")
	for _, token := range []string{clitest.DerivedToken("agent:pe-1/ag-1"), "hlk_an-api-key"} {
		for name, args := range commands {
			got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"id": "hyp-9"} })
			t.Setenv("HL_TOKEN", token)
			if token == "hlk_an-api-key" && name == "hypothesis submit" {
				args = append(append([]string{}, args...), "--platform-experiment", "pe-1")
			}
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 || len(*got) != 1 {
				t.Fatalf("%s: exit %d with %d requests, want 0 and one. stderr: %s", name, code, len(*got), stderr.String())
			}
			body := (*got)[0].Body
			if agent, named := body["agent_id"]; named != (token != "hlk_an-api-key") || named && agent != "ag-1" {
				t.Errorf("%s: body = %v, want the token's agent, and none for a key", name, body)
			}
			if pe, stated := body["platform_experiment_id"]; stated && pe != "pe-1" {
				t.Errorf("%s: body = %v, want the token's platform experiment", name, body)
			}
			if _, stated := body["author"]; stated {
				t.Errorf("%s: body = %v, want no author", name, body)
			}
			if code := run(append(append([]string{}, args...), "--agent", "ag-7"), &stdout, &stderr); code != 0 || (*got)[1].Body["agent_id"] != "ag-7" {
				t.Errorf("%s --agent ag-7: exit %d, body = %v, want it written for ag-7", name, code, (*got)[1].Body)
			}
		}
	}
}

func TestCloseAndSummaryAreWhatACoordinatorEndsARunWith(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "ok"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "summary", "--text", "what was found"}, &stdout, &stderr); code != 0 {
		t.Fatalf("summary: exit %d, stderr %s", code, stderr.String())
	}
	file := clitest.WriteFile(t, "from a file\n")
	if code := run([]string{"platform-experiments", "summary", "--platform-experiment", "pe-2", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("summary from a file: exit %d, stderr %s", code, stderr.String())
	}
	if code := run([]string{"platform-experiments", "close"}, &stdout, &stderr); code != 0 {
		t.Fatalf("close: exit %d, stderr %s", code, stderr.String())
	}
	if code := run([]string{"platform-experiments", "close", "--reason", "the baseline measured the old split"}, &stdout, &stderr); code != 0 {
		t.Fatalf("close with a reason: exit %d, stderr %s", code, stderr.String())
	}
	// The brief is changed by naming it, and nothing else of the experiment is sent.
	if code := run([]string{"platform-experiments", "update", "--description-file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("update the description: exit %d, stderr %s", code, stderr.String())
	}
	fields := clitest.WriteFile(t, "max_agents: 4\n")
	if code := run([]string{"platform-experiments", "update", "--platform-experiment", "pe-2", "--name", "renamed", fields}, &stdout, &stderr); code != 0 {
		t.Fatalf("update fields of a named experiment: exit %d, stderr %s", code, stderr.String())
	}
	// The baseline is declared with its job alone: the platform writes it into the brief.
	if code := run([]string{"platform-experiments", "update", "--baseline-job", "job-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("declare the baseline: exit %d, stderr %s", code, stderr.String())
	}
	// Raised while the run goes on, with why.
	stderr.Reset()
	if code := run([]string{"platform-experiments", "update", "--baseline-job", "job-2", "--reason", "the control's other coder"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stderr.String(), "the baseline is raised to job") {
		t.Fatalf("raise the baseline: exit %d, stderr %s", code, stderr.String())
	}
	if code := run([]string{"platform-experiments", "update"}, &stdout, &stderr); code != 2 {
		t.Errorf("an update that names no field: exit %d, want 2", code)
	}
	want := []clitest.Recorded{
		{Method: "PUT", Path: "/platform-experiments/pe-1/summary", Body: map[string]any{"summary": "what was found"}},
		{Method: "PUT", Path: "/platform-experiments/pe-2/summary", Body: map[string]any{"summary": "from a file"}},
		{Method: "POST", Path: "/platform-experiments/pe-1/close", Body: map[string]any{}},
		{Method: "POST", Path: "/platform-experiments/pe-1/close", Body: map[string]any{"reason": "the baseline measured the old split"}},
		{Method: "PATCH", Path: "/platform-experiments/pe-1", Body: map[string]any{"description": "from a file"}},
		{Method: "PATCH", Path: "/platform-experiments/pe-2", Body: map[string]any{"name": "renamed", "max_agents": float64(4)}},
		{Method: "PATCH", Path: "/platform-experiments/pe-1", Body: map[string]any{"baseline_job_id": "job-1"}},
		{Method: "PATCH", Path: "/platform-experiments/pe-1", Body: map[string]any{"baseline_job_id": "job-2", "baseline_reason": "the control's other coder"}},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v\nwant %+v", *got, want)
	}
	for name, args := range map[string][]string{
		"neither text nor file": {"platform-experiments", "summary"},
		"both":                  {"platform-experiments", "summary", "--text", "t", "--file", file},
		"blank":                 {"platform-experiments", "summary", "--text", "  "},
	} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if len(*got) != 8 {
		t.Errorf("a refused summary was sent: %+v", *got)
	}
}
