package quota

import (
	"context"
	"encoding/json"
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

// A planner read "has no quota" as something missing that it was owed.
func TestAPlannersQuotaIsRefusedSayingItNeedsNone(t *testing.T) {
	store := newRosterStore(domain.PlatformExpDraft, 5)
	store.signups = []domain.AgentSignup{{AgentID: "pat", Name: "Planner", Role: domain.SignupRolePlanner}}
	r := chi.NewRouter()
	doc := apidocs.New(r, "test", "1.0.0", "", authtest.As(waitingAgent("pat")), auth.NewAccess(agentsTestPlacement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()))

	code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/pat", "")
	message, _ := out["message"].(string)
	if code != http.StatusNotFound || out["reason"] != "planner_has_no_quota" || !strings.Contains(message, "a planner submits no job, so it has no quota and needs none") {
		t.Errorf("got = %d %v, want 404 planner_has_no_quota saying a planner needs none", code, out)
	}
}

// A researcher's machine asked for 40 seconds after the start read instance.state requested with
// nothing beside it, and was taken for a launch that failed.
func TestAnAgentWhoseMachineIsOnItsWayCarriesInstanceStarting(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	asked := now.Add(-40 * time.Second)
	hostedWith := func(instance domain.AgentInstance) domain.AgentSignup {
		return domain.AgentSignup{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &instance}
	}
	starting := map[string]struct {
		signup domain.AgentSignup
		says   string
	}{
		"not asked for yet":  {hostedWith(domain.AgentInstance{}), "its machine is asked of its launcher at the launcher's next pass"},
		"asked, not listed":  {hostedWith(domain.AgentInstance{Name: "m-1", AskedAt: &asked}), "its machine was asked for 40s ago and its launcher has not listed it yet"},
		"listed as starting": {hostedWith(domain.AgentInstance{Name: "m-1", AskedAt: &asked, SeenAt: &asked, Reported: domain.InstanceStarting}), "its machine was asked for 40s ago and is starting"},
	}
	for name, c := range starting {
		why := instanceStarting(c.signup, now)
		if why == nil || why.Reason != "instance_starting" || !strings.HasPrefix(why.Message, c.says+": a start in progress and no failure") {
			t.Errorf("%s: got = %+v", name, why)
		}
	}

	ended := hostedWith(domain.AgentInstance{})
	ended.Ended = domain.RunEndCut
	for name, signup := range map[string]domain.AgentSignup{
		"running":            hostedWith(domain.AgentInstance{Name: "m-1", AskedAt: &asked, SeenAt: &asked, Reported: domain.InstanceRunning}),
		"stopped":            hostedWith(domain.AgentInstance{Stopped: true}),
		"released":           hostedWith(domain.AgentInstance{Released: true}),
		"its run is over":    ended,
		"not the platform's": {AgentID: "dana", Role: domain.SignupRoleCompetitor},
	} {
		if why := instanceStarting(signup, now); why != nil {
			t.Errorf("%s: got = %+v, want none", name, why)
		}
	}
}

func TestTheRosterOfARunningPlatformExperimentSaysWhichMachinesAreStillStarting(t *testing.T) {
	store := newRosterStore(domain.PlatformExpRunning, 5)
	asked := time.Now().Add(-40 * time.Second)
	signups := []domain.AgentSignup{
		{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-1", AskedAt: &asked}},
		{AgentID: "eli", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-2", AskedAt: &asked, SeenAt: &asked, Reported: domain.InstanceRunning}},
	}
	roster, err := newSignupTestService(t, store).roster(context.Background(), "pe-1", signups)
	if err != nil {
		t.Fatal(err)
	}
	if why := roster[0].StatusReason; why == nil || why.Reason != "instance_starting" || !strings.Contains(why.Message, "asked for 40s ago") {
		t.Errorf("a machine not listed yet: got = %+v", why)
	}
	if roster[1].StatusReason != nil {
		t.Errorf("a running machine: got = %+v, want no status_reason", roster[1].StatusReason)
	}

	// Before the start a researcher has no machine asked for, and says that and not a start.
	store.pe.Status = domain.PlatformExpOpen
	store.signups = signups
	roster, err = newSignupTestService(t, store).roster(context.Background(), "pe-1", signups[:1])
	if err != nil {
		t.Fatal(err)
	}
	if why := roster[0].StatusReason; why == nil || why.Reason == "instance_starting" || !strings.Contains(why.Message, "its machine is asked for when the platform starts the run") {
		t.Errorf("before the start: got = %+v", why)
	}
}

// A researcher read instance.state requested from the accept on, while no machine is asked for
// until the start.
func TestAResearchersInstanceReadsWaitingUntilThePlatformExperimentStarts(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.signups = []domain.AgentSignup{
		{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{}},
		{AgentID: "bo", Role: domain.SignupRoleBaseline, Hosted: true, Instance: &domain.AgentInstance{}},
		{AgentID: "eli", Role: domain.SignupRoleCompetitor},
	}
	state := func(agent JoinedAgent) string {
		t.Helper()
		body, err := json.Marshal(agent)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Instance *struct {
				State string `json:"state"`
			} `json:"instance"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Instance == nil {
			return ""
		}
		return out.Instance.State
	}
	svc := newSignupTestService(t, store)
	roster, err := svc.roster(context.Background(), "pe-1", store.signups)
	if err != nil {
		t.Fatal(err)
	}
	if got := state(roster[0]); got != "waiting" || roster[0].StatusReason == nil {
		t.Errorf("a researcher before the start: state %q with %+v, want waiting beside its status_reason", got, roster[0].StatusReason)
	}
	if got := state(roster[1]); got != "requested" {
		t.Errorf("a baseline agent, whose machine starts at once: state %q, want requested", got)
	}
	if got := state(roster[2]); got != "" {
		t.Errorf("an agent the platform does not run: instance state %q, want no instance", got)
	}
	if store.signups[0].Instance.Waiting {
		t.Error("the roster changed the signup it was given")
	}

	store.pe.Status = domain.PlatformExpRunning
	if roster, err = svc.roster(context.Background(), "pe-1", store.signups); err != nil {
		t.Fatal(err)
	}
	if got := state(roster[0]); got != "requested" {
		t.Errorf("a researcher after the start: state %q, want requested", got)
	}
}
