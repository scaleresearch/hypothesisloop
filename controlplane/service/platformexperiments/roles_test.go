package platformexperiments

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

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// signupStore fakes only the reads and the write Signup makes; every other
// Store method is inherited from the embedded nil interface, so a test that
// reaches one panics loudly instead of quietly getting a zero value back.
type signupStore struct {
	fakeStore
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

func newSignupTestService(t *testing.T, store Store) *Service {
	t.Helper()
	return reaching(NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), 3*time.Minute), domain.CPUType)
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
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.Reason != "max_agents_reached" || !strings.Contains(refused.Message, "executors (agents of role competitor)") || !strings.Contains(refused.Message, "researchers, agents of role baseline") {
		t.Fatalf("err = %v, want max_agents_reached saying that executors count and researchers and a baseline agent do not", err)
	}
}

// A researcher steers the field and is no part of it: max_agents full of executors leaves it room.
func TestAResearcherJoinsAFieldFullOfExecutors(t *testing.T) {
	store := &signupStore{
		pe:     &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 2},
		byRole: map[domain.SignupRole]int{domain.SignupRoleCompetitor: 2},
	}
	if err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", domain.AgentSignup{AgentID: "agent-r", Role: domain.SignupRoleResearcher}); err != nil || store.insertedFor != domain.SignupRoleResearcher {
		t.Fatalf("researcher signup: err = %v, inserted %s, want it signed up", err, store.insertedFor)
	}
}

// quotaStore answers an agent with no quota, of role.
type quotaStore struct {
	signupStore
	role domain.SignupRole
}

func (f *quotaStore) GetAgentQuota(context.Context, string, string) (*domain.AgentQuota, error) {
	return nil, nil
}

func (f *quotaStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	return &domain.AgentSignup{AgentID: agentID, Role: f.role}, nil
}

// A researcher submits no job, so it holds no quota, and its read of one says so as a planner's does.
func TestAResearcherHasNoQuota(t *testing.T) {
	store := &quotaStore{role: domain.SignupRoleResearcher}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning}
	_, err := newSignupTestService(t, store).AgentQuota(context.Background(), "agent-r", "pe-1", auth.Principal{})
	var refused *refusal.Refusal
	if !errors.As(err, &refused) || refused.GetStatus() != http.StatusNotFound || refused.Reason != platformapi.ReasonResearcherHasNoQuota {
		t.Fatalf("err = %v, want 404 researcher_has_no_quota", err)
	}
}

// An unrecognized role is rejected rather than defaulted: silently reading a typo as "competitor"
// would rank, cut and pay a top-3 bonus to an agent nobody meant to enter in the competition.
func TestUnknownSignupRoleIsRejectedRatherThanDefaulted(t *testing.T) {
	if _, err := domain.ParseSignupRole("baselineee"); err == nil {
		t.Fatalf("err = %v, want an error for an unrecognized role", err)
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
	svc.metrics = metricsdb.New(srv.URL)

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
	svc.metrics = metricsdb.New(srv.URL)

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
	want := []AgentBest{{"baseline", 99, "job-baseline"}, {"c1", 50, "job-c1"}, {"c2", 40, "job-c2"}}
	for _, bests := range results.Bests {
		if fmt.Sprint(bests.Agents) != fmt.Sprint(want) {
			t.Errorf("bests on %s = %+v, want %+v", bests.Metric, bests.Agents, want)
		}
	}
}

// The standings are set against the declared baseline: each ranking metric says its value and
// each standing whether it beats it. Raised to a stronger job, the bar moves with it.
func TestResultsSetEachStandingAgainstTheDeclaredBaseline(t *testing.T) {
	srv := bestPerAgentMetricServer(t, map[string]float64{"baseline": 45, "raised": 55, "c1": 50, "c2": 40})
	defer srv.Close()
	store := &standingsStore{competitors: []string{"c1", "c2"}}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour), BaselineJobID: "job-baseline", Metrics: []domain.MetricDefinition{
		{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking},
	}}
	svc := newSignupTestService(t, store)
	svc.metrics = metricsdb.New(srv.URL)
	beats := func() string {
		results, err := svc.Results(context.Background(), "pe-1")
		if err != nil {
			t.Fatalf("Results err = %v, want nil", err)
		}
		m := results.Metrics[0]
		out := fmt.Sprintf("%s %v:", m.BaselineJobID, *m.Baseline)
		for _, st := range m.Standings {
			out += fmt.Sprintf(" %s=%v", st.AgentID, *st.BeatsBaseline)
		}
		return out
	}
	if got, want := beats(), "job-baseline 45: c1=true c2=false"; got != want {
		t.Errorf("against the declared baseline: got %q, want %q", got, want)
	}
	store.pe.BaselineJobID = "job-raised"
	if got, want := beats(), "job-raised 55: c1=false c2=false"; got != want {
		t.Errorf("against the raised baseline: got %q, want %q", got, want)
	}
}

