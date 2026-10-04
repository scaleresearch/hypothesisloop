package main

import (
	"bytes"
	"encoding/base64"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"net/http"
	"reflect"
	"strings"
	"testing"
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
		auth.AgentScope("pe-1", "ag-1"): callerAgent,
		auth.CoordinatorScope("pe-1"):   callerCoordinator,
		auth.LauncherScope("main"):      callerOther,
		"staff":                         callerOther,
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

// A coordinator is no agent and acts for none. Every command that acts as one says so before any
// request, whether --agent names one or a variable left in its environment does. What it writes
// to the hypothesis graph it writes as itself, so there only naming an agent is refused.
func TestACoordinatorIsRefusedEveryCommandThatActsAsAnAgent(t *testing.T) {
	job := writeFile(t, "job: {image: i}\nmetadata: {hypothesis_id: hyp-1}\n")
	graphWrites := map[string][]string{
		"hypothesis submit":  {"hypothesis", "submit", "--text", "t", "--builds-on", "root"},
		"hypothesis link":    {"hypothesis", "link", "hyp-1", "--type", "related", "--to", "hyp-2"},
		"hypothesis comment": {"hypothesis", "comment", "hyp-1", "--text", "t"},
		"hypothesis status":  {"hypothesis", "status", "hyp-1", "inconclusive"},
	}
	spending := map[string][]string{
		"job submit": {"job", "submit", job},
		"checkout":   {"hypothesis", "checkout", "hyp-1"},
		"data-key":   {"data-key"},
		"finish":     {"platform-experiments", "agents", "finish", "--reason", "quota spent"},
	}
	for name, args := range graphWrites {
		got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "hyp-9"} })
		t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || len(*got) != 1 {
			t.Fatalf("%s as itself: exit %d with %d requests, want 0 and one. stderr: %s", name, code, len(*got), stderr.String())
		}
		if _, named := (*got)[0].Body["agent_id"]; named {
			t.Errorf("%s as itself: body = %v, want no agent named", name, (*got)[0].Body)
		}
	}
	commands := map[string][]string{}
	for name, args := range spending {
		commands[name] = args
	}
	for name, args := range commands {
		for _, named := range [][]string{nil, {"--agent", "ag-7"}} {
			got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
			t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
			var stdout, stderr bytes.Buffer
			if code := run(append(append([]string{}, args...), named...), &stdout, &stderr); code != 1 || len(*got) != 0 {
				t.Errorf("%s %v: exit %d with %d requests, want 1 and none. stderr: %s", name, named, code, len(*got), stderr.String())
			}
			if !strings.Contains(stderr.String(), "is no agent and acts for none") || !strings.Contains(stderr.String(), "hl platform-experiments summary") {
				t.Errorf("%s %v: stderr = %q, want it to say why and what a coordinator writes", name, named, stderr.String())
			}
		}
	}
	for name, args := range graphWrites {
		got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
		t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
		var stdout, stderr bytes.Buffer
		if code := run(append(append([]string{}, args...), "--agent", "ag-7"), &stdout, &stderr); code != 1 || len(*got) != 0 {
			t.Errorf("%s for an agent: exit %d with %d requests, want 1 and none. stderr: %s", name, code, len(*got), stderr.String())
		}
		if !strings.Contains(stderr.String(), "is no agent and acts for none") || !strings.Contains(stderr.String(), "without --agent") {
			t.Errorf("%s for an agent: stderr = %q, want it to say a coordinator writes as itself", name, stderr.String())
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
	if code := run([]string{"platform-experiments", "update"}, &stdout, &stderr); code != 2 {
		t.Errorf("an update that names no field: exit %d, want 2", code)
	}
	want := []recorded{
		{Method: "PUT", Path: "/platform-experiments/pe-1/summary", Body: map[string]any{"summary": "what was found"}},
		{Method: "PUT", Path: "/platform-experiments/pe-2/summary", Body: map[string]any{"summary": "from a file\n"}},
		{Method: "POST", Path: "/platform-experiments/pe-1/close", Body: map[string]any{}},
		{Method: "PATCH", Path: "/platform-experiments/pe-1", Body: map[string]any{"description": "from a file\n"}},
		{Method: "PATCH", Path: "/platform-experiments/pe-2", Body: map[string]any{"name": "renamed", "max_agents": float64(4)}},
		{Method: "PATCH", Path: "/platform-experiments/pe-1", Body: map[string]any{"baseline_job_id": "job-1"}},
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
	if len(*got) != 6 {
		t.Errorf("a refused summary was sent: %+v", *got)
	}
}

// A hosted agent asked `hl cluster list` and was answered "403: a agent token may not call GET
// /me", which says neither what the command is for nor what to use.
func TestAnAgentTokenAskingForAnOrgsCommandIsToldWhatItCanUse(t *testing.T) {
	for _, scope := range []string{"agent:pe-1/ag-1", "coordinator:pe-1"} {
		got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
		t.Setenv("HL_TOKEN", derivedToken(scope))
		t.Setenv("HL_ORG", "")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"cluster", "list"}, &stdout, &stderr); code != 1 || len(*got) != 0 {
			t.Fatalf("%s: exit %d with %d requests, want 1 and none. stderr: %s", scope, code, len(*got), stderr.String())
		}
		if said := stderr.String(); !strings.Contains(said, "reaches no org") || !strings.Contains(said, "hl capacity") || strings.Count(said, "\n") != 1 {
			t.Errorf("%s: stderr = %q, want one line saying why and naming hl capacity", scope, said)
		}
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
	_, example, _ := strings.Cut(help.String(), "\n\n  experiment:")
	example, _, _ = strings.Cut("  experiment:"+example, "\n\nThe proposal is checked")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "propose", writeFile(t, example)}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose the help's example: exit %d, stderr %s", code, stderr.String())
	}
	body := (*got)[0].Body
	if body["cpu"] != "8" || body["memory"] != "32Gi" || body["storage"] != "100Gi" {
		t.Errorf("sent cpu %#v, memory %#v, storage %#v, want the strings \"8\", \"32Gi\" and \"100Gi\"", body["cpu"], body["memory"], body["storage"])
	}
}

