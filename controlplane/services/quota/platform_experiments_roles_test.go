package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// signupStore fakes only the reads and the write Signup makes; every other
// PlatformExperimentsStore method is inherited from the embedded nil interface, so a test that
// reaches one panics loudly instead of quietly getting a zero value back.
type signupStore struct {
	PlatformExperimentsStore
	pe          *domain.PlatformExperiment
	byRole      map[domain.SignupRole]int
	insertedFor domain.SignupRole
	// refuseInsert makes the guarded insert report inserted=false, which the store returns both
	// for a repeat signup and for an experiment that started since the status was read.
	refuseInsert bool
	// alreadySignedUp is whether the agent is a member before the call.
	alreadySignedUp bool
	// racedIn makes the read after a refused insert find the agent signed up, as when a
	// concurrent signup of the same agent committed first.
	racedIn      bool
	insertCalled bool
}

func (f *signupStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (f *signupStore) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) (bool, error) {
	role := signup.Role
	f.insertCalled = true
	if role == domain.SignupRoleCompetitor && f.byRole[role] >= f.pe.MaxAgents {
		return false, db.ErrMaxAgentsReached
	}
	f.insertedFor = role
	return !f.refuseInsert, nil
}

func (f *signupStore) IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error) {
	return f.alreadySignedUp || (f.insertCalled && f.racedIn), nil
}

func newSignupTestService(t *testing.T, store PlatformExperimentsStore) *PlatformExperimentsService {
	t.Helper()
	return reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", 3*time.Minute), domain.CPUType)
}

// max_agents sizes the field being ranked. If a baseline or a reviewer counted against it, adding
// the control an experiment measures against would silently shrink the competition it exists to
// measure — one fewer competitor for every non-competitor the coordinator launches.
func TestMaxAgentsCountsCompetitorsOnly(t *testing.T) {
	store := &signupStore{
		pe:     &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 2},
		byRole: map[domain.SignupRole]int{domain.SignupRoleCompetitor: 2, domain.SignupRoleBaseline: 0},
	}
	svc := newSignupTestService(t, store)

	if err := svc.Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-baseline", Role: domain.SignupRoleBaseline}); err != nil {
		t.Fatalf("baseline signup err = %v, want nil — max_agents is full of competitors, which must not block a baseline", err)
	}
	if got, want := store.insertedFor, domain.SignupRoleBaseline; got != want {
		t.Errorf("inserted role = %v, want %v", got, want)
	}
}

// The competitor limit still binds for competitors: roles widen who may join, never how many
// agents are ranked against each other.
func TestMaxAgentsStillRejectsACompetitorOnceTheFieldIsFull(t *testing.T) {
	store := &signupStore{
		pe:     &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 2},
		byRole: map[domain.SignupRole]int{domain.SignupRoleCompetitor: 2},
	}
	svc := newSignupTestService(t, store)

	err := svc.Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-3", Role: domain.SignupRoleCompetitor})
	var refused *api.Refusal
	if !errors.As(err, &refused) || refused.Reason != "max_agents_reached" || !strings.Contains(refused.Message, "researchers (agents of role competitor)") || !strings.Contains(refused.Message, "baseline") {
		t.Fatalf("err = %v, want max_agents_reached saying that researchers count and a baseline agent does not", err)
	}
}

// An unrecognized role is rejected rather than defaulted: silently reading a typo as "competitor"
// would rank, cut and pay a top-3 bonus to an agent nobody meant to enter in the competition.
func TestUnknownSignupRoleIsRejectedRatherThanDefaulted(t *testing.T) {
	if _, err := domain.ParseSignupRole("baselineee"); err == nil {
		t.Fatalf("err = %v, want an error for an unrecognized role", err)
	}
}

// An absent role means every caller written before roles existed keeps meaning what it meant.
func TestAbsentSignupRoleResolvesToCompetitor(t *testing.T) {
	role, err := domain.ParseSignupRole("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got, want := role, domain.SignupRoleCompetitor; got != want {
		t.Errorf("role = %v, want %v", got, want)
	}
}

// standingsStore adds the reads a ranking makes on top of signupStore's.
type standingsStore struct {
	signupStore
	competitors []string
}

// Every job these tests name completed.
func (f *standingsStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{"job-baseline": true, "job-c1": true, "job-c2": true, "job-c3": true}, nil
}

func (f *standingsStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	return f.competitors, nil
}

// job-c2 is on the objective of pe-1, as a baseline agent's job is stored.
func (f *standingsStore) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	job := &domain.Experiment{ID: id, CodeRef: "repo@" + id, HypothesisID: "hyp-" + id, PlatformExperimentID: "pe-1",
		Job: domain.JobSpec{Env: map[string]string{"LR": "0.1"}}}
	if id == "job-c2" {
		job.HypothesisID = "pe-1"
	}
	return job, nil
}

