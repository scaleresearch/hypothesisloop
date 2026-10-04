package main

import (
	"bytes"
	"net/http"
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
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "add", "--platform-experiment", "pe-env"}, &stdout, &stderr); code != 0 {
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
			return nil
		}
		if r.Method == "POST" {
			return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "finished_reason": "quota spent"}}}
		}
		return map[string]any{"agents": []map[string]any{}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "remove", "--platform-experiment", "pe-1", "ag-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remove: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "DELETE" || r.Path != "/platform-experiments/pe-1/agents/ag-1" || !strings.Contains(stderr.String(), "removed ag-1 from platform experiment pe-1") {
		t.Fatalf("remove request = %+v, stderr %q", r, stderr.String())
	}
	for _, args := range [][]string{{"remove"}, {"remove", "pe-1", "ag-1"}, {"list", "pe-1"}, {"add", "pe-1"}} {
		if code := run(append([]string{"platform-experiments", "agents"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	// An agent finishes itself: its token says who and where, and the platform answers 403 to
	// anyone else, so hl sends nothing for them. The answer is agents, a list of that one agent,
	// printed as the line add and list print, where finish once printed the object around it.
	stderr.Reset()
	stdout.Reset()
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	*got = nil
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "quota spent"}, &stdout, &stderr); code != 0 {
		t.Fatalf("finish: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "POST" || r.Path != "/platform-experiments/pe-env/agents/ag-1/finish" || !reflect.DeepEqual(r.Body, map[string]any{"reason": "quota spent"}) {
		t.Fatalf("finish request = %+v", r)
	}
	if want := `{"agent_id":"ag-1","finished_reason":"quota spent"}` + "\n"; stdout.String() != want {
		t.Errorf("finish printed %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "quota spent", "--blocker", "the build has no network"}, &stdout, &stderr); code != 0 ||
		!reflect.DeepEqual((*got)[1].Body, map[string]any{"reason": "quota spent", "blocker": "the build has no network"}) {
		t.Fatalf("finish with a blocker: exit %d, request %+v, want the blocker sent", code, (*got)[1])
	}
	stdout.Reset()
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "baseline measured by job-2", "--baseline", "job-2"}, &stdout, &stderr); code != 0 ||
		!reflect.DeepEqual((*got)[2].Body, map[string]any{"reason": "baseline measured by job-2", "baseline_job_id": "job-2"}) {
		t.Fatalf("finish naming the baseline: exit %d, request %+v, want the job sent as baseline_job_id", code, (*got)[2])
	}
	*got = (*got)[:1]
	if code := run([]string{"platform-experiments", "agents", "finish"}, &stdout, &stderr); code != 2 || len(*got) != 1 {
		t.Errorf("finish without a reason: exit %d with %d requests, want 2 and no further one", code, len(*got))
	}
	t.Setenv("HL_TOKEN", "hlk_x")
	stderr.Reset()
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "quota spent"}, &stdout, &stderr); code != 1 || len(*got) != 1 || !strings.Contains(stderr.String(), "an agent does this itself") {
		t.Errorf("finish by a person: exit %d with %d requests, stderr %q, want 1, no further request and why", code, len(*got), stderr.String())
	}
	if code := run([]string{"platform-experiments", "agents", "finish", "--reason", "quota spent", "--agent", "ag-1"}, &stdout, &stderr); code != 1 || len(*got) != 1 {
		t.Errorf("finish by a person naming an agent: exit %d with %d requests, want 1 and no further one", code, len(*got))
	}
	t.Setenv("HL_TOKEN", "test-token")
	*got = nil
	stderr.Reset()
	if code := run([]string{"platform-experiments", "agents", "list", "--platform-experiment", "pe-env"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "GET" || r.Path != "/platform-experiments/pe-env/agents" {
		t.Fatalf("list request = %+v", r)
	}
	if !strings.Contains(stderr.String(), "no agents have joined pe-env") {
		t.Fatalf("stderr = %q, want the empty roster said out loud", stderr.String())
	}
}

// An agent that finished after the run closed got a bare 409: the close already ended its run, so
// hl says nothing needs finishing and succeeds.
func TestFinishingAfterTheCloseSaysNothingNeedsFinishing(t *testing.T) {
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"experiment_closed","message":"the platform experiment is closed"}`))
	}))
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"platform-experiments", "agents", "finish", "--reason", "done"}, &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "the platform experiment is closed, which ended your run: nothing needs finishing") {
		t.Errorf("exit %d, stdout %q, stderr %q, want 0 and the close said plainly", code, stdout.String(), stderr.String())
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

// The coordinator and the owner message one agent or every researcher; hl refuses an agent's
// token and a message that names neither before anything is sent.
func TestAgentsMessageSendsToOneAgentOrEveryResearcher(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		return map[string]any{"messages": []map[string]any{{"agent_id": "ag-1", "from": "coordinator", "text": "try warmup"}}}
	})
	var stdout, stderr bytes.Buffer
	t.Setenv("HL_TOKEN", derivedToken("coordinator:pe-env"))
	if code := run([]string{"platform-experiments", "agents", "message", "ag-1", "--text", "try warmup"}, &stdout, &stderr); code != 0 {
		t.Fatalf("message: exit %d, stderr %s", code, stderr.String())
	}
	if r := (*got)[0]; r.Method != "POST" || r.Path != "/platform-experiments/pe-env/messages" || !reflect.DeepEqual(r.Body, map[string]any{"agent_id": "ag-1", "text": "try warmup"}) {
		t.Fatalf("message request = %+v", r)
	}
	if want := `{"agent_id":"ag-1","from":"coordinator","text":"try warmup"}` + "\n"; stdout.String() != want {
		t.Errorf("message printed %q, want %q", stdout.String(), want)
	}
	if code := run([]string{"platform-experiments", "agents", "message", "--all", "--text", "queue more"}, &stdout, &stderr); code != 0 ||
		!reflect.DeepEqual((*got)[1].Body, map[string]any{"all": true, "text": "queue more"}) {
		t.Fatalf("message --all: exit %d, request %+v", code, (*got)[1])
	}
	if code := run([]string{"platform-experiments", "agents", "messages", "--agent", "ag-1"}, &stdout, &stderr); code != 0 ||
		(*got)[2].Method != "GET" || (*got)[2].Path != "/platform-experiments/pe-env/messages" || (*got)[2].Query != "agent_id=ag-1" {
		t.Fatalf("messages: exit %d, request %+v", code, (*got)[2])
	}
	for _, args := range [][]string{{"message", "--text", "x"}, {"message", "ag-1", "--all", "--text", "x"}, {"message", "ag-1"}} {
		if code := run(append([]string{"platform-experiments", "agents"}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	t.Setenv("HL_TOKEN", derivedToken("agent:pe-env/ag-1"))
	if code := run([]string{"platform-experiments", "agents", "message", "ag-2", "--text", "x"}, &stdout, &stderr); code != 1 || len(*got) != 3 {
		t.Errorf("message by an agent: exit %d with %d requests, want 1 and no further request", code, len(*got))
	}
}
