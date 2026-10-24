package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func waitingAgent(id string) auth.Principal {
	return auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: id, OrgIDs: []string{"org-a"}}
}

// hl quota asked the platform experiment a second time to explain a 404 before the start. The
// refusal says it itself: quota is granted at the start, and what its reader does until then.
func TestAQuotaAskedForBeforeTheStartIsRefusedWithTheReadersNextStep(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.signups = []domain.AgentSignup{{AgentID: "dana", Name: "Executor 1", Role: domain.SignupRoleCompetitor}}
	r := authtest.Router(authtest.As(waitingAgent("dana")))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerPlatformExperimentsAndBudget(doc, newSignupTestService(t, store).WithRepositories(store).WithPlanning(testPlanning))

	code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/dana", "")
	message, _ := out["message"].(string)
	if code != http.StatusNotFound || out["reason"] != "quota_not_granted" ||
		!strings.HasPrefix(message, "agent dana holds no quota yet: it is granted when the platform experiment starts. Every job you submit is refused until the platform starts the run") {
		t.Errorf("open: got = %d %v, want 404 quota_not_granted with the executor's next step", code, out)
	}
	store.pe.Status = domain.PlatformExpRunning
	if code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/dana", ""); code != http.StatusNotFound || out["reason"] == "quota_not_granted" {
		t.Errorf("running: got = %d %v, want a plain 404", code, out)
	}
}

// A planner read "has no quota" as something missing that it was owed.
func TestAPlannersQuotaIsRefusedSayingItNeedsNone(t *testing.T) {
	store := newRosterStore(domain.PlatformExpDraft, 5)
	store.signups = []domain.AgentSignup{{AgentID: "pat", Name: "Planner", Role: domain.SignupRolePlanner}}
	r := authtest.Router(authtest.As(waitingAgent("pat")))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerPlatformExperimentsAndBudget(doc, newSignupTestService(t, store).WithRepositories(store).WithPlanning(testPlanning))

	code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/pat", "")
	message, _ := out["message"].(string)
	if code != http.StatusNotFound || out["reason"] != "planner_has_no_quota" || !strings.Contains(message, "a planner submits no job, so it has no quota and needs none") ||
		!strings.Contains(message, "budget_accelerator_hours its proposal states") {
		t.Errorf("got = %d %v, want 404 planner_has_no_quota saying a planner needs none and where the budget comes from", code, out)
	}
}
