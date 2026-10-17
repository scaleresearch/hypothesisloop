package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
)

func derivedToken(scope string) string {
	return "hls_" + base64.RawURLEncoding.EncodeToString([]byte(scope)) + "." + base64.RawURLEncoding.EncodeToString([]byte("mac"))
}

func TestTheKindOfCallerIsReadFromADerivedTokenAndNothingElse(t *testing.T) {
	for token, want := range map[string]callerKind{
		derivedToken("agent:pe-1/ag-1"):  callerAgent,
		derivedToken("coordinator:pe-1"): callerCoordinator,
		derivedToken("staff"):            callerOther,
		"hlk_an-api-key":                 callerOther,
		"hls_not base64.x":               callerOther,
		"hls_nodot":                      callerOther,
		"":                               callerOther,
	} {
		if got := kindOf(token); got != want {
			t.Errorf("kindOf(%q) = %v, want %v", token, got, want)
		}
	}
}

// kindOf reads the platform's own token format, so it is held to what the platform mints.
func TestTheKindOfCallerIsReadFromTheTokensThePlatformMints(t *testing.T) {
	const secret = "a-signing-secret-only-this-test-knows"
	for scope, want := range map[string]callerKind{
		scope.Agent("pe-1", "ag-1"): callerAgent,
		scope.Coordinator("pe-1"):   callerCoordinator,
		"staff":                     callerOther,
	} {
		token, err := auth.Mint(secret, scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := kindOf(token); got != want {
			t.Errorf("kindOf(the token minted for %q) = %v, want %v", scope, got, want)
		}
	}
}

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
	for _, token := range []string{derivedToken("agent:pe-1/ag-1"), "hlk_an-api-key"} {
		for name, args := range commands {
			got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "hyp-9"} })
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

func TestACoordinatorReadsTheWholeExperimentWhereAnAgentReadsItsOwn(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return []any{} })
	t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"quota"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if path := (*got)[0].Path; path != "/platform-experiments/pe-1/quotas" {
		t.Fatalf("path = %q, want every agent's quota", path)
	}
	if code := run([]string{"quota", "--agent", "ag-3"}, &stdout, &stderr); code != 0 || (*got)[1].Path != "/platform-experiments/pe-1/quotas/ag-3" {
		t.Fatalf("with --agent: exit %d, requests %+v", code, *got)
	}
}

func TestCloseAndSummaryAreWhatACoordinatorEndsARunWith(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "ok"} })
	t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "summary", "--text", "what was found"}, &stdout, &stderr); code != 0 {
		t.Fatalf("summary: exit %d, stderr %s", code, stderr.String())
	}
	file := writeFile(t, "from a file\n")
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
	fields := writeFile(t, "max_agents: 4\n")
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
	want := []recorded{
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

// A request refused for its shape is answered by the API with every field it read. Seen by an
// agent that left one field out: a page of its own request around the one line that mattered.
func TestARequestRefusedForItsShapeIsOneLineNamingTheFields(t *testing.T) {
	problem := `{"title":"Unprocessable Entity","status":422,"detail":"validation failed","errors":[` +
		`{"message":"expected required property budget_accelerator_hours to be present","location":"body","value":{"name":"x","description":"` + strings.Repeat("long ", 200) + `"}},` +
		`{"message":"expected number >= 1","location":"body.max_agents","value":0}]}`
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(problem))
	})
	setAPIURL(t, url)
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-1:ag-1"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "comment", "h-1", "--agent", "ag-1", "--text", "t"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := "HTTP 422: body: expected required property budget_accelerator_hours to be present; max_agents: expected number >= 1\n"
	if said := stderr.String(); !strings.HasSuffix(said, want) || strings.Contains(said, "long") {
		t.Errorf("stderr = %q, want it to end with %q and carry no value", said, want)
	}
}

// A planner copied cpu: 8 from the help's example and was refused 422 "expected string": the
// example, proposed as it stands, sends what the API reads.
func TestTheProposeExampleIsSentAsTheAPIReadsIt(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	var help bytes.Buffer
	cmdPlatformExperimentsPropose(nil, []string{"-h"}, &help, &help)
	_, example, _ := strings.Cut(help.String(), "\n\n  goal_version:")
	example, _, _ = strings.Cut("  goal_version:"+example, "\n\nThe proposal is checked")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "propose", writeFile(t, example)}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose the help's example: exit %d, stderr %s", code, stderr.String())
	}
	body := (*got)[0].Body
	if body["cpu"] != "8" || body["memory"] != "32Gi" || body["storage"] != "100Gi" || body["goal_version"] != float64(1) {
		t.Errorf("sent cpu %#v, memory %#v, storage %#v, goal_version %#v, want the strings \"8\", \"32Gi\" and \"100Gi\" and 1", body["cpu"], body["memory"], body["storage"], body["goal_version"])
	}
}

