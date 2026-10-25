package agent

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A planner copied cpu: 8 from the help's example and was refused 422 "expected string": the
// example, proposed as it stands, sends what the API reads.
func TestTheProposeExampleIsSentAsTheAPIReadsIt(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	var help bytes.Buffer
	PlatformExperimentsPropose(nil, []string{"-h"}, &help, &help)
	_, example, _ := strings.Cut(help.String(), "\n\n  goal_version:")
	example, _, _ = strings.Cut("  goal_version:"+example, "\n\nThe proposal is checked")
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(PlatformExperimentsPropose, []string{clitest.WriteFile(t, example)}, &stdout, &stderr); code != 0 {
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
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	file := clitest.WriteFile(t, "out_of_scope: |\n  what was left out\nrisks:\n  - |\n    a risk\n")
	if code := clitest.Run(PlatformExperimentsPropose, []string{file}, &stdout, &stderr); code != 0 {
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
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	file := clitest.WriteFile(t, "experiment:\n  name: n\nbudget_accelerator_hours: 6\ncpu: 2\n")
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(PlatformExperimentsPropose, []string{"--check", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose --check: exit %d, stderr %s", code, stderr.String())
	}
	want := []clitest.Recorded{{Method: "PUT", Path: "/platform-experiments/pe-env/proposal", Query: "check=true", Body: map[string]any{
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
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(refusal))
	}))
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	stdout.Reset()
	if code := clitest.Run(PlatformExperimentsPropose, []string{"--check", file}, &stdout, &stderr); code != 1 {
		t.Fatalf("a refused check: exit %d, want 1. stderr %s", code, stderr.String())
	}
	for _, line := range []string{"2 problems, fix them all:", "  invalid_cost: budget_accelerator_hours is required", "  accelerator_unreachable: accelerator_type \"cpu\""} {
		if !strings.Contains(stderr.String(), line) {
			t.Errorf("stderr = %q, want the line %q", stderr.String(), line)
		}
	}
}

// The planner's first plan is stored as the draft's working plan, for its baseline agent to start
// from, and opens nothing.
func TestAPlannerStoresAWorkingPlan(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	file := clitest.WriteFile(t, "experiment:\n  name: n\nbudget_accelerator_hours: 6\n")
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(PlatformExperimentsPropose, []string{"--working", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose --working: exit %d, stderr %s", code, stderr.String())
	}
	if len(*got) != 1 || (*got)[0].Query != "working=true" || !strings.Contains(stdout.String(), "stored as the working plan") {
		t.Fatalf("requests = %+v, stdout %q, want one PUT with working=true and the line that it is stored", *got, stdout.String())
	}
	if code := clitest.Run(PlatformExperimentsPropose, []string{"--working", "--check", file}, &stdout, &stderr); code != 2 {
		t.Fatalf("--working with --check: exit %d, want 2", code)
	}
}

func TestAPlannerProposesItsDraftFromAFile(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"status": "draft"} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	file := clitest.WriteFile(t, "experiment:\n  name: n\n  metrics:\n    - {key: val_loss, direction: minimize}\nbudget_accelerator_hours: 6\nagents:\n  - agent_type: generalist\n  - {}\nmetric_reason: r\n")
	if code := clitest.Run(PlatformExperimentsPropose, []string{file}, &stdout, &stderr); code != 0 {
		t.Fatalf("propose: exit %d, stderr %s", code, stderr.String())
	}
	want := []clitest.Recorded{{Method: "PUT", Path: "/platform-experiments/pe-env/proposal", Body: map[string]any{
		"experiment":               map[string]any{"name": "n", "metrics": []any{map[string]any{"key": "val_loss", "direction": "minimize"}}},
		"budget_accelerator_hours": float64(6), "agents": []any{map[string]any{"agent_type": "generalist"}, map[string]any{}}, "metric_reason": "r",
	}}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("requests = %+v\nwant %+v", *got, want)
	}
	if code := clitest.Run(PlatformExperimentsPropose, []string{file, "another"}, &stdout, &stderr); code != 2 || len(*got) != 1 {
		t.Errorf("two files: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}

	// A goal that cannot be planned is declined with the reason the person will read.
	if code := clitest.Run(PlatformExperimentsDecline, []string{"--goal-version", "2", "--reason", "nothing here can be measured"}, &stdout, &stderr); code != 0 {
		t.Fatalf("decline: exit %d, stderr %s", code, stderr.String())
	}
	if last := (*got)[1]; last.Method != "PUT" || last.Path != "/platform-experiments/pe-env/decline" || !reflect.DeepEqual(last.Body, map[string]any{"reason": "nothing here can be measured", "goal_version": float64(2)}) {
		t.Fatalf("decline sent %+v", last)
	}
	if code := clitest.Run(PlatformExperimentsDecline, []string{"--goal-version", "2", "--reason", " "}, &stdout, &stderr); code != 2 || len(*got) != 2 {
		t.Errorf("declining with no reason: exit %d with %d requests, want 2 and nothing sent", code, len(*got))
	}
}
