package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestAgentsAddCreatesAgentsWithWhatTheyAreLaunchedWith(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"agents": []map[string]any{
			{"agent_id": "ag-1", "hosted": true, "launcher": "main"},
			{"agent_id": "ag-2", "hosted": true, "launcher": "main"},
		}}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "agents", "add", "pe-1", "--count", "2", "--hosted", "--flavor", "data-search",
		"--hyperparameters", `{"batch_size":64}`, "--model", "claude-sonnet-5", "--name", "sweep", "--role", "baseline", "--quota-tier", "burst_only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	want := map[string]any{
		"count": float64(2), "hosted": true, "flavor": "data-search", "model": "claude-sonnet-5", "name": "sweep",
		"role": "baseline", "quota_tier": "burst_only", "hyperparameters": map[string]any{"batch_size": float64(64)},
	}
	if request.Method != "POST" || request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, want) {
		t.Fatalf("request = %+v\nwant body %v", request, want)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "ag-1") || !strings.Contains(lines[1], "ag-2") {
		t.Fatalf("stdout = %q, want one agent per line", stdout.String())
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
	if request.Path != "/platform-experiments/pe-env/agents" || !reflect.DeepEqual(request.Body, map[string]any{"count": float64(1)}) {
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
	if code := run([]string{"platform-experiments", "agents", "add", "pe-1", "--agent", "jane"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	if request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, map[string]any{"agent_id": "jane"}) {
		t.Fatalf("request = %+v, want agent_id alone: quota_tier is omitted when --quota-tier is not passed", request)
	}

	*got = nil
	if code := run([]string{"platform-experiments", "agents", "add", "pe-1", "--agent", "bot-1", "--quota-tier", "guaranteed"}, &stdout, &stderr); code != 0 {
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
		{"pe-1", "--agent", "jane", "--count", "2"},
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
		return map[string]any{"agents": []map[string]any{}}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "remove", "pe-1", "ag-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "DELETE" || r.Path != "/platform-experiments/pe-1/agents/ag-1" || !strings.Contains(stdout.String(), "removed") {
		t.Fatalf("remove request = %+v, stdout %q", r, stdout.String())
	}
	for _, args := range [][]string{{"remove"}, {"remove", "pe-1"}, {"remove", "pe-1", "ag-1", "ag-2"}} {
		if code := run(append([]string{"platform-experiments", "agents"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	*got = nil
	stderr.Reset()
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

func TestSignupPointsAtTheOneWayToJoin(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"signup", "--platform-experiment", "pe-1", "--agent", "jane"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "platform-experiments agents add") || len(*got) != 0 {
		t.Fatalf("stderr = %q, requests = %+v", stderr.String(), *got)
	}
}