func TestAPlannerProposesItsDraftFromAFile(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	file := writeFile(t, "experiment:\n  name: n\n  metrics:\n    - {key: val_loss, direction: minimize}\nbudget_accelerator_hours: 6\nagents:\n  - flavor: generalist\n  - {}\nmetric_reason: r\n")
	if code := run([]string{"platform-experiments", "propose", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr.String())
	}
	want := []recorded{{Method: "PUT", Path: "/platform-experiments/pe-env/proposal", Body: map[string]any{
		"experiment":               map[string]any{"name": "n", "metrics": []any{map[string]any{"key": "val_loss", "direction": "minimize"}}},
		"budget_accelerator_hours": float64(6), "agents": []any{map[string]any{"flavor": "generalist"}, map[string]any{}}, "metric_reason": "r",
	}}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v\nwant %+v", *got, want)
	}
	if code := run([]string{"platform-experiments", "propose", file, "another"}, &stdout, &stderr); code != 2 || len(*got) != 1 {
		t.Errorf("two files: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}

	// A goal that cannot be planned is declined with the reason the person will read.
	if code := run([]string{"platform-experiments", "decline", "--reason", "nothing here can be measured"}, &stdout, &stderr); code != 0 {
		t.Fatalf("decline: exit %d, stderr %s", code, stderr.String())
	}
	if last := (*got)[1]; last.Method != "PUT" || last.Path != "/platform-experiments/pe-env/decline" || !reflect.DeepEqual(last.Body, map[string]any{"reason": "nothing here can be measured"}) {
		t.Fatalf("decline sent %+v", last)
	}
	if code := run([]string{"platform-experiments", "decline", "--reason", " "}, &stdout, &stderr); code != 2 || len(*got) != 2 {
		t.Errorf("declining with no reason: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}
}
