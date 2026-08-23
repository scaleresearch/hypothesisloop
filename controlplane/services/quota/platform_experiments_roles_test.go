package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

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
	// alreadySignedUp is what the follow-up read finds, and the only thing that tells those two
	// cases apart.
	alreadySignedUp bool
}

func (f *signupStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (f *signupStore) CountSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) (int, error) {
	return f.byRole[role], nil
}

func (f *signupStore) Signup(ctx context.Context, platformExpID, agentID string, role domain.SignupRole, quotaTier domain.QuotaTier) (bool, error) {
	f.insertedFor = role
	return !f.refuseInsert, nil
}

func (f *signupStore) IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error) {
	return f.alreadySignedUp, nil
}

func newSignupTestService(t *testing.T, store PlatformExperimentsStore) *PlatformExperimentsService {
	t.Helper()
	return NewPlatformExperimentsService(store, domain.QuotaConfig{}, zap.NewNop(), "", 3*time.Minute)
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

	if err := svc.Signup(context.Background(), "pe-1", "agent-baseline", domain.SignupRoleBaseline, ""); err != nil {
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

	err := svc.Signup(context.Background(), "pe-1", "agent-3", domain.SignupRoleCompetitor, "")
	if err == nil || !strings.Contains(err.Error(), "max_agents_reached") {
		t.Fatalf("err = %v, want max_agents_reached", err)
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

// An unrecognized submitter policy is rejected rather than defaulted: silently reading a typo as
// "mixed" would open a restricted platform experiment to submitters the operator meant to
// exclude.
func TestUnknownSubmitterPolicyIsRejectedRatherThanDefaulted(t *testing.T) {
	if _, err := domain.ParseSubmitterPolicy("human_onyl"); err == nil {
		t.Fatalf("err = nil, want an error for an unrecognized submitter policy")
	}
}

// An absent policy means every caller written before this field existed keeps meaning what it
// meant: both humans and agents may submit.
func TestAbsentSubmitterPolicyResolvesToMixed(t *testing.T) {
	policy, err := domain.ParseSubmitterPolicy("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got, want := policy, domain.SubmitterPolicyMixed; got != want {
		t.Errorf("policy = %v, want %v", got, want)
	}
	if !policy.AllowsHuman() || !policy.AllowsAgent() {
		t.Errorf("mixed policy AllowsHuman()=%v AllowsAgent()=%v, want both true", policy.AllowsHuman(), policy.AllowsAgent())
	}
}

func TestSubmitterPolicyHumanOnlyRejectsAgents(t *testing.T) {
	policy, err := domain.ParseSubmitterPolicy("human_only")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !policy.AllowsHuman() {
		t.Errorf("human_only AllowsHuman() = false, want true")
	}
	if policy.AllowsAgent() {
		t.Errorf("human_only AllowsAgent() = true, want false")
	}
}

func TestSubmitterPolicyAgentOnlyRejectsHumans(t *testing.T) {
	policy, err := domain.ParseSubmitterPolicy("agent_only")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if policy.AllowsHuman() {
		t.Errorf("agent_only AllowsHuman() = true, want false")
	}
	if !policy.AllowsAgent() {
		t.Errorf("agent_only AllowsAgent() = false, want true")
	}
}

// standingsStore adds the two reads standingsOnMetric makes on top of signupStore's.
type standingsStore struct {
	signupStore
	competitors []string
}

func (f *standingsStore) ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error) {
	return f.competitors, nil
}

func (f *standingsStore) GetExperiment(ctx context.Context, id string) (*domain.Experiment, error) {
	return &domain.Experiment{ID: id, CodeRef: "repo@" + id}, nil
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
	standings, _, err := svc.standingsOnMetric(context.Background(), pe, metric)
	if err != nil {
		t.Fatalf("standingsOnMetric err = %v, want nil", err)
	}
	if len(standings) != 2 {
		t.Fatalf("standings = %v, want the 2 competitors only", standings)
	}
	if got, want := standings[0].AgentID, "c1"; got != want {
		t.Errorf("rank 1 = %v, want %v", got, want)
	}
	for _, st := range standings {
		if st.AgentID == "baseline" {
			t.Errorf("standings = %v, want no baseline entry", standings)
		}
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
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// The guarded insert reports the same inserted=false for two different situations, and the caller
// gets a different answer for each. An agent signing up twice has succeeded — it is in the
// experiment, which is all it asked for — and returning an error there would fail a retry of a
// request that already worked, the exact thing an idempotent signup exists to prevent.
func TestARepeatSignupSucceedsRatherThanReportingTheExperimentClosed(t *testing.T) {
	store := &signupStore{
		pe:              &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, MaxAgents: 5},
		byRole:          map[domain.SignupRole]int{domain.SignupRoleCompetitor: 1},
		refuseInsert:    true,
		alreadySignedUp: true,
	}

	if err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", "agent-1", domain.SignupRoleCompetitor, ""); err != nil {
		t.Fatalf("repeat signup err = %v, want nil — the agent is already in the experiment, which is what it asked for", err)
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

	err := newSignupTestService(t, store).Signup(context.Background(), "pe-1", "agent-1", domain.SignupRoleCompetitor, "")
	if err == nil || !strings.Contains(err.Error(), "signup_closed") {
		t.Fatalf("err = %v, want signup_closed: this agent is not in the experiment and must not be told it is", err)
	}
}
