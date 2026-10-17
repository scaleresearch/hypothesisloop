package shared

import (
	"bytes"
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
