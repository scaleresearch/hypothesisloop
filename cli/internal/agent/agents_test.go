package agent

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// An agent that finished after the run closed got a bare 409: the close already ended its run, so
// hl says nothing needs finishing and succeeds.
func TestFinishingAfterTheCloseSaysNothingNeedsFinishing(t *testing.T) {
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"experiment_closed","message":"the platform experiment is closed"}`))
	}))
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-env/ag-1"))
	var stdout, stderr bytes.Buffer
	code := clitest.Run(AgentsFinish, []string{"--reason", "done"}, &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "the platform experiment is closed, which ended your run: nothing needs finishing") {
		t.Errorf("exit %d, stdout %q, stderr %q, want 0 and the close said plainly", code, stdout.String(), stderr.String())
	}
}
