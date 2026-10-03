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

// A coordinator told only that a baseline was required waited for one, and a person read the same
// words whether an agent was measuring, had left nothing, or had left a job the budget could not
// hold. Without a baseline status_reason says where the measuring stands, by a reason of its own
// for each state, and names the command of whoever acts.
func TestStatusReasonOfAnOpenPlatformExperimentSaysWhereItsBaselineStands(t *testing.T) {
	finished := time.Now()
	measuring := domain.AgentSignup{AgentID: "ag-b", Name: "Baseline 1", Role: domain.SignupRoleBaseline, Hosted: true}
	done := measuring
	done.FinishedAt, done.FinishedReason = &finished, "train.py does not run"
	stopped := measuring
	stopped.Instance = &domain.AgentInstance{Stopped: true}
	second := domain.AgentSignup{AgentID: "ag-c", Name: "Baseline 2", Role: domain.SignupRoleBaseline, Hosted: true}
	researcher := domain.AgentSignup{AgentID: "ag-r", Name: "Researcher 1", Role: domain.SignupRoleCompetitor, Hosted: true}
	const add, declare = "`hl platform-experiments agents add --hosted --role baseline --platform-experiment pe-1`", "`hl platform-experiments update --baseline-job JOB --platform-experiment pe-1`"
	job := func(id string) *domain.Experiment {
		measured := *measuredBaseline().Job
		measured.ID = id
		return &measured
	}

	for name, c := range map[string]struct {
		reader       auth.Principal
		signups      []domain.AgentSignup
		left         []*domain.Experiment
		budget       float64
		reason       string
		names, omits []string
	}{
		"no baseline agent":                      {waitingCoordinator, []domain.AgentSignup{researcher}, nil, 6, "baseline_required", []string{add}, []string{declare}},
		"a baseline agent at work":               {waitingCoordinator, []domain.AgentSignup{measuring, researcher}, nil, 6, "baseline_measuring", []string{"Baseline 1", "the platform declares"}, []string{add, declare}},
		"one that finished with no job":          {waitingPerson, []domain.AgentSignup{done, researcher}, nil, 6, "baseline_required", []string{add, `Baseline 1 finished with the reason "train.py does not run"`}, []string{declare}},
		"one whose machine stopped for good":     {waitingCoordinator, []domain.AgentSignup{stopped}, nil, 6, "baseline_required", []string{add, "Baseline 1's machine stopped with no restart left"}, nil},
		"a second at work after the first ended": {waitingCoordinator, []domain.AgentSignup{done, second}, nil, 6, "baseline_measuring", []string{"Baseline 2"}, []string{add}},
		"one that left two jobs":                 {waitingCoordinator, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1"), job("job-2")}, 6, "baseline_undeclared", []string{"job-1, job-2", declare}, []string{add}},
		"one that left a job the budget is under": {waitingPerson, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1")}, 1, "budget_below_baseline",
			[]string{"the platform did not declare job job-1", "its owner raises budget_accelerator_hours", "declares it in the request that clears this"}, []string{add, declare}},
		"one that left a job the sweep has yet to declare": {waitingPerson, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1")}, 6, "baseline_declaring", []string{"job-1"}, []string{add, declare}},
		"the baseline agent":                               {waitingAgent("ag-b"), []domain.AgentSignup{measuring, researcher}, nil, 6, "baseline_measuring", []string{"Your job is admitted now", "hl job summary", "agents finish"}, []string{add}},
		"a baseline agent that finished":                   {waitingAgent("ag-b"), []domain.AgentSignup{done, second}, nil, 6, "baseline_measuring", nil, []string{"Your job is admitted now", "Every job you submit"}},
		"a researcher":                                     {waitingAgent("ag-r"), []domain.AgentSignup{measuring, researcher}, nil, 6, "baseline_measuring", []string{"Every job you submit is refused until the platform starts the run", "end your turn"}, []string{add}},
	} {
		store := newRosterStore(domain.PlatformExpOpen, 2)
		store.pe.CoordinatorHosted, store.signups, store.left = true, c.signups, c.left
		store.pe.Metrics, store.pe.BudgetAcceleratorHours = openExperiment().Metrics, c.budget
		svc := newSignupTestService(t, store)
		svc.baseline = func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*Baseline, error) {
			baseline := measuredBaseline()
			baseline.Job = job(jobID)
			return baseline, nil
		}
		if err := svc.Describe(context.Background(), store.pe, c.reader); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		why := store.pe.StatusReason
		if why == nil || why.Reason != c.reason {
			t.Errorf("%s: status_reason = %+v, want %s", name, why, c.reason)
			continue
		}
		if who, err := domain.WaitsFor(c.reason, true); err != nil || why.WaitsFor != who {
			t.Errorf("%s: waits_for = %q, want %q, %v", name, why.WaitsFor, who, err)
		}
		for _, want := range c.names {
			if !strings.Contains(why.Message, want) {
				t.Errorf("%s: message %q does not name %q", name, why.Message, want)
			}
		}
		for _, not := range c.omits {
			if strings.Contains(why.Message, not) {
				t.Errorf("%s: message %q names %q, which is nobody's step in this state", name, why.Message, not)
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
	for _, reader := range []auth.Principal{waitingCoordinator, waitingPerson, waitingAgent("ag-b")} {
		if step := nextStep("starts_at_ahead", signups, reader); step != "" {
			t.Errorf("%s past the baseline: step %q, want none", reader.Kind, step)
		}
	}
	if step := nextStep("starts_at_ahead", signups, waitingAgent("ag-r")); !strings.HasPrefix(step, "Every job you submit is refused") {
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

// The roster said which machines ran and nothing of what their agents did. Each entry answers
// what its agent is doing now and since when, and one whose machine says nothing answers none.
func TestTheRosterAnswersWhatEachHostedAgentIsDoing(t *testing.T) {
	store := newRosterStore(domain.PlatformExpRunning, 5)
	since := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	running := &domain.AgentInstance{Name: "m-1", AskedAt: &since, SeenAt: &since, Reported: domain.InstanceRunning}
	store.signups = []domain.AgentSignup{
		{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: running, Activity: &domain.AgentActivity{Text: "running hl job submit", At: since}},
		{AgentID: "eli", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: running},
	}
	code, out := call(t, agentsRouter(t, store, waitingPerson), "GET", "/platform-experiments/pe-1/agents", "")
	if code != http.StatusOK {
		t.Fatalf("list: got = %d %v", code, out)
	}
	roster := joined(t, out)
	doing, _ := roster[0]["activity"].(map[string]any)
	if doing["text"] != "running hl job submit" || doing["at"] != "2026-10-06T12:00:00Z" {
		t.Errorf("an agent at work: activity %v, want its sentence and since when", roster[0]["activity"])
	}
	if _, says := roster[1]["activity"]; says {
		t.Errorf("an agent whose machine said nothing: activity %v, want none", roster[1]["activity"])
	}
}