// A reference is shown beside the standings with the machine each value was measured on, so a
// result compares with it on the same machine, and it ranks nobody. Every completed job that
// reported it is listed with whether it met the constraints, and only one that met them counts
// toward best: a reference measured without the goal's constraint was invisible, and so read as
// never measured.
func TestResultsShowEachReferenceWithWhereItRanAndRankNoneOfIt(t *testing.T) {
	vector := func(values map[string]float64) string {
		var samples []string
		for agentID, v := range values {
			samples = append(samples, fmt.Sprintf(`{"metric":{"agent_id":%q,"metric_basis":"raw","job_id":"job-%s"},"value":[1,"%v"]}`, agentID, agentID, v))
		}
		return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		switch {
		case r.URL.Path == "/v1/sql":
			_, _ = w.Write([]byte(`{"output":[{"records":{"schema":{"column_schemas":[{"name":"experiment_id"},{"name":"node"},{"name":"greptime_timestamp"},{"name":"greptime_value"}]},"rows":[["job-c1","node-a",1700000000000,1]]}}]}`))
		case strings.Contains(r.Form.Get("query"), `metric_name="gate"`):
			// c1 broke the gate and c2 never reported it.
			_, _ = w.Write([]byte(vector(map[string]float64{"baseline": 1, "c1": 0, "c3": 1})))
		default:
			_, _ = w.Write([]byte(vector(map[string]float64{"baseline": 99, "c1": 120, "c2": 130, "c3": 50})))
		}
	}))
	defer srv.Close()
	bound := 1.0
	store := &standingsStore{competitors: []string{"c1", "c2", "c3"}}
	store.pe = &domain.PlatformExperiment{ID: "pe-1", CreatedAt: time.Now().UTC().Add(-time.Hour), Metrics: []domain.MetricDefinition{
		{Key: "score", Direction: "maximize", Role: domain.MetricRoleRanking},
		{Key: "other_score", Direction: "maximize", Role: domain.MetricRoleReference},
		{Key: "gate", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound},
	}}
	svc := newSignupTestService(t, store)
	svc.metrics = metricsdb.New(srv.URL)

	results, err := svc.Results(context.Background(), "pe-1")
	if err != nil {
		t.Fatalf("Results err = %v, want nil", err)
	}
	if len(results.Metrics) != 1 || results.Metrics[0].Metric != "score" {
		t.Fatalf("metrics = %+v, want standings on score alone", results.Metrics)
	}
	if st := results.Metrics[0].Standings; len(st) != 1 || st[0].AgentID != "c3" {
		t.Errorf("standings = %+v, want c3 alone, the one competitor that held the gate", st)
	}
	want := []ReferenceValue{
		{"baseline", 99, "job-baseline", nil, "true"},
		{"c1", 120, "job-c1", &domain.RanOn{Node: "node-a"}, "false"},
		{"c2", 130, "job-c2", nil, "unknown"},
		{"c3", 50, "job-c3", nil, "true"},
	}
	if len(results.References) != 1 || results.References[0].Metric != "other_score" || !reflect.DeepEqual(results.References[0].Values, want) {
		t.Errorf("references = %+v, want other_score with %+v", results.References, want)
	}
	// The best that met the gate is 99 and the leader is c3 at 50: the goal is not met.
	if r := results.References[0]; r.Best == nil || *r.Best != 99 || r.LeaderMetric != "score" || r.Leader == nil || *r.Leader != 50 || r.Beaten == nil || *r.Beaten {
		t.Errorf("reference = %+v, want best 99, leader 50 on score, not beaten", r)
	}
}

