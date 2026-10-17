package platformexperiments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

func (f *rosterStore) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return f.quota, nil
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
	due := time.Now().UTC().Add(10*time.Minute - time.Second).Truncate(time.Second)
	const add, declare = "`hl platform-experiments agents add --hosted --role baseline --platform-experiment pe-1`", "`hl platform-experiments update --baseline-job JOB --platform-experiment pe-1`"
	job := func(id string) *domain.Experiment {
		measured := *measuredBaseline().Job
		measured.ID = id
		return &measured
	}

	check := job("job-2")
	check.Branch = "hl/pe-1/baseline-check"
	building := &domain.Experiment{ID: "job-9", Status: domain.StatusQueued, NotAdmittedReason: domain.NotAdmittedEnvironmentBuilding, EstimatedDurationHours: 0.5}
	quick := &domain.Experiment{ID: "job-8", Status: domain.StatusSubmitted, EstimatedDurationHours: 0.1}
	for name, c := range map[string]struct {
		reader       auth.Principal
		signups      []domain.AgentSignup
		left         []*domain.Experiment
		objective    []*domain.Experiment
		budget       float64
		reason       string
		names, omits []string
	}{
		"no baseline agent":                      {waitingCoordinator, []domain.AgentSignup{researcher}, nil, nil, 6, "baseline_required", []string{add}, []string{declare}},
		"a baseline agent at work":               {waitingCoordinator, []domain.AgentSignup{measuring, researcher}, nil, nil, 6, "baseline_measuring", []string{"Baseline 1", "the platform declares"}, []string{add, declare}},
		"one that finished with no job":          {waitingPerson, []domain.AgentSignup{done, researcher}, nil, nil, 6, "baseline_required", []string{add, `Baseline 1 finished with the reason "train.py does not run"`}, []string{declare}},
		"one whose machine stopped for good":     {waitingCoordinator, []domain.AgentSignup{stopped}, nil, nil, 6, "baseline_required", []string{add, "Baseline 1's machine stopped with no restart left"}, nil},
		"a second at work after the first ended": {waitingCoordinator, []domain.AgentSignup{done, second}, nil, nil, 6, "baseline_measuring", []string{"Baseline 2"}, []string{add}},
		"one that left two jobs":                 {waitingCoordinator, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1"), job("job-2")}, nil, 6, "baseline_undeclared", []string{"job-1, job-2", declare}, []string{add}},
		"one that left a job the budget is under": {waitingPerson, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1")}, nil, 1, "budget_below_baseline",
			[]string{"the platform did not declare job job-1", "its owner raises budget_accelerator_hours", "declares it in the request that clears this"}, []string{add, declare}},
		"one that left a job the sweep has yet to declare": {waitingPerson, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1")}, nil, 6, "baseline_declaring", []string{"job-1"}, []string{add, declare}},
		"one that left the control and its check":          {waitingPerson, []domain.AgentSignup{done}, []*domain.Experiment{job("job-1"), check}, nil, 6, "baseline_declaring", []string{"job-1"}, []string{"job-2", add, declare}},
		"the baseline agent":                               {waitingAgent("ag-b"), []domain.AgentSignup{measuring, researcher}, nil, nil, 6, "baseline_measuring", []string{"Your job is admitted now", "hl job summary", "agents finish", "baseline_due_at is " + due.Format(time.RFC3339) + ", 9 minutes from now", "The run lasts its duration_hours, 1, from the moment the platform starts it, once its baseline is declared"}, []string{add, "cannot be met"}},
		"the baseline agent whose job cannot end in time": {waitingAgent("ag-b"), []domain.AgentSignup{measuring, researcher}, nil, []*domain.Experiment{quick, building}, 6, "baseline_measuring",
			[]string{"job job-9 is QUEUED (environment_building) and its estimated_duration_hours is 0.5: started now it ends after baseline_due_at, 9 minutes from now, so baseline_due_at cannot be met by it"}, []string{"job-8"}},
		"a baseline agent that finished": {waitingAgent("ag-b"), []domain.AgentSignup{done, second}, nil, nil, 6, "baseline_measuring", nil, []string{"Your job is admitted now", "Every job you submit"}},
		"a researcher":                   {waitingAgent("ag-r"), []domain.AgentSignup{measuring, researcher}, nil, nil, 6, "baseline_measuring", []string{"Every job you submit is refused until the platform starts the run", "end your turn", "The run lasts its duration_hours, 1"}, []string{add, "baseline_due_at is"}},
	} {
		store := newRosterStore(domain.PlatformExpOpen, 2)
		store.pe.CoordinatorHosted, store.signups, store.left, store.objective = true, c.signups, c.left, c.objective
		store.pe.Metrics, store.pe.BudgetAcceleratorHours, store.pe.BaselineDueAt, store.pe.DurationHours = openExperiment().Metrics, c.budget, due, 1
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

// A run whose last researcher had finished read running until its ends_at, with machines kept
// and nobody told to close it. It reads researchers_done once no researcher can still work, and
// names who closes it: its coordinator where the platform hosts one, its owner otherwise.
func TestARunningPlatformExperimentWithNoResearcherLeftSaysSoAndWhoClosesIt(t *testing.T) {
	finished := time.Now()
	atWork := domain.AgentSignup{AgentID: "ag-1", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-1", Reported: domain.InstanceRunning}}
	done := domain.AgentSignup{AgentID: "ag-2", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{}, FinishedAt: &finished, Ended: domain.RunEndFinished}
	cut := domain.AgentSignup{AgentID: "ag-3", Role: domain.SignupRoleCompetitor, Ended: domain.RunEndCut}
	stopped := domain.AgentSignup{AgentID: "ag-4", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Stopped: true}}
	selfRun := domain.AgentSignup{AgentID: "ag-5", Role: domain.SignupRoleCompetitor}
	baseline := domain.AgentSignup{AgentID: "ag-b", Role: domain.SignupRoleBaseline, Hosted: true, Instance: &domain.AgentInstance{}, FinishedAt: &finished, Ended: domain.RunEndFinished}
	reviewer := domain.AgentSignup{AgentID: "ag-v", Role: domain.SignupRoleReviewer}

	for name, c := range map[string]struct {
		signups           []domain.AgentSignup
		coordinatorHosted bool
		clockRuns         bool
		reason            string
		waitsFor          domain.Waiter
	}{
		"one at work beside those that ended":    {[]domain.AgentSignup{baseline, done, cut, stopped, atWork}, true, true, "running", domain.WaitsForPlatform},
		"one its owner runs, which says nothing": {[]domain.AgentSignup{baseline, done, selfRun}, true, true, "running", domain.WaitsForPlatform},
		"every one finished, cut or stopped":     {[]domain.AgentSignup{baseline, done, cut, stopped, reviewer}, true, true, "researchers_done", domain.WaitsForCoordinator},
		"the same with a person coordinating":    {[]domain.AgentSignup{baseline, done, cut, stopped}, false, true, "researchers_done", domain.WaitsForOwner},
		"none up yet and its clock not started":  {[]domain.AgentSignup{baseline, stopped}, true, false, "agents_starting", domain.WaitsForPlatform},
	} {
		store := newRosterStore(domain.PlatformExpRunning, 5)
		store.pe.CoordinatorHosted, store.signups = c.coordinatorHosted, c.signups
		if c.clockRuns {
			store.pe.StartedAt = finished.Add(-time.Hour)
		}
		store.pe.SetClosesAt(finished.Add(time.Hour))
		if err := newSignupTestService(t, store).Describe(context.Background(), store.pe, waitingPerson); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		why := store.pe.StatusReason
		if why == nil || why.Reason != c.reason || why.WaitsFor != c.waitsFor {
			t.Errorf("%s: status_reason = %+v, want %s waiting for %s", name, why, c.reason, c.waitsFor)
			continue
		}
		closes := strings.Contains(why.Message, "`hl platform-experiments close --platform-experiment pe-1`") && strings.Contains(why.Message, "`hl platform-experiments summary --file FILE --platform-experiment pe-1`")
		if closes != (c.reason == "researchers_done") {
			t.Errorf("%s: message %q names the summary and the close = %v", name, why.Message, closes)
		}
		if ends := "ends_at, " + store.pe.EndsAt.UTC().Format(time.RFC3339) + ", "; !strings.Contains(why.Message, ends) {
			t.Errorf("%s: message %q does not say %q: an agent reads the run's end from the platform, not from its brief", name, why.Message, ends)
		}
		if store.pe.Status != domain.PlatformExpRunning {
			t.Errorf("%s: it is %s, want it left running for whoever closes it", name, store.pe.Status)
		}
	}
}

// toldStore keeps what the sweep tells the coordinator of its one platform experiment.
type toldStore struct {
	*rosterStore
	told []string
}

func (f *toldStore) ListPlatformExperimentsByStatus(_ context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	if f.pe.Status != status {
		return nil, nil
	}
	return []*domain.PlatformExperiment{f.pe}, nil
}

func (f *toldStore) TellCoordinator(_ context.Context, _, reason string) error {
	f.told = append(f.told, reason)
	return nil
}

// A coordinator was woken when the last researcher finished, by agent.finished, and by nothing
// when the last one's machine stopped with no restart left: the run read researchers_done and
// its coordinator slept until ends_at. The sweep records the reason that waits for a hosted
// coordinator, and none while the experiment waits for someone else.
func TestTheSweepTellsAHostedCoordinatorTheReasonThatWaitsForIt(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	atWork := domain.AgentSignup{AgentID: "ag-1", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-1", Reported: domain.InstanceRunning}}
	stopped := domain.AgentSignup{AgentID: "ag-1", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Stopped: true}}
	cut := domain.AgentSignup{AgentID: "ag-2", Role: domain.SignupRoleCompetitor, Ended: domain.RunEndCut}

	for name, c := range map[string]struct {
		status            domain.PlatformExperimentStatus
		coordinatorHosted bool
		signups           []domain.AgentSignup
		told              []string
	}{
		"a researcher at work":                    {domain.PlatformExpRunning, true, []domain.AgentSignup{atWork, cut}, []string{""}},
		"the last one's machine stopped for good": {domain.PlatformExpRunning, true, []domain.AgentSignup{stopped, cut}, []string{"researchers_done"}},
		"the same with a person coordinating":     {domain.PlatformExpRunning, false, []domain.AgentSignup{stopped, cut}, nil},
		"open with no baseline agent":             {domain.PlatformExpOpen, true, []domain.AgentSignup{atWork}, []string{"baseline_required"}},
		"closed":                                  {domain.PlatformExpClosed, true, []domain.AgentSignup{stopped}, nil},
	} {
		store := &toldStore{rosterStore: newRosterStore(c.status, 5)}
		store.pe.CoordinatorHosted, store.signups, store.pe.StartedAt = c.coordinatorHosted, c.signups, started
		store.pe.Metrics = openExperiment().Metrics
		if err := newSignupTestService(t, store).TellCoordinators(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Join(store.told, ",") != strings.Join(c.told, ",") || len(store.told) != len(c.told) {
			t.Errorf("%s: told = %q, want %q", name, store.told, c.told)
		}
	}
}

// hl quota asked the platform experiment a second time to explain a 404 before the start. The
// refusal says it itself: quota is granted at the start, and what its reader does until then.
func TestAQuotaAskedForBeforeTheStartIsRefusedWithTheReadersNextStep(t *testing.T) {
	store := newRosterStore(domain.PlatformExpOpen, 5)
	store.signups = []domain.AgentSignup{{AgentID: "dana", Name: "Researcher 1", Role: domain.SignupRoleCompetitor}}
	r := authtest.Router(authtest.As(waitingAgent("dana")))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()))

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
	r := authtest.Router(authtest.As(waitingAgent("pat")))
	doc := apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(agentsTestPlacement), zap.NewNop())
	registerHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(newSignupTestService(t, store), store, testPlanning, zap.NewNop()))

	code, out := call(t, r, "GET", "/platform-experiments/pe-1/quotas/pat", "")
	message, _ := out["message"].(string)
	if code != http.StatusNotFound || out["reason"] != "planner_has_no_quota" || !strings.Contains(message, "a planner submits no job, so it has no quota and needs none") ||
		!strings.Contains(message, "budget_accelerator_hours its proposal states") {
		t.Errorf("got = %d %v, want 404 planner_has_no_quota saying a planner needs none and where the budget comes from", code, out)
	}
}