// A planner wrote questions: | and was refused 422 "expected array": one block is a one-item list.
// Questions are a list of a question and its answer each, which no block is.
func TestAProposalsListOfTextsMayBeOneBlock(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	file := writeFile(t, "out_of_scope: |\n  what was left out\nrisks:\n  - |\n    a risk\n")
	if code := run([]string{"platform-experiments", "propose", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr.String())
	}
	body := (*got)[0].Body
	if !reflect.DeepEqual(body["out_of_scope"], []any{"what was left out\n"}) || !reflect.DeepEqual(body["risks"], []any{"a risk\n"}) {
		t.Errorf("sent out_of_scope %#v and risks %#v, want each a list of one text", body["out_of_scope"], body["risks"])
	}
}

// A planner tried a variant by proposing a copy, which was stored and accepted. --check sends the
// same proposal for the same check with nothing stored, and a refusal prints every problem.
func TestAPlannerChecksAProposalWithoutStoringIt(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	file := writeFile(t, "experiment:\n  name: n\nbudget_accelerator_hours: 6\ncpu: 2\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "propose", "--check", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose --check: exit %d, stderr %s", code, stderr.String())
	}
	want := []recorded{{Method: "PUT", Path: "/platform-experiments/pe-env/proposal", Query: "check=true", Body: map[string]any{
		"experiment": map[string]any{"name": "n"}, "budget_accelerator_hours": float64(6), "cpu": "2",
	}}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v\nwant %+v", *got, want)
	}
	if said := stdout.String(); !strings.Contains(said, "passes every check; nothing is stored") || strings.Contains(said, "status") {
		t.Errorf("stdout = %q, want the line that it passes and nothing stored, and no draft", said)
	}

	refusal := `{"reason":"invalid_cost","message":"2 problems: ...","problems":[` +
		`{"reason":"invalid_cost","message":"budget_accelerator_hours is required and must be positive"},` +
		`{"reason":"accelerator_unreachable","message":"accelerator_type \"cpu\" is on no cluster: \"rtx-4090\""}]}`
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(refusal))
	}))
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	stdout.Reset()
	if code := run([]string{"platform-experiments", "propose", "--check", file}, &stdout, &stderr); code != 1 {
		t.Fatalf("a refused check: exit %d, want 1. stderr %s", code, stderr.String())
	}
	for _, line := range []string{"2 problems, fix them all:", "  invalid_cost: budget_accelerator_hours is required", "  accelerator_unreachable: accelerator_type \"cpu\""} {
		if !strings.Contains(stderr.String(), line) {
			t.Errorf("stderr = %q, want the line %q", stderr.String(), line)
		}
	}
}

func TestAPlannerProposesItsDraftFromAFile(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	file := writeFile(t, "experiment:\n  name: n\n  metrics:\n    - {key: val_loss, direction: minimize}\nbudget_accelerator_hours: 6\nagents:\n  - agent_type: generalist\n  - {}\nmetric_reason: r\n")
	if code := run([]string{"platform-experiments", "propose", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr.String())
	}
	want := []recorded{{Method: "PUT", Path: "/platform-experiments/pe-env/proposal", Body: map[string]any{
		"experiment":               map[string]any{"name": "n", "metrics": []any{map[string]any{"key": "val_loss", "direction": "minimize"}}},
		"budget_accelerator_hours": float64(6), "agents": []any{map[string]any{"agent_type": "generalist"}, map[string]any{}}, "metric_reason": "r",
	}}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v\nwant %+v", *got, want)
	}
	if code := run([]string{"platform-experiments", "propose", file, "another"}, &stdout, &stderr); code != 2 || len(*got) != 1 {
		t.Errorf("two files: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}

	// A goal that cannot be planned is declined with the reason the person will read.
	if code := run([]string{"platform-experiments", "decline", "--goal-version", "2", "--reason", "nothing here can be measured"}, &stdout, &stderr); code != 0 {
		t.Fatalf("decline: exit %d, stderr %s", code, stderr.String())
	}
	if last := (*got)[1]; last.Method != "PUT" || last.Path != "/platform-experiments/pe-env/decline" || !reflect.DeepEqual(last.Body, map[string]any{"reason": "nothing here can be measured", "goal_version": float64(2)}) {
		t.Fatalf("decline sent %+v", last)
	}
	if code := run([]string{"platform-experiments", "decline", "--goal-version", "2", "--reason", " "}, &stdout, &stderr); code != 2 || len(*got) != 2 {
		t.Errorf("declining with no reason: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}
}