// Each reference is set against the leader only where the comparison means something: the
// same direction, and both measured.
func TestAReferenceIsBeatenOnlyByALeaderStrictlyBetterInItsDirection(t *testing.T) {
	ranked := func(direction string, best ...float64) MetricStandings {
		m := MetricStandings{Metric: "score", Direction: direction}
		for _, b := range best {
			m.Standings = append(m.Standings, AgentStanding{Best: b})
		}
		return m
	}
	reference := func(direction string, values ...float64) MetricReference {
		r := MetricReference{Metric: "ref", Direction: direction}
		for _, v := range values {
			r.Values = append(r.Values, ReferenceValue{Value: v, ConstraintsMet: metricsdb.ConstraintsHeld})
		}
		return r
	}
	for _, c := range []struct {
		name      string
		metrics   []MetricStandings
		reference MetricReference
		best      string
		leader    string
		beaten    string
	}{
		{"leader above", []MetricStandings{ranked("maximize", 5, 2)}, reference("maximize", 3, 4), "4", "5", "true"},
		{"tie", []MetricStandings{ranked("maximize", 4)}, reference("maximize", 4), "4", "4", "false"},
		{"minimize", []MetricStandings{ranked("minimize", 2)}, reference("minimize", 3, 1), "1", "2", "false"},
		{"other direction", []MetricStandings{ranked("minimize", 2)}, reference("maximize", 3), "3", "-", "-"},
		{"nobody ranks", []MetricStandings{ranked("maximize")}, reference("maximize", 3), "3", "-", "-"},
		{"not measured", []MetricStandings{ranked("maximize", 5)}, reference("maximize"), "-", "-", "-"},
		{"no ranking metric", nil, reference("maximize", 3), "3", "-", "-"},
		{"constraints not met", []MetricStandings{ranked("maximize", 5)}, MetricReference{Metric: "ref", Direction: "maximize", Values: []ReferenceValue{
			{Value: 9, ConstraintsMet: metricsdb.ConstraintsBroken}, {Value: 8, ConstraintsMet: metricsdb.ConstraintsUnknown}}}, "-", "-", "-"},
	} {
		out := &PlatformExperimentResults{Metrics: c.metrics, References: []MetricReference{c.reference}}
		setAgainstLeader(out)
		r := out.References[0]
		show := func(v any) string {
			switch p := v.(type) {
			case *float64:
				if p != nil {
					return fmt.Sprint(*p)
				}
			case *bool:
				if p != nil {
					return fmt.Sprint(*p)
				}
			}
			return "-"
		}
		if got := []string{show(r.Best), show(r.Leader), show(r.Beaten)}; !reflect.DeepEqual(got, []string{c.best, c.leader, c.beaten}) {
			t.Errorf("%s: best, leader, beaten = %v, want %v", c.name, got, []string{c.best, c.leader, c.beaten})
		}
	}
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
	svc.metrics = metricsdb.New(srv.URL)

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

type priorRunsStore struct {
	fakeStore
	runs []db.RunToRank
}

func (s *priorRunsStore) RunsToRank(context.Context, []string) ([]db.RunToRank, error) {
	return s.runs, nil
}

// Each prior run's best is its leading executor's, as results ranks it, and every prior run is
// read together, by one query of the metrics store for their bests and one for their constraints
// when any declares one.
func TestPriorRunsAreRankedAsResultsRanksThemAndReadTogether(t *testing.T) {
	queries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"platform_experiment_id":"pe-a","metric_name":"score","agent_id":"baseline","metric_basis":"raw","job_id":"job-base"},"value":[1,"99"]},` +
			`{"metric":{"platform_experiment_id":"pe-a","metric_name":"score","agent_id":"c1","metric_basis":"raw","job_id":"job-c1"},"value":[1,"50"]},` +
			`{"metric":{"platform_experiment_id":"pe-b","metric_name":"loss","agent_id":"c2","metric_basis":"raw","job_id":"job-c2"},"value":[1,"3"]}` +
			`]}}`))
	}))
	defer srv.Close()
	created := time.Now().UTC().Add(-time.Hour)
	store := &priorRunsStore{runs: []db.RunToRank{
		{PE: &domain.PlatformExperiment{ID: "pe-a", Status: domain.PlatformExpClosed, CreatedAt: created, Metrics: []domain.MetricDefinition{{Key: "score", Direction: "maximize"}}},
			Completed: map[string]bool{"job-base": true, "job-c1": true}, Competitors: map[string]bool{"c1": true}},
		{PE: &domain.PlatformExperiment{ID: "pe-b", Status: domain.PlatformExpClosed, CreatedAt: created, Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}},
			Completed: map[string]bool{"job-c2": true}},
		{PE: &domain.PlatformExperiment{ID: "pe-c", Status: domain.PlatformExpClosed, CreatedAt: created}},
	}}
	svc := newSignupTestService(t, store)
	svc.metrics = metricsdb.New(srv.URL)

	got, err := svc.PriorRuns(context.Background(), []string{"pe-a", "pe-b", "pe-c"})
	if err != nil || len(got) != 3 {
		t.Fatalf("prior runs: got = %+v, %v, want three", got, err)
	}
	if got[0].Metric != "score" || got[0].Best == nil || *got[0].Best != 50 || got[0].BestJobID != "job-c1" {
		t.Errorf("pe-a = %+v, want c1's 50 on score: the baseline agent ranks nowhere", got[0])
	}
	if got[1].Metric != "loss" || got[1].Best != nil {
		t.Errorf("pe-b = %+v, want no best: nobody who reported it is an executor", got[1])
	}
	if got[2].Metric != "" || got[2].Best != nil {
		t.Errorf("pe-c = %+v, want neither metric nor best", got[2])
	}
	if queries != 1 {
		t.Errorf("%d queries of the metrics store, want 1 with no constraint declared", queries)
	}
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
	if refusal.ReasonOf(err) != "signup_closed" {
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
	if refusal.ReasonOf(err) != "signup_closed" {
		t.Fatalf("err = %v, want signup_closed: this agent is not in the experiment and must not be told it is", err)
	}
}
