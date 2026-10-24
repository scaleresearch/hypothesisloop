package shared

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestAgentsAddCreatesOneAgentWithWhatItIsLaunchedWith(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "in_agentsandbox": true, "launcher": "main"}}}
	})
	var stdout, stderr bytes.Buffer
	code := clitest.Run(AgentsAdd, []string{"--platform-experiment", "pe-1", "--in-agentsandbox", "--agent-type", "data-search", "--hyperparameters", `{"batch_size":64}`, "--model", "anthropic/claude-sonnet-5", "--name", "sweep", "--role", "baseline", "--quota-tier", "burst_only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	want := map[string]any{
		"in_agentsandbox": true, "agent_type": "data-search", "model": "anthropic/claude-sonnet-5", "name": "sweep",
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
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "token": "hls_x"}}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(AgentsAdd, []string{"--platform-experiment", "pe-env"}, &stdout, &stderr); code != 0 {
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
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "jane"}}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(AgentsAdd, []string{"--platform-experiment", "pe-1", "--agent", "jane"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	request := (*got)[0]
	if request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, map[string]any{"agent_id": "jane"}) {
		t.Fatalf("request = %+v, want agent_id alone: quota_tier is omitted when --quota-tier is not passed", request)
	}

	*got = nil
	if code := clitest.Run(AgentsAdd, []string{"--platform-experiment", "pe-1", "--agent", "bot-1", "--quota-tier", "guaranteed"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if body := (*got)[0].Body; body["quota_tier"] != "guaranteed" || body["agent_id"] != "bot-1" {
		t.Fatalf("body = %+v", body)
	}
}

func TestAgentsAddRefusesWhatTheServerWouldBeforeSending(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return nil })
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
		if code := clitest.Run(AgentsAdd, append([]string{}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("requests sent for refused commands: %+v", *got)
	}
}

// A message to every agent that some were rate limited for prints the messages sent, then
// each agent refused and why, and succeeds.
func TestAgentsMessageAllNamesTheAgentsItDidNotReach(t *testing.T) {
	clitest.RecordingServer(t, func(_ int, _ clitest.Recorded) any {
		return map[string]any{
			"messages": []map[string]any{{"agent_id": "ag-1", "from": "owner", "text": "queue more"}},
			"refused":  []map[string]any{{"agent_id": "ag-2", "reason": "rate_limited", "retry_after": "2026-10-09T12:00:00Z"}},
		}
	})
	var stdout, stderr bytes.Buffer
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-env"))
	if code := clitest.Run(AgentsMessage, []string{"--all", "--text", "queue more"}, &stdout, &stderr); code != 0 {
		t.Fatalf("message --all: exit %d, stderr %s", code, stderr.String())
	}
	want := `{"agent_id":"ag-1","from":"owner","text":"queue more"}` + "\n" +
		`{"agent_id":"ag-2","reason":"rate_limited","retry_after":"2026-10-09T12:00:00Z"}` + "\n"
	if stdout.String() != want {
		t.Errorf("printed %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "1 recipients were not sent to") {
		t.Errorf("stderr %q, want the note on the refused", stderr.String())
	}
}

// A researcher is added with its role alone: the server runs it as the built-in researcher type
// on the coordinator's model.
func TestAgentsAddResearcherSendsTheRoleAlone(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-r", "role": "researcher"}}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(AgentsAdd, []string{"--platform-experiment", "pe-1", "--in-agentsandbox", "--role", "researcher"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := map[string]any{"in_agentsandbox": true, "role": "researcher"}
	if request := (*got)[0]; request.Method != "POST" || request.Path != "/platform-experiments/pe-1/agents" || !reflect.DeepEqual(request.Body, want) {
		t.Fatalf("request = %+v", request)
	}
}
