package platformexperiments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// rosterStore is a platform experiment's roster in memory, with the store's own refusals.
type rosterStore struct {
	fakeStore
	pe      *domain.PlatformExperiment
	agents  map[string]*domain.Agent
	signups []domain.AgentSignup
	// unregistered is a launcher config names and no operator registered.
	unregistered bool
	// left is the COMPLETED jobs on the objective its baseline agents that finished left.
	left []*domain.Experiment
	// objective is every job on the objective, oldest first.
	objective []*domain.Experiment
	// completed is every COMPLETED job, oldest first.
	completed []*domain.Experiment
	// inFlight is the agents that have a job queued or running.
	inFlight map[string]bool
	// lateShare is what the researchers that ended left unspent, for one joining the run.
	lateShare float64
	// submitted is how many jobs each agent submitted.
	submitted map[string]int
	// hypotheses is the graph, and graphErr what reading it fails with.
	hypotheses []*domain.Hypothesis
	graphErr   error
	// quota is every agent's allocation, nil for none.
	quota *domain.AgentQuota
}

// MessageAllowance answers every agent the whole hour's messages.
func (s *rosterStore) MessageAllowance(context.Context, string, domain.Actor) (map[string]domain.MessageAllowance, error) {
	out := map[string]domain.MessageAllowance{}
	for _, signup := range s.signups {
		out[signup.AgentID] = domain.MessageAllowance{LeftThisHour: domain.MaxAgentMessagesPerHour}
	}
	return out, nil
}

func (s *rosterStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	jobs := map[string]db.AgentJobs{}
	for agentID, submitted := range s.submitted {
		jobs[agentID] = db.AgentJobs{Submitted: submitted}
	}
	return jobs, nil
}

// ListHypotheses answers as the graph does: the hypotheses the filter keeps, in the order given.
func (f *rosterStore) ListHypotheses(_ context.Context, filter graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.graphErr != nil {
		return nil, f.graphErr
	}
	var out []*domain.HypothesisListItem
	for _, h := range f.hypotheses {
		agent := filter.AgentID == "" || h.AgentID == filter.AgentID
		listed := filter.IDs == nil || slices.Contains(filter.IDs, h.ID)
		if h.PlatformExperimentID == filter.PlatformExperimentID && agent && listed && h.Status == filter.Status && len(out) < filter.Limit {
			out = append(out, &domain.HypothesisListItem{Hypothesis: h})
		}
	}
	return out, nil
}

func (f *rosterStore) ListExperimentsTesting(context.Context, []string) ([]*domain.Experiment, error) {
	return f.objective, nil
}

func (f *rosterStore) FinishedBaselineJobs(context.Context, string) ([]*domain.Experiment, error) {
	return f.left, nil
}

func (f *rosterStore) CompletedJobs(context.Context, string) ([]*domain.Experiment, error) {
	return f.completed, nil
}

func (f *rosterStore) Reachable(_ context.Context, _, repository string) (string, error) {
	return repository, nil
}

func (f *rosterStore) URL(string) string { return "" }

func (f *rosterStore) File(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (f *rosterStore) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	return nil, nil
}

func (f *rosterStore) Serves(string, string, string, string) bool { return false }

func (f *rosterStore) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, githost.ErrFileNotFound
}

func (f *rosterStore) Create(context.Context, string, string, string, string, map[string]string) (string, error) {
	return "", nil
}

func (f *rosterStore) CreatesOn(context.Context, string, string) (string, error) { return "", nil }

const rosterMaxHosted = 6

func (f *rosterStore) ListSignups(context.Context, string) ([]string, error) {
	ids := make([]string, 0, len(f.signups))
	for _, s := range f.signups {
		ids = append(ids, s.AgentID)
	}
	return ids, nil
}

// machines is what the platform rents for the experiment at once, with more of them added.
func (f *rosterStore) machines(more ...domain.AgentSignup) int {
	measuring, others := f.rented(more...)
	return measuring + others
}

// rented counts the machines as the store does: an agent that finished holds none, and those
// that measure the baseline are counted apart from the coordinator and the other agents.
func (f *rosterStore) rented(more ...domain.AgentSignup) (measuring, others int) {
	if f.pe.CoordinatorHosted {
		others++
	}
	for _, s := range append(append([]domain.AgentSignup{}, f.signups...), more...) {
		switch {
		case !s.Hosted || s.FinishedAt != nil || s.Instance != nil && s.Instance.Stopped:
		case s.Role == domain.SignupRoleBaseline:
			measuring++
		default:
			others++
		}
	}
	return measuring, others
}

// full says more would be a machine too many, or would take the one kept for the baseline.
func (f *rosterStore) full(more domain.AgentSignup) bool {
	measuring, others := f.rented(more)
	return more.Hosted && (others > f.pe.MaxHosted-1 || measuring+others > f.pe.MaxHosted)
}

func newRosterStore(status domain.PlatformExperimentStatus, maxAgents int) *rosterStore {
	return &rosterStore{
		pe: &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "project-a", Status: status, MaxAgents: maxAgents, MaxHosted: rosterMaxHosted, Repository: "acme/model",
		},
		agents: map[string]*domain.Agent{},
	}
}

func (f *rosterStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	if id != f.pe.ID {
		return nil, nil
	}
	return f.pe, nil
}

func (f *rosterStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return f.agents[id], nil
}

func (f *rosterStore) competitors() int {
	n := 0
	for _, s := range f.signups {
		if s.Role == domain.SignupRoleCompetitor && s.FinishedAt == nil && (s.Instance == nil || !s.Instance.Stopped) {
			n++
		}
	}
	return n
}

