package httpapi

import (
	"net/http"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// hl hypothesis checkout names an agent's branch after the agent, and read the name from the
// roster, which an agent's token may not call: every agent was refused its own branch.
// Its own signup is what an agent's token reads, so the name is answered there.
func TestAnAgentReadsItsOwnNameInItsSignup(t *testing.T) {
	store := newRosterStore(domain.PlatformExpRunning, 5)
	store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Name: "Executor 1", Role: domain.SignupRoleCompetitor}}
	dana := auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: "dana", ProjectID: "project-a", OrgIDs: []string{"org-a"}}

	r := authtest.Router(authtest.As(dana))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerPlatformExperimentsAndBudget(doc, newSignupTestService(t, store).WithRepositories(store).WithPlanning(testPlanning))

	if code, out := call(t, r, "GET", "/platform-experiments/pe-1/signups/dana", ""); code != http.StatusOK || out["name"] != "Executor 1" {
		t.Errorf("an agent reading its own signup: got = %d %v, want = 200 with name Executor 1", code, out)
	} else if ended, sent := out["ended"]; !sent || ended != "" {
		// Left out while the run goes on, every reader had to supply the default itself.
		t.Errorf("a signup whose run goes on: ended = %v (sent: %v), want it sent as the empty string", ended, sent)
	}
}