// The metrics store answers for every agent that reported, which is the point — a baseline's
// numbers stay fully readable. Ranking is where the role applies: a control that beats every
// treatment is a result to read, not the winner of the competition it was the control for.
func TestStandingsOmitABaselineAgentHoldingTheBestValue(t *testing.T) {
	srv := bestPerAgentMetricServer(t, map[string]float64{"baseline": 99, "c1": 50, "c2": 40})
	defer srv.Close()
	store := &standingsStore{competitors: []string{"c1", "c2"}}
	svc := newSignupTestService(t, store)
	svc.metricsDBURL = srv.URL

	pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour)}
	metric := domain.MetricDefinition{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking}
	best, _, err := svc.bestPerAgent(context.Background(), pe, metric)
	if err != nil {
		t.Fatalf("bestPerAgent err = %v, want nil", err)
	}
	standings, err := svc.rank(context.Background(), pe, metric, best)
	if err != nil {
		t.Fatalf("rank err = %v, want nil", err)
	}
	if len(standings) != 2 {
		t.Fatalf("standings = %v, want the 2 competitors only", standings)
	}
	if got, want := standings[0].AgentID, "c1"; got != want {
		t.Errorf("rank 1 = %v, want %v", got, want)
	}
	if st := standings[0]; st.CodeRef != "repo@job-c1" || st.HypothesisID != "hyp-job-c1" || st.Objective || st.Env["LR"] != "0.1" {
		t.Errorf("rank 1 = %+v, want the code ref, hypothesis and env of job-c1", st)
	}
	// It was shown the platform experiment's id as the hypothesis it tested.
	if st := standings[1]; st.HypothesisID != "" || !st.Objective || st.CodeRef != "repo@job-c2" {
		t.Errorf("rank 2 = %+v, want job-c2 on the objective with no hypothesis id", st)
	}
	for _, st := range standings {
		if st.AgentID == "baseline" {
			t.Errorf("standings = %v, want no baseline entry", standings)
		}
	}
}

// The standings rank competitors on ranking metrics. A scoreboard also shows the attribute metrics
// and the baseline's numbers, and reads them here exactly, not off a downsampled history.
func TestResultsListEveryAgentsBestOnEveryDeclaredMetric(t *testing.T) {
	srv := bestPerAgentMetricServer(t, map[string]float64{"baseline": 99, "c1": 50, "c2": 40})
	defer srv.Close()
	store := &standingsStore{competitors: []string{"c1", "c2"}}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking},
		{Key: "loss", Direction: "minimize", Role: domain.MetricRoleAttribute},
	}}
	svc := newSignupTestService(t, store)
	svc.metricsDBURL = srv.URL

	results, err := svc.Results(context.Background(), "pe-1")
	if err != nil {
		t.Fatalf("Results err = %v, want nil", err)
	}
	if len(results.Metrics) != 1 || results.Metrics[0].Metric != "score" || len(results.Metrics[0].Standings) != 2 {
		t.Errorf("metrics = %+v, want standings of the 2 competitors on score only", results.Metrics)
	}
	if len(results.Bests) != 2 || results.Bests[0].Metric != "score" || results.Bests[1].Metric != "loss" {
		t.Fatalf("bests = %+v, want score then loss", results.Bests)
	}
	// The best job of c1 completed with no summary filed.
	want := []AgentBest{{"baseline", 99, "job-baseline", false}, {"c1", 50, "job-c1", true}, {"c2", 40, "job-c2", false}}
	for _, bests := range results.Bests {
		if fmt.Sprint(bests.Agents) != fmt.Sprint(want) {
			t.Errorf("bests on %s = %+v, want %+v", bests.Metric, bests.Agents, want)
		}
	}
}

// A reference is shown beside the standings with the machine each value was measured on, so a
// result compares with it on the same machine, and it ranks nobody. One nobody measured is listed
// with no values.
func TestResultsShowEachReferenceWithWhereItRanAndRankNoneOfIt(t *testing.T) {
	srv := bestPerAgentMetricServer(t, map[string]float64{"baseline": 99, "c1": 50})
	defer srv.Close()
	store := &standingsStore{competitors: []string{"c1"}}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking},
		{Key: "other_score", Direction: "maximize", Role: domain.MetricRoleReference},
	}}
	svc := newSignupTestService(t, store)
	svc.metricsDBURL = srv.URL

	results, err := svc.Results(context.Background(), "pe-1")
	if err != nil {
		t.Fatalf("Results err = %v, want nil", err)
	}
	if len(results.Metrics) != 1 || results.Metrics[0].Metric != "score" {
		t.Fatalf("metrics = %+v, want standings on score alone", results.Metrics)
	}
	if st := results.Metrics[0].Standings; len(st) != 1 || st[0].RanOn == nil || st[0].RanOn.Node != "node-a" {
		t.Errorf("standings = %+v, want c1 with the node its job ran on", st)
	}
	want := []ReferenceValue{{"baseline", 99, "job-baseline", nil}, {"c1", 50, "job-c1", &domain.RanOn{Node: "node-a"}}}
	if len(results.References) != 1 || results.References[0].Metric != "other_score" || !reflect.DeepEqual(results.References[0].Values, want) {
		t.Errorf("references = %+v, want other_score with %+v", results.References, want)
	}
}

