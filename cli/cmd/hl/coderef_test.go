package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestJobSubmitFlagsOverrideTheCommittedSpecWithoutACommit(t *testing.T) {
	clone, origin := clitest.PushedClone(t)
	clitest.MustGit(t, clone, "checkout", "-q", "-b", "hl/pe-flag/jane/idea-abc123")
	var sent map[string]any
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	var stdout, stderr bytes.Buffer
	args := []string{"job", "submit", "job.yaml", "--agent", "jane", "--hypothesis", "hyp-9", "--theory", "wider wins",
		"--env", "LR=0.1", "--env", "NOTE=a=b", "--platform-experiment", "pe-flag",
		"--estimated-duration-hours", "0.05", "--capacity-tier", "burst"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	metadata := sent["metadata"].(map[string]any)
	if metadata["hypothesis_id"] != "hyp-9" || metadata["theory"] != "wider wins" || metadata["platform_experiment_id"] != "pe-flag" {
		t.Fatalf("metadata = %+v", metadata)
	}
	// A researcher refused for quota wrote five copies of the spec, each committed and pushed,
	// to change these two fields.
	if metadata["estimated_duration_hours"] != 0.05 || metadata["capacity_tier"] != "burst" {
		t.Fatalf("metadata = %+v, want the estimate and the tier of the flags", metadata)
	}
	if want := origin + "@" + clitest.MustGit(t, clone, "rev-parse", "HEAD"); metadata["code_ref"] != want {
		t.Fatalf("code_ref = %v, want %s", metadata["code_ref"], want)
	}
	env := sent["job"].(map[string]any)["env"].(map[string]any)
	if env["LR"] != "0.1" || env["NOTE"] != "a=b" {
		t.Fatalf("env = %+v", env)
	}
	if dirty := clitest.MustGit(t, clone, "status", "--porcelain"); dirty != "" {
		t.Fatalf("submit changed the clone: %s", dirty)
	}
}
