package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestAgentsAddCreatesOneAgentWithWhatItIsLaunchedWith(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "hosted": true, "launcher": "main"}}}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "agents", "add", "--platform-experiment", "pe-1", "--hosted", "--flavor", "data-search",
		"--hyperparameters", `{"batch_size":64}`, "--model", "claude-sonnet-5", "--name", "sweep", "--role", "baseline", "--quota-tier", "burst_only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	want := map[string]any{
		"hosted": true, "flavor": "data-search", "model": "claude-sonnet-5", "name": "sweep",
		"role": "baseline", "quota_tier": "burst_only", "hyperparameters": map[string]any{"batch_size": float64(64)},
	}
	if request.Method != "POST" || request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, want) {
		t.Fatalf("request = %+v\nwant body %v", request, want)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(*got) != 1 || len(lines) != 1 || !strings.Contains(lines[0], "ag-1") {
		t.Fatalf("stdout = %q after %d requests, want the one agent of one request", stdout.String(), len(*got))
	}
}

func TestAgentsAddSendsOnlyWhatWasAsked(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "token": "hls_x"}}}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "add"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	if request.Path != "/platform-experiments/pe-env/agents" || !reflect.DeepEqual(request.Body, map[string]any{}) {
		t.Fatalf("request = %+v", request)
	}
	if !strings.Contains(stdout.String(), "hls_x") {
		t.Fatalf("stdout = %q, want the self-run agent's token", stdout.String())
	}
}

// What hl signup did: a registered agent joins by id, with an optional quota tier.
func TestAgentsAddJoinsARegisteredAgent(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "jane"}}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "add", "--platform-experiment", "pe-1", "--agent", "jane"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	if request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, map[string]any{"agent_id": "jane"}) {
		t.Fatalf("request = %+v, want agent_id alone: quota_tier is omitted when --quota-tier is not passed", request)
	}

	*got = nil
	if code := run([]string{"platform-experiments", "agents", "add", "--platform-experiment", "pe-1", "--agent", "bot-1", "--quota-tier", "guaranteed"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if body := (*got)[0].Body; body["quota_tier"] != "guaranteed" || body["agent_id"] != "bot-1" {
		t.Fatalf("body = %+v", body)
	}
}

func TestAgentsAddRefusesWhatTheServerWouldBeforeSending(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	t.Setenv("PLATFORM_EXPERIMENT_ID", "")
	for _, args := range [][]string{
		{"pe-1", "--quota-tier", "vip"},
		{"pe-1", "--hyperparameters", "not json"},
		{"pe-1", "--hyperparameters", "[1,2]"},
		{"pe-1", "--hyperparameters", "null"},
		{"pe-1", "--count", "2"},
		{"pe-1", "pe-2"},
		{},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"platform-experiments", "agents", "add"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("requests sent for refused commands: %+v", *got)
	}
}

func TestAgentsRemoveAndList(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		if r.Method == "DELETE" {
			return map[string]any{"status": "removed"}
		}
		if r.Method == "POST" {
			return map[string]any{"status": "finished"}
		}
		return map[string]any{"agents": []map[string]any{}}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "remove", "--platform-experiment", "pe-1", "ag-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "DELETE" || r.Path != "/platform-experiments/pe-1/agents/ag-1" || !strings.Contains(stdout.String(), "removed") {
		t.Fatalf("remove request = %+v, stdout %q", r, stdout.String())
	}
	for _, args := range [][]string{{"remove"}, {"remove", "pe-1", "ag-1"}, {"list", "pe-1"}, {"add", "pe-1"}} {
		if code := run(append([]string{"platform-experiments", "agents"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	*got = nil
	stderr.Reset()
	t.Setenv("AGENT_ID", "ag-1")
	*got = nil
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "quota spent"}, &stdout, &stderr); code != 0 {
		t.Fatalf("finish: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "POST" || r.Path != "/platform-experiments/pe-env/agents/ag-1/finish" || !reflect.DeepEqual(r.Body, map[string]any{"reason": "quota spent"}) {
		t.Fatalf("finish request = %+v", r)
	}
	if code := run([]string{"platform-experiments", "agents", "finish"}, &stdout, &stderr); code != 2 || len(*got) != 1 {
		t.Errorf("finish without a reason: exit %d with %d requests, want 2 and no further one", code, len(*got))
	}
	*got = nil
	if code := run([]string{"platform-experiments", "agents", "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "GET" || r.Path != "/platform-experiments/pe-env/agents" {
		t.Fatalf("list request = %+v", r)
	}
	if !strings.Contains(stderr.String(), "no agents have joined pe-env") {
		t.Fatalf("stderr = %q, want the empty roster said out loud", stderr.String())
	}
}

// A hosted coordinator's briefing is built from this listing. It crashed on a subcommand that
// read the token before parsing its flags, so every coordinator exited as it started.
func TestTheAgentsGroupListsItsCommands(t *testing.T) {
	code, stdout, stderr := hl(t, "platform-experiments", "agents", "--help")
	if code != 0 || !strings.Contains(stdout, "\n  add [--platform-experiment PE]") || !strings.Contains(stdout, "\n  list [--platform-experiment PE]") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}
