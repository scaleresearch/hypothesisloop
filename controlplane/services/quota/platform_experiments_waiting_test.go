package quota

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func (f *rosterStore) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return nil, nil
}

var (
	waitingCoordinator = auth.Principal{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}}
	waitingPerson      = auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}
)

func waitingAgent(id string) auth.Principal {
	return auth.Principal{Kind: auth.KindAgent, PlatformExperimentID: "pe-1", AgentID: id, OrgIDs: []string{"org-a"}}
}

// hl printed each caller's next step on stderr beside the record, which broke every reader that
// parsed both streams as JSON, and a coordinator told only that a baseline was required waited
// for one. status_reason is the one place that says what an open platform experiment waits for,
// and its message begins with what its reader does, the command included.
func TestStatusReasonOfAnOpenPlatformExperimentNamesItsReadersNextStep(t *testing.T) {
	finished := time.Now()
	measuring := domain.AgentSignup{AgentID: "ag-b", Name: "Baseline 1", Role: domain.SignupRoleBaseline, Hosted: true}
	done := measuring
	done.FinishedAt = &finished
	stopped := measuring
	stopped.Instance = &domain.AgentInstance{Stopped: true}
	researcher := domain.AgentSignup{AgentID: "ag-r", Name: "Researcher 1", Role: domain.SignupRoleCompetitor, Hosted: true}
	const add, declare = "`hl platform-experiments agents add --hosted --role baseline`", "`hl platform-experiments update --baseline-job JOB"

	for name, c := range map[string]struct {
		reader            auth.Principal
		signups           []domain.AgentSignup
		coordinatorHosted bool
		begins            string
		names, omits      []string
	}{
		"a coordinator with no baseline agent":      {waitingCoordinator, []domain.AgentSignup{researcher}, true, "Add a baseline agent with " + add, []string{declare}, nil},
		"a coordinator whose baseline agent works":  {waitingCoordinator, []domain.AgentSignup{measuring, researcher}, true, "Baseline agent Baseline 1 is measuring", []string{declare}, []string{add}},
		"a coordinator whose baseline agent ended":  {waitingCoordinator, []domain.AgentSignup{done, researcher}, true, "Every baseline agent (Baseline 1) finished or stopped", []string{declare, add}, nil},
		"a coordinator whose baseline machine died": {waitingCoordinator, []domain.AgentSignup{stopped}, true, "Every baseline agent (Baseline 1) finished or stopped", []string{declare, add}, nil},
		"the baseline agent":                        {waitingAgent("ag-b"), []domain.AgentSignup{measuring, researcher}, true, "Your job is admitted now", []string{"hl job summary", "agents finish"}, []string{add}},
		"a researcher":                              {waitingAgent("ag-r"), []domain.AgentSignup{measuring, researcher}, true, "Every job you submit is refused until the platform starts the run", []string{"end your turn"}, []string{add}},
		"a person who coordinates":                  {waitingPerson, nil, false, "Add a baseline agent with " + add, []string{declare}, nil},
		"a person whose coordinator is hosted":      {waitingPerson, nil, true, "Its coordinator adds the agent of role baseline", nil, []string{add}},
	} {
		store := newRosterStore(domain.PlatformExpOpen, 5)
		store.pe.CoordinatorHosted, store.signups = c.coordinatorHosted, c.signups
		if err := newSignupTestService(t, store).Describe(context.Background(), store.pe, c.reader); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		why := store.pe.StatusReason
		if why == nil || why.Reason != "baseline_required" || !strings.HasPrefix(why.Message, c.begins) || !strings.HasSuffix(why.Message, "pe-1 has no baseline declared, and the platform starts the run only once it has") {
			t.Errorf("%s: status_reason = %+v, want baseline_required, beginning %q and ending in what the start waits for", name, why, c.begins)
			continue
		}
		for _, want := range c.names {
			if !strings.Contains(why.Message, want) {
				t.Errorf("%s: message %q does not name %q", name, why.Message, want)
			}
		}
		for _, not := range c.omits {
			if strings.Contains(why.Message, not) {
				t.Errorf("%s: message %q names %q, which is not this reader's step", name, why.Message, not)
			}
		}
	}
}

// Once the baseline is declared nobody but the platform acts, except a researcher, who still
// prepares: the step is said for the reason it belongs to and for no other.
func TestStatusReasonNamesNoStepWhereTheReaderOnlyWaits(t *testing.T) {
	signups := []domain.AgentSignup{
		{AgentID: "ag-b", Name: "Baseline 1", Role: domain.SignupRoleBaseline},
		{AgentID: "ag-r", Name: "Researcher 1", Role: domain.SignupRoleCompetitor},
	}
	pe := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen}
	for _, reader := range []auth.Principal{waitingCoordinator, waitingPerson, waitingAgent("ag-b")} {
		if step := nextStep(pe, "starts_at_ahead", signups, reader); step != "" {
			t.Errorf("%s past the baseline: step %q, want none", reader.Kind, step)
		}
	}
	if step := nextStep(pe, "starts_at_ahead", signups, waitingAgent("ag-r")); !strings.HasPrefix(step, "Every job you submit is refused") {
		t.Errorf("a researcher past the baseline: step %q, want it told its jobs are refused until the start", step)
	}
}

// hl quota asked the platform experiment a second time to explain a 404 before the start. The
// refusal says it itself: quota is granted at the start, and what its reader does until then.
func TestAQuotaAskedForBeforeTheStartIsRefusedWithTheReadersNextStep(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.signups = []domain.AgentSignup{{AgentID: "dana", Name: "Researcher 1", Role: domain.SignupRoleCompetitor}}
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(waitingAgent("dana")), auth.NewAccess(agentsTestPlacement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()))

	code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/dana", "")
	message, _ := out["message"].(string)
	if code != http.StatusNotFound || out["reason"] != "quota_not_granted" ||
		!strings.HasPrefix(message, "agent dana holds no quota yet: it is granted when the platform experiment starts. Every job you submit is refused until the platform starts the run") {
		t.Errorf("open: got = %d %v, want 404 quota_not_granted with the researcher's next step", code, out)
	}
	store.pe.Status = domain.PlatformExpRunning
	if code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/dana", ""); code != http.StatusNotFound || out["reason"] == "quota_not_granted" {
		t.Errorf("running: got = %d %v, want a plain 404", code, out)
	}
}
