package shared

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// One command reaches every session of an experiment: an agent's by its id, the coordinator's,
// which is no agent and has a path of its own, and the planner's, found in the roster.
func TestAgentsSessionWritesTheStoredBytesAsTheyAre(t *testing.T) {
	stored := []byte{0x1f, 0x8b, 0x00, 0xff, '\n', 0x00, 'x'}
	var asked string
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agents") {
			_, _ = w.Write([]byte(`{"agents":[{"agent_id":"ag-p","role":"planner"},{"agent_id":"ag-1","role":"competitor"}]}`))
			return
		}
		asked = r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(stored)
	}))
	for who, want := range map[string]string{
		"ag-1":        "GET /platform-experiments/pe-1/agents/ag-1/session",
		"coordinator": "GET /platform-experiments/pe-1/coordinator/session",
		"planner":     "GET /platform-experiments/pe-1/agents/ag-p/session",
	} {
		var stdout, stderr bytes.Buffer
		if code := clitest.Run(AgentsSession, []string{"--platform-experiment", "pe-1", who}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d, stderr %s", who, code, stderr.String())
		}
		if asked != want || !bytes.Equal(stdout.Bytes(), stored) {
			t.Fatalf("%s: asked %q and wrote %v, want %q and the stored bytes", who, asked, stdout.Bytes(), want)
		}
	}
}

func TestAgentsSessionOfAPlannerThatNeverWasSaysSo(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"agents": []map[string]any{{"agent_id": "ag-1", "role": "competitor"}}}
	})
	var stdout, stderr bytes.Buffer
	code := clitest.Run(AgentsSession, []string{"--platform-experiment", "pe-1", "planner"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "pe-1 has no planner") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestAgentsSessionSaysWhyItWasRefused(t *testing.T) {
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reason":"not_found","message":"session not found"}`))
	}))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(AgentsSession, []string{"--platform-experiment", "pe-1", "ag-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "session not found") {
		t.Fatalf("stdout %q, stderr %q: want nothing written and the platform's reason", stdout.String(), stderr.String())
	}
}