func TestTheRosterOfARunningPlatformExperimentSaysWhichMachinesAreStillStarting(t *testing.T) {
	store := newRosterStore(domain.PlatformExpRunning, 5)
	asked := time.Now().Add(-40 * time.Second)
	signups := []domain.AgentSignup{
		{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-1", AskedAt: &asked}},
		{AgentID: "eli", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Name: "m-2", AskedAt: &asked, SeenAt: &asked, Reported: domain.InstanceRunning}},
	}
	roster, err := newSignupTestService(t, store).roster(context.Background(), "pe-1", signups, domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if why := roster[0].StatusReason; why == nil || why.Reason != "instance_starting" || !strings.Contains(why.Message, "asked for less than a minute ago") ||
		why.Summary != "Its machine was requested less than a minute ago and is on its way." {
		t.Errorf("a machine not listed yet: got = %+v", why)
	}
	if roster[1].StatusReason != nil {
		t.Errorf("a running machine: got = %+v, want no status_reason", roster[1].StatusReason)
	}

	// Before the start a researcher has no machine asked for, and says that and not a start.
	store.pe.Status = domain.PlatformExpOpen
	store.signups = signups
	roster, err = newSignupTestService(t, store).roster(context.Background(), "pe-1", signups[:1], domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if why := roster[0].StatusReason; why == nil || why.Reason == "instance_starting" || !strings.Contains(why.Message, "its machine is asked for when the platform starts the run") ||
		why.Summary != "Its machine starts with the run." {
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
	roster, err := svc.roster(context.Background(), "pe-1", store.signups, domain.Actor{})
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
	if roster, err = svc.roster(context.Background(), "pe-1", store.signups, domain.Actor{}); err != nil {
		t.Fatal(err)
	}
	if got := state(roster[0]); got != "requested" {
		t.Errorf("a researcher after the start: state %q, want requested", got)
	}
}

// A researcher of an experiment that closed before its run started read stopped with no reason,
// as if its machine had failed.
func TestAResearcherOfAnExperimentClosedBeforeItsRunSaysItNeverLaunchedAndWhy(t *testing.T) {
	store := newRosterStore(domain.PlatformExpClosed, 5)
	store.pe.CloseReason = domain.CloseBaselineLimitReached
	store.submitted = map[string]int{"bo": 2}
	asked := time.Now()
	store.signups = []domain.AgentSignup{
		{AgentID: "dana", Role: domain.SignupRoleCompetitor, Hosted: true, Instance: &domain.AgentInstance{Released: true}},
		{AgentID: "bo", Role: domain.SignupRoleBaseline, Hosted: true, Instance: &domain.AgentInstance{AskedAt: &asked, Released: true}},
	}
	roster, err := newSignupTestService(t, store).roster(context.Background(), "pe-1", store.signups, domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if why := roster[0].StatusReason; why == nil || why.Reason != "never_launched" || !strings.Contains(why.Message, string(domain.CloseBaselineLimitReached)) || !strings.Contains(why.Message, "2 jobs were submitted to measure its baseline") || why.WaitsFor != "" {
		t.Errorf("a researcher whose run never started: got = %+v, want never_launched naming the close_reason, the baseline jobs submitted, and waiting for nobody", why)
	}
	if roster[1].StatusReason != nil {
		t.Errorf("the baseline agent, whose machine ran: got = %+v, want no reason", roster[1].StatusReason)
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

// tendedStore records what the baseline sweep asks of the store: whether each pass saw the
// baseline wait on the platform, and the jobs it told their agent were late, with why. A held
// pass moves baseline_due_at by moveBy.
type tendedStore struct {
	*toldStore
	held   []bool
	late   map[string]string
	moveBy time.Duration
}

func (f *tendedStore) HoldBaselineClock(_ context.Context, _ string, held bool, most time.Duration) error {
	if most != domain.MaxEnvironmentBuildTime {
		return fmt.Errorf("bound %v, want the longest the platform spends on a build", most)
	}
	f.held = append(f.held, held)
	if held {
		f.pe.BaselineDueAt = f.pe.BaselineDueAt.Add(f.moveBy)
	}
	return nil
}

func (f *tendedStore) MarkBaselineLate(_ context.Context, id, reason string) error {
	f.late[id] = reason
	return nil
}

// A baseline job waited 17 minutes for its environment and the platform closed the
// experiment while the job pulled its image: the platform's time was counted against the
// baseline, and its agent, never told its job could not end in time, slept until the close. The
// sweep moves baseline_due_at by the time a job waits on the platform, and tells the agent of a
// job that cannot end by it.
func TestTheBaselineSweepCountsThePlatformsTimeAsThePlatformsAndWakesALateBaseline(t *testing.T) {
	due := time.Now().UTC().Add(10 * time.Minute)
	building := &domain.Experiment{ID: "job-building", Status: domain.StatusQueued, NotAdmittedReason: domain.NotAdmittedEnvironmentBuilding, EstimatedDurationHours: 0.75}
	pulling := &domain.Experiment{ID: "job-pulling", Status: domain.StatusSubmitted, EstimatedDurationHours: 0.1}
	capacity := &domain.Experiment{ID: "job-capacity", Status: domain.StatusQueued, NotAdmittedReason: "capacity_unavailable", EstimatedDurationHours: 0.1}
	running := &domain.Experiment{ID: "job-running", Status: domain.StatusRunning, EstimatedDurationHours: 2}
	for name, c := range map[string]struct {
		jobs     []*domain.Experiment
		declared string
		held     []bool
		late     []string
		due      time.Time
	}{
		"a job whose environment is built": {[]*domain.Experiment{building}, "", []bool{true}, []string{"job-building"}, due.Add(time.Minute)},
		"a job its cluster is starting":    {[]*domain.Experiment{pulling, running}, "", []bool{true}, nil, due.Add(time.Minute)},
		"a job that waits for room":        {[]*domain.Experiment{capacity}, "", []bool{false}, nil, due},
		"a baseline declared":              {[]*domain.Experiment{building}, "job-1", nil, nil, due},
	} {
		store := &tendedStore{toldStore: &toldStore{rosterStore: newRosterStore(domain.PlatformExpOpen, 2)}, late: map[string]string{}, moveBy: time.Minute}
		store.pe.BaselineDueAt, store.pe.BaselineJobID, store.objective = due, c.declared, c.jobs
		if err := newSignupTestService(t, store).TendBaselines(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fmt.Sprint(store.held) != fmt.Sprint(c.held) || !store.pe.BaselineDueAt.Equal(c.due) {
			t.Errorf("%s: held = %v, due = %v, want %v, %v", name, store.held, store.pe.BaselineDueAt, c.held, c.due)
		}
		if len(store.late) != len(c.late) {
			t.Errorf("%s: told late = %v, want %v", name, store.late, c.late)
		}
		for _, id := range c.late {
			reason := store.late[id]
			for _, want := range []string{domain.HeldBaselineLate + ": job " + id + " is QUEUED (environment_building)", "baseline_due_at cannot be met by it",
				"baseline_due_at is " + c.due.Format(time.RFC3339), "moves out by that time, at most 90 minutes in all."} {
				if !strings.Contains(reason, want) {
					t.Errorf("%s: told %s %q, want it to say %q", name, id, reason, want)
				}
			}
		}
	}
}