func (f *rosterStore) IsSignedUp(_ context.Context, _, agentID string) (bool, error) {
	signup, _ := f.GetAgentSignup(context.Background(), f.pe.ID, agentID)
	return signup != nil, nil
}

func (f *rosterStore) Signup(_ context.Context, _ string, signup domain.AgentSignup) (bool, error) {
	if f.pe.Status != domain.PlatformExpOpen {
		return false, nil
	}
	if f.agents[signup.AgentID] == nil || f.agents[signup.AgentID].OrgID != "org-a" {
		return false, db.ErrAgentOutsideOrg
	}
	if signup.Role == domain.SignupRoleCompetitor && f.competitors() >= f.pe.MaxAgents {
		return false, db.ErrMaxAgentsReached
	}
	if f.full(signup) {
		return false, db.ErrMaxHostedReached
	}
	signup.PlatformExperimentID, signup.Name = f.pe.ID, f.agents[signup.AgentID].Name
	f.signups = append(f.signups, signup)
	return true, nil
}

func (f *rosterStore) SpawnAgent(_ context.Context, _ string, agent *domain.Agent, signup domain.AgentSignup) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	if signup.Role == domain.SignupRoleCompetitor && f.competitors() >= f.pe.MaxAgents {
		return db.ErrMaxAgentsReached
	}
	if f.full(signup) {
		return db.ErrMaxHostedReached
	}
	taken := func(name string) bool {
		for _, s := range f.signups {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	if agent.Name == "" {
		agent.Name = domain.AgentName(signup.Role, taken)
	} else if taken(agent.Name) {
		return db.ErrAgentNameTaken
	}
	agent.OrgID = "org-a"
	f.agents[agent.ID] = agent
	signup.PlatformExperimentID, signup.Name = f.pe.ID, agent.Name
	f.signups = append(f.signups, signup)
	return nil
}

func (f *rosterStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	var ids []string
	for _, s := range f.signups {
		if s.Role == role {
			ids = append(ids, s.AgentID)
		}
	}
	return ids, nil
}

// JoinRunning joins as an open experiment would, while the researchers that ended left
// lateShare unspent.
func (f *rosterStore) JoinRunning(ctx context.Context, id string, agent *domain.Agent, signup domain.AgentSignup, _ db.LateJoin) error {
	if f.pe.Status != domain.PlatformExpRunning || signup.Role != domain.SignupRoleCompetitor {
		return db.ErrSignupClosed
	}
	if f.lateShare <= 0 {
		return db.ErrNoShareLeft
	}
	f.pe.Status = domain.PlatformExpOpen
	defer func() { f.pe.Status = domain.PlatformExpRunning }()
	if agent != nil {
		return f.SpawnAgent(ctx, id, agent, signup)
	}
	_, err := f.Signup(ctx, id, signup)
	return err
}

func (f *rosterStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	return append([]domain.AgentSignup{}, f.signups...), nil
}

func (f *rosterStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	for _, s := range f.signups {
		if s.AgentID == agentID {
			return &s, nil
		}
	}
	return nil, nil
}

func (f *rosterStore) NameActors(context.Context, []*domain.Actor) error { return nil }

func (f *rosterStore) RemoveAgent(_ context.Context, _, agentID string, _ func(context.Context, string) error) error {
	if f.pe.Status != domain.PlatformExpOpen {
		return db.ErrSignupClosed
	}
	for i, s := range f.signups {
		if s.AgentID == agentID {
			if f.inFlight[agentID] {
				return db.ErrWorkUnfinished
			}
			f.signups = append(f.signups[:i], f.signups[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotSignedUp
}

var agentsTestPlacement = authtest.Placement{
	PlatformExperiments: map[string]domain.Tenancy{
		"pe-1": {PlatformExperimentID: "pe-1", ProjectID: "project-a", OrgID: "org-a"},
	},
	Agents: map[string]string{"veteran": "org-a", "dana": "org-a", "outsider": "org-b"},
}

func call(t *testing.T, r http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(req))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body %q is not JSON: %v", method, path, rec.Body, err)
		}
	}
	return rec.Code, out
}

func joined(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	list, ok := body["agents"].([]any)
	if !ok {
		t.Fatalf("answer has no agents list: %v", body)
	}
	out := make([]map[string]any, len(list))
	for i, a := range list {
		out[i] = a.(map[string]any)
	}
	return out
}

var staff = auth.Principal{Kind: auth.KindStaff}

// FinishAgent refuses as the store does: an agent finishes in a running platform experiment, and
// one of role baseline also in an open one.
func (f *rosterStore) FinishAgent(_ context.Context, _, agentID, reason string) error {
	for i, signup := range f.signups {
		if signup.AgentID != agentID {
			continue
		}
		if f.pe.Status == domain.PlatformExpRunning || f.pe.Status == domain.PlatformExpOpen && signup.Role == domain.SignupRoleBaseline {
			now := time.Now()
			f.signups[i].FinishedAt, f.signups[i].FinishedReason = &now, reason
			return nil
		}
		return db.ErrSignupClosed
	}
	return domain.ErrNotSignedUp
}

func (f *rosterStore) AddDesiredQuotaUsageOne(context.Context, *domain.AgentQuota) error { return nil }

func (f *rosterStore) IsAgentCut(context.Context, string, string) (bool, error) { return false, nil }

func (f *rosterStore) GetAgentRunningExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return nil, nil
}
