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
}

func (f *signupStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (f *signupStore) CountSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) (int, error) {
	return f.byRole[role], nil
}

func (f *signupStore) Signup(ctx context.Context, platformExpID, agentID string, role domain.SignupRole) (bool, error) {
	f.insertedFor = role
	return true, nil
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

	if err := svc.Signup(context.Background(), "pe-1", "agent-baseline", domain.SignupRoleBaseline); err != nil {
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

	err := svc.Signup(context.Background(), "pe-1", "agent-3", domain.SignupRoleCompetitor)
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
