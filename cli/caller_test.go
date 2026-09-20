package main

import (
	"bytes"
	"encoding/base64"
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

// A coordinator is no agent. Every command that acts as one says so before any request, and
// $AGENT_ID is not read for it: a variable left in its environment must not decide who writes.
func TestACoordinatorIsRefusedEveryCommandThatActsAsAnAgentUnlessItNamesOne(t *testing.T) {
	job := writeFile(t, "job: {image: i}\nmetadata: {hypothesis_id: hyp-1}\n")
	commands := map[string][]string{
		"hypothesis submit":  {"hypothesis", "submit", "--text", "t", "--builds-on", "root"},
		"hypothesis link":    {"hypothesis", "link", "hyp-1", "--type", "related", "--to", "hyp-2"},
		"hypothesis comment": {"hypothesis", "comment", "hyp-1", "--text", "t"},
		"hypothesis status":  {"hypothesis", "status", "hyp-1", "confirmed"},
		"job submit":         {"job", "submit", job},
		"donation request":   {"donation", "request", "--credits", "1", "--reason", "r"},
		"donation fulfill":   {"donation", "fulfill", "don-1"},
		"data-key":           {"data-key"},
	}
	for name, args := range commands {
		got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
		t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
		t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
		t.Setenv("AGENT_ID", "left-over")
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 || len(*got) != 0 {
			t.Errorf("%s: exit %d with %d requests, want 1 and none. stderr: %s", name, code, len(*got), stderr.String())
		}
		if !strings.Contains(stderr.String(), "coordinator's, which is no agent") || !strings.Contains(stderr.String(), "hl platform-experiments summary") {
			t.Errorf("%s: stderr = %q, want it to say why and what a coordinator writes", name, stderr.String())
		}
	}

	// Naming the agent is the coordinator acting for it, which the platform decides on.
	got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
	t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "comment", "hyp-1", "--text", "t", "--agent", "ag-7"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if body := (*got)[0].Body; !reflect.DeepEqual(body, map[string]any{"agent_id": "ag-7", "text": "t"}) {
		t.Fatalf("body = %v", body)
	}
}

func TestAnAgentAndAPersonStillWriteAsTheAgentInTheEnvironment(t *testing.T) {
	for _, token := range []string{derivedToken("agent:pe-1/ag-1"), "hlk_an-api-key"} {
		got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
		t.Setenv("HL_TOKEN", token)
		t.Setenv("AGENT_ID", "ag-1")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"hypothesis", "comment", "hyp-1", "--text", "t"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d, stderr %s", code, stderr.String())
		}
		if body := (*got)[0].Body; body["agent_id"] != "ag-1" {
			t.Fatalf("body = %v", body)
		}
	}
}

func TestACoordinatorReadsTheWholeExperimentWhereAnAgentReadsItsOwn(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return []any{} })
	t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-1"))
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-1")
	t.Setenv("AGENT_ID", "")
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
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	t.Setenv("AGENT_ID", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "summary", "--text", "what was found"}, &stdout, &stderr); code != 0 {
		t.Fatalf("summary: exit %d, stderr %s", code, stderr.String())
	}
	file := writeFile(t, "from a file\n")
	if code := run([]string{"platform-experiments", "summary", "pe-2", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("summary from a file: exit %d, stderr %s", code, stderr.String())
	}
	if code := run([]string{"platform-experiments", "close"}, &stdout, &stderr); code != 0 {
		t.Fatalf("close: exit %d, stderr %s", code, stderr.String())
	}
	want := []recorded{
		{Method: "POST", Path: "/platform-experiments/pe-env/summary", Body: map[string]any{"summary": "what was found"}},
		{Method: "POST", Path: "/platform-experiments/pe-2/summary", Body: map[string]any{"summary": "from a file\n"}},
		{Method: "POST", Path: "/platform-experiments/pe-env/close", Body: map[string]any{}},
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
	if len(*got) != 3 {
		t.Errorf("a refused summary was sent: %+v", *got)
	}
}
