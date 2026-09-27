package quota

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// hl hypothesis checkout names an agent's branch after the agent, and read the name from the
// roster, which an agent's token may not call: every researcher was refused its own branch.
// Its own signup is what an agent's token reads, so the name is answered there.
func TestAnAgentReadsItsOwnNameInItsSignup(t *testing.T) {
	store := newRosterStore(domain.PlatformExpRunning, 5)
	store.signups = []domain.AgentSignup{{PlatformExperimentID: "pe-1", AgentID: "dana", Name: "Researcher 1", Role: domain.SignupRoleCompetitor}}
	dana := auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: "dana", ProjectID: "project-a", OrgIDs: []string{"org-a"}}

	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(dana), auth.NewAccess(agentsTestPlacement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()))

	if code, out := call(t, r, "GET", "/platform-experiments/pe-1/signups/dana", ""); code != http.StatusOK || out["name"] != "Researcher 1" {
		t.Errorf("an agent reading its own signup: got = %d %v, want = 200 with name Researcher 1", code, out)
	}
}
