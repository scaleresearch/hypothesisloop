package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A submit the platform stored after hl stopped waiting was sent again and made a second job.
// hl names a submit by all it sends, so the same one sent twice carries one key, and a submit
// that differs in any part, or says --repeat, carries another.
func TestASubmitCarriesAKeyThatOnlyTheSameSubmitRepeats(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"id": "job-1", "status": "QUEUED"} })
	file := clitest.WriteJobFile(t)
	keyOf := func(args ...string) string {
		t.Helper()
		*got = nil
		var stdout, stderr bytes.Buffer
		if code := clitest.Run(JobSubmit, append([]string{"--agent", "jane", file}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("hl job submit %v: exit %d, stderr %s", args, code, stderr.String())
		}
		key, _ := (*got)[0].Body["submission_key"].(string)
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
			t.Fatalf("hl job submit %v: submission_key %q, want 64 hex digits", args, key)
		}
		return key
	}
	first := keyOf()
	if again := keyOf(); again != first {
		t.Errorf("the same submit sent twice carries %s and %s, want one key", first, again)
	}
	if keyOf("--repeat", "1") != first {
		t.Error("--repeat 1 is the first job of the request, want its key")
	}
	seen := map[string]string{first: "the plain submit"}
	for _, args := range [][]string{{"--repeat", "2"}, {"--repeat", "3"}, {"--env", "SEED=1"}, {"--hypothesis", "hyp-2"}, {"--theory", "t"}, {"--estimated-duration-hours", "0.5"}, {"--capacity-tier", "burst"}} {
		key := keyOf(args...)
		if other, taken := seen[key]; taken {
			t.Errorf("%v carries the key of %s, want a key of its own", args, other)
		}
		seen[key] = strings.Join(args, " ")
	}
	t.Setenv("HF_TOKEN", "one")
	one := keyOf("--secret-env", "HF_TOKEN")
	t.Setenv("HF_TOKEN", "two")
	if keyOf("--secret-env", "HF_TOKEN") == one {
		t.Error("a submit with another secret value carries the same key, want another")
	}

	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobSubmit, []string{"--agent", "jane", file, "--repeat", "0"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--repeat wants 1 or more") {
		t.Errorf("--repeat 0: exit %d, stderr %q, want 2 and why", code, stderr.String())
	}
}

// A job a submit created is QUEUED in its answer. Any other status is the job an identical submit
// created before: hl says nothing was submitted, and fails when that job has ended, since no
// result will arrive for the agent to wait on.
func TestASubmitAnsweredWithAnEarlierJobSaysNothingWasSubmitted(t *testing.T) {
	status := ""
	clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"id": "job-1", "status": status} })
	file := clitest.WriteJobFile(t)
	for _, tc := range []struct {
		status string
		args   []string
		code   int
		want   string
	}{
		{"RUNNING", nil, 0, "hl: job submit: an identical submit already created job-1, which is RUNNING, and nothing more was submitted. --repeat 2 submits the same request as another job\n"},
		{"EVICTED", nil, 1, "hl: job submit: nothing was submitted: an identical submit already created job-1, which ended EVICTED. --repeat 2 submits the same request as another job\n"},
		{"COMPLETED", []string{"--repeat", "2"}, 1, "hl: job submit: nothing was submitted: an identical submit already created job-1, which ended COMPLETED. --repeat 3 submits the same request as another job\n"},
	} {
		status = tc.status
		var stdout, stderr bytes.Buffer
		code := clitest.Run(JobSubmit, append([]string{"--agent", "jane", file}, tc.args...), &stdout, &stderr)
		if code != tc.code || stderr.String() != tc.want || stdout.String() != `{"id":"job-1","status":"`+tc.status+`"}`+"\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %d and %q", tc.status, code, stdout.String(), stderr.String(), tc.code, tc.want)
		}
	}
}

func TestJobSubmitFromYAML(t *testing.T) {
	var gotBody map[string]any
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","agent_id":"jane","code_ref":"repo@abc","job":{"image":"foo:latest"},` +
			`"estimated_cost_acch":0.5,"capacity_tier":"guaranteed","status":"QUEUED","not_admitted_reason":"capacity_unavailable"}`))
	})
	clitest.SetAPIURL(t, url)

	dir := t.TempDir()
	t.Chdir(dir)
	jobFile := dir + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\njob:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := clitest.Run(JobSubmit, []string{"--agent", "jane", "--platform-experiment", "pe-env", jobFile}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing on a submit that went through", stderr.String())
	}
	if want := `{"id":"job-1","status":"QUEUED","code_ref":"repo@abc","capacity_tier":"guaranteed","estimated_cost_acch":0.5}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	metadata, ok := gotBody["metadata"].(map[string]any)
	if !ok || metadata["agent_id"] != "jane" || metadata["platform_experiment_id"] != "pe-env" {
		t.Fatalf("agent_id and platform_experiment_id not stamped onto metadata: %+v", gotBody)
	}
	// The platform issues the id, and hl prints the one in the answer.
	if id, sent := gotBody["id"]; sent {
		t.Fatalf("hl sent an id of its own: %q", id)
	}
}

// The refusal is the platform's whole word on a job sent before the start: hl asks nothing more
// and prints it as any other.
func TestJobSubmitBeforeTheStartPrintsTheRefusalAndAsksNothingMore(t *testing.T) {
	requests := 0
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"reason":"experiment_not_started","message":"platform experiment pe-1 has not started"}`, http.StatusUnprocessableEntity)
	})
	clitest.SetAPIURL(t, url)
	jobFile := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(jobFile, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\njob:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := clitest.Run(JobSubmit, []string{"--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 1 || requests != 1 || stdout.Len() != 0 || strings.Count(stderr.String(), "\n") != 1 ||
		!strings.Contains(stderr.String(), `"reason":"experiment_not_started"`) || !strings.Contains(stderr.String(), "pe-1 has not started") {
		t.Fatalf("exit %d after %d requests, stdout %q, stderr %q", code, requests, stdout.String(), stderr.String())
	}
}

func TestJobSubmitBadYAML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	jobFile := dir + "/bad.yaml"
	if err := os.WriteFile(jobFile, []byte("not: valid: yaml: at: all: -"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := clitest.Run(JobSubmit, []string{"--agent", "jane", jobFile}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, stderr = %s", code, stderr.String())
	}
}
