package shared

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestACoordinatorReadsTheWholeExperimentWhereAnAgentReadsItsOwn(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return []any{} })
	t.Setenv("HL_TOKEN", clitest.DerivedToken("coordinator:pe-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Quota, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if path := (*got)[0].Path; path != "/platform-experiments/pe-1/quotas" {
		t.Fatalf("path = %q, want every agent's quota", path)
	}
	if code := clitest.Run(Quota, []string{"--agent", "ag-3"}, &stdout, &stderr); code != 0 || (*got)[1].Path != "/platform-experiments/pe-1/quotas/ag-3" {
		t.Fatalf("with --agent: exit %d, requests %+v", code, *got)
	}
}

// A request refused for its shape is answered by the API with every field it read. Seen by an
// agent that left one field out: a page of its own request around the one line that mattered.
func TestARequestRefusedForItsShapeIsOneLineNamingTheFields(t *testing.T) {
	problem := `{"title":"Unprocessable Entity","status":422,"detail":"validation failed","errors":[` +
		`{"message":"expected required property budget_accelerator_hours to be present","location":"body","value":{"name":"x","description":"` + strings.Repeat("long ", 200) + `"}},` +
		`{"message":"expected number >= 1","location":"body.max_agents","value":0}]}`
	url := clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(problem))
	})
	clitest.SetAPIURL(t, url)
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1:ag-1"))
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(HypothesisComment, []string{"h-1", "--agent", "ag-1", "--text", "t"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := "HTTP 422: body: expected required property budget_accelerator_hours to be present; max_agents: expected number >= 1\n"
	if said := stderr.String(); !strings.HasSuffix(said, want) || strings.Contains(said, "long") {
		t.Errorf("stderr = %q, want it to end with %q and carry no value", said, want)
	}
}
