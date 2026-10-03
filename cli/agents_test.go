package main

import (
	"bytes"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

// The help of get listed five of the reasons by hand while the platform answered a dozen more.
// It names every reason of the platform's one table and each value of waits_for.
func TestTheHelpOfGetNamesEveryStatusReasonThePlatformAnswers(t *testing.T) {
	var help bytes.Buffer
	run([]string{"platform-experiments", "get", "--help"}, &help, &help)
	words := regexp.MustCompile(`[a-z_]+`).FindAllString(help.String(), -1)
	for _, reason := range append(domain.StatusReasons(), "waits_for", string(domain.WaitsForOwner), string(domain.WaitsForCoordinator), string(domain.WaitsForPlatform)) {
		if !slices.Contains(words, reason) {
			t.Errorf("hl platform-experiments get --help does not name %s", reason)
		}
	}
	// A reason the table dropped is one the help must drop too.
	_, listed, _ := strings.Cut(help.String(), "The reasons:")
	listed, _, _ = strings.Cut(listed, "On an open one")
	listed = strings.NewReplacer("a draft:", "", "open, its baseline not declared:", "", "open, the declaration of the measured job refused:", "",
		"open, its baseline declared:", "", "running:", "", "while no researcher's machine is up yet, then", "",
		"An agent of the roster adds", "", "(hl platform-experiments agents list).", "").Replace(listed)
	for _, reason := range strings.FieldsFunc(listed, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if !slices.Contains(domain.StatusReasons(), reason) {
			t.Errorf("hl platform-experiments get --help names %q, which is no status reason of the platform", reason)
		}
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

// Before the start a researcher's instance reads waiting, and the roster's help listed
// requested for it.
func TestTheRostersHelpNamesEveryInstanceState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"platform-experiments", "agents", "list", "--help"}, &stdout, &stderr)
	for _, state := range []string{"waiting", "requested", "starting", "running", "stopped"} {
		if !strings.Contains(stderr.String(), state) {
			t.Errorf("the help of the roster does not name instance.state %s", state)
		}
	}
}