func (f *standingsStore) DescribeJobs(_ context.Context, jobs []*domain.Experiment) error {
	for _, job := range jobs {
		job.SummaryDue = job.ID == "job-c1"
	}
	return nil
}

func (f *standingsStore) ListSignups(ctx context.Context, platformExpID string) ([]string, error) {
	return f.competitors, nil
}

// An agent whose only job breaks a constraint is not in the standings Results serves, so it must
// not be in the top 3 recorded at close either: that record is what earns the bonus in the next run.
func TestCloseDerivesTheTopResultsFromConstraintEligibleAgentsOnly(t *testing.T) {
	bound := 60.0
	// Every metric answers the same values, so c1 leads on score and breaks the bound on memory.
	srv := bestPerAgentMetricServer(t, map[string]float64{"c1": 90, "c2": 50, "c3": 40})
	defer srv.Close()
	store := &standingsStore{competitors: []string{"c1", "c2", "c3"}}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking},
		{Key: "memory", Direction: "minimize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}
	svc := newSignupTestService(t, store)
	svc.metricsDBURL = srv.URL

	top, err := svc.derivedTopResults(context.Background(), "pe-1")
	if err != nil {
		t.Fatalf("derivedTopResults err = %v, want nil", err)
	}
	if len(top) != 2 || top[0].AgentID != "c2" || top[1].AgentID != "c3" {
		t.Fatalf("top results = %+v, want c2 then c3: c1 breaks the memory constraint", top)
	}
}

// bestPerAgentMetricServer answers BestPerAgentOnMetric's single PromQL query with one sample per
// agent.
func bestPerAgentMetricServer(t *testing.T, values map[string]float64) *httptest.Server {
	t.Helper()
	results := make([]string, 0, len(values))
	for agentID, v := range values {
		results = append(results, fmt.Sprintf(
			`{"metric":{"agent_id":%q,"metric_basis":"raw","job_id":"job-%s"},"value":[1,"%v"]}`, agentID, agentID, v))
	}
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(results, ","))
	// Where a job ran: job-c1 on node-a, every other job nowhere recorded.
	nodes := `{"output":[{"records":{"schema":{"column_schemas":[{"name":"experiment_id"},{"name":"node"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[["job-c1","node-a",1700000000000,1]]}}]}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/sql" {
			_, _ = w.Write([]byte(nodes))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

// A member signing up again has succeeded: it is in the experiment, which is all it asked for.
// The harness repeats signup on every restart, by which time the run has usually started and
// filled up, so neither may turn the repeat into an error.
func TestARepeatSignupSucceedsWhateverTheExperimentsStatusOrSize(t *testing.T) {
	for _, pe := range []*domain.PlatformExperiment{
		{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 5},
		{ID: "pe-1", Status: domain.PlatformExpRunning, MaxAgents: 5},
		{ID: "pe-1", Status: domain.PlatformExpClosed, MaxAgents: 5},
		{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 1},
	} {
		store := &signupStore{pe: pe, byRole: map[domain.SignupRole]int{domain.SignupRoleCompetitor: 1}, alreadySignedUp: true}
		if err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-1", Role: domain.SignupRoleCompetitor}); err != nil {
			t.Fatalf("%s, max_agents %d: repeat signup err = %v, want nil", pe.Status, pe.MaxAgents, err)
		}
		if store.insertCalled {
			t.Fatalf("%s: a repeat signup inserted again", pe.Status)
		}
	}
}

// Two signups of the same agent racing: the loser's guarded insert is refused, and the read after
// it finds the agent in, so the loser succeeds too.
func TestASignupThatLostTheRaceWithItsOwnTwinSucceeds(t *testing.T) {
	store := &signupStore{
		pe:           &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 5},
		byRole:       map[domain.SignupRole]int{domain.SignupRoleCompetitor: 1},
		refuseInsert: true,
		racedIn:      true,
	}
	if err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-1", Role: domain.SignupRoleCompetitor}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// A newcomer still meets the closed and full checks.
func TestANewcomerIsRefusedOnceTheExperimentStarted(t *testing.T) {
	store := &signupStore{pe: &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, MaxAgents: 5}}
	err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-2", Role: domain.SignupRoleCompetitor})
	if err == nil || !strings.Contains(err.Error(), "signup_closed") {
		t.Fatalf("err = %v, want signup_closed", err)
	}
}

// The other cause of the same refusal: the experiment started between the status read and the
// insert. This agent is genuinely not in it, and saying nothing would leave a caller believing it
// had joined a run it will never receive quota in.
func TestASignupThatLostTheRaceWithStartIsReportedClosed(t *testing.T) {
	store := &signupStore{
		pe:              &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 5},
		byRole:          map[domain.SignupRole]int{domain.SignupRoleCompetitor: 1},
		refuseInsert:    true,
		alreadySignedUp: false,
	}

	err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-1", Role: domain.SignupRoleCompetitor})
	if err == nil || !strings.Contains(err.Error(), "signup_closed") {
		t.Fatalf("err = %v, want signup_closed: this agent is not in the experiment and must not be told it is", err)
	}
}
