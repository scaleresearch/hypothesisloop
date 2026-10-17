package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// poolStore is the Postgres side of the registry reduced to what hypothesis writes consult: every
// platform experiment exists with a mixed policy, every agent exists, and a comment is accepted.
// It embeds Store so any method a test did not think about panics instead of quietly answering zero.
type poolStore struct {
	registry.Store
	// jobs is every job on record, and comments every note written, in order.
	jobs     []*domain.Experiment
	comments []string
	// commentActors is whose credential wrote each of comments, and actorNames the name kept
	// for an actor, by its id.
	commentActors []domain.Actor
	actorNames    map[string]string
	commentRows   []*domain.HypothesisComment
	// failNextComment fails the next comment insert, once.
	failNextComment bool
	// closed holds the platform experiments that are closed.
	closed map[string]bool
	// metrics is what every platform experiment declares.
	metrics []domain.MetricDefinition
}

func (s *poolStore) GetPlatformExperimentMetrics(context.Context, string) ([]domain.MetricDefinition, bool, error) {
	return s.metrics, true, nil
}

func (s *poolStore) ListExperimentsTesting(_ context.Context, ids []string) ([]*domain.Experiment, error) {
	var testing []*domain.Experiment
	for _, job := range s.jobs {
		if slices.Contains(ids, job.HypothesisID) {
			testing = append(testing, job)
		}
	}
	return testing, nil
}

func (s *poolStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	for _, job := range s.jobs {
		if job.ID == id {
			return job, nil
		}
	}
	return nil, nil
}

func newPoolStore() *poolStore { return &poolStore{} }

func (s *poolStore) PlatformExperimentClosed(_ context.Context, id string) (bool, error) {
	return s.closed[id], nil
}

// PlatformExperimentExists knows every platform experiment id but pe-none.
func (s *poolStore) PlatformExperimentExists(_ context.Context, id string) (bool, error) {
	return strings.HasPrefix(id, "pe-") && id != "pe-none", nil
}

func (s *poolStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return &domain.Agent{ID: id}, nil
}

// GetAgentSignup signs every agent up as a researcher, but the one named baseline-agent, and
// the one named agent-elsewhere not at all.
func (s *poolStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	if agentID == "agent-elsewhere" {
		return nil, nil
	}
	role := domain.SignupRoleCompetitor
	if agentID == "baseline-agent" {
		role = domain.SignupRoleBaseline
	}
	return &domain.AgentSignup{AgentID: agentID, Role: role}, nil
}

// CreateHypothesisComment mirrors the store insert, which consults nothing about the hypothesis
// beyond its id — a note is attached to a row, not authorised against its owner.
func (s *poolStore) CreateHypothesisComment(_ context.Context, hypothesisID, platformExperimentID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	if s.failNextComment {
		s.failNextComment = false
		return nil, errors.New("insert failed")
	}
	if s.closed[platformExperimentID] {
		return nil, fmt.Errorf("%w: %s", domain.ErrPlatformExperimentClosed, platformExperimentID)
	}
	s.comments = append(s.comments, text)
	s.commentActors = append(s.commentActors, actor)
	c := &domain.HypothesisComment{
		ID: "c-1", HypothesisID: hypothesisID, PlatformExperimentID: platformExperimentID,
		AgentID: agentID, Actor: actor, Text: text, CreatedAt: time.Now().UTC(),
	}
	s.commentRows = append(s.commentRows, c)
	return c, nil
}

func (s *poolStore) ListCommentsByHypothesis(_ context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error) {
	var of []*domain.HypothesisComment
	for _, c := range s.commentRows {
		if c.HypothesisID == hypothesisID {
			of = append(of, c)
		}
	}
	of = of[min(offset, len(of)):]
	return of[:min(limit, len(of))], nil
}

func (s *poolStore) CountCommentsByHypothesis(_ context.Context, hypothesisID string) (int, error) {
	n := 0
	for _, c := range s.commentRows {
		if c.HypothesisID == hypothesisID {
			n++
		}
	}
	return n, nil
}

// NameActors names an actor as the database does: by the name kept for it, else by its id, and a
// coordinator and a staff token by what they are.
func (s *poolStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, a := range actors {
		switch a.Kind {
		case domain.ActorCoordinator, domain.ActorStaff:
			a.Name = string(a.Kind)
		case domain.ActorUnverified:
		default:
			a.Name = a.ID
			if name, ok := s.actorNames[a.ID]; ok {
				a.Name = name
			}
		}
	}
	return nil
}

func (s *poolStore) DescribeJobs(context.Context, []*domain.Experiment) error { return nil }

func (s *poolStore) DeleteHypothesisComments(_ context.Context, hypothesisID string) error {
	s.commentRows = slices.DeleteFunc(s.commentRows, func(c *domain.HypothesisComment) bool { return c.HypothesisID == hypothesisID })
	return nil
}

func agentActor(id string) domain.Actor { return domain.Actor{Kind: domain.ActorAgent, ID: id} }

var adaActor = domain.Actor{Kind: domain.ActorUser, ID: "user-ada"}

// fakeGraph is the hypothesis graph reduced to the rules the registry relies on it for: dedup on
// (platform_experiment_id, normalized text), and the owner rule on status and links. Link
// validation itself is the graph store's and is tested against Memgraph in storage/graphdb.
type fakeGraph struct {
	registry.Graph
	rows  map[string]*domain.Hypothesis
	links map[string]bool
	// in is the links into each hypothesis, by its id.
	in    map[string][]domain.Link
	clock time.Time
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{rows: map[string]*domain.Hypothesis{}, links: map[string]bool{}, in: map[string][]domain.Link{},
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (g *fakeGraph) tick() time.Time {
	g.clock = g.clock.Add(time.Second)
	return g.clock
}

func (g *fakeGraph) Register(_ context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, []domain.SimilarHypothesis, error) {
	id := n.PlatformExperimentID + "\x00" + domain.NormalizeHypothesisText(n.Text)
	if existing, ok := g.rows[id]; ok {
		return existing, []domain.Link{}, true, nil, nil
	}
	h := &domain.Hypothesis{
		ID: id, AgentID: n.AgentID, Actor: n.Actor,
		PlatformExperimentID: n.PlatformExperimentID, Text: n.Text, Status: domain.HypothesisOpen, CreatedAt: g.tick(),
	}
	g.rows[id] = h
	return h, []domain.Link{}, false, nil, nil
}

func (g *fakeGraph) GetHypothesis(_ context.Context, id string) (*domain.Hypothesis, error) {
	return g.rows[id], nil
}

func (g *fakeGraph) UpdateStatus(_ context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[id]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.AgentID != "" && h.AgentID != callerAgentID {
		return nil, false, graphdb.ErrNotOwner
	}
	if h.Status == status {
		return h, false, nil
	}
	at := g.tick()
	h.Status, h.StatusChangedAt, h.StatusActor = status, &at, &actor
	return h, true, nil
}

func (g *fakeGraph) AddLink(_ context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec) (*graphdb.LinkResult, error) {
	h, ok := g.rows[fromID]
	if !ok {
		return nil, graphdb.ErrNotFound
	}
	if h.AgentID != "" && h.AgentID != agentID {
		return nil, graphdb.ErrNotOwner
	}
	key := fromID + string(spec.Type) + spec.To
	result := &graphdb.LinkResult{From: h, Created: !g.links[key], Later: true,
		Out: []domain.Link{{Type: spec.Type, HypothesisID: spec.To, AgentID: agentID, Actor: actor}}}
	g.links[key] = true
	if !result.Created {
		return result, nil
	}
	result.CreatedAt = g.tick()
	g.in[spec.To] = append(g.in[spec.To], domain.Link{Type: spec.Type, HypothesisID: fromID, AgentID: agentID, Actor: actor})
	return result, nil
}

func (g *fakeGraph) Links(_ context.Context, id string) (domain.HypothesisLinks, error) {
	return domain.HypothesisLinks{Out: []domain.Link{}, In: g.in[id]}, nil
}

// EditText and Remove hold the graph's own check: nothing may link to the hypothesis.
func (g *fakeGraph) EditText(_ context.Context, id, text string) (*domain.Hypothesis, error) {
	h, ok := g.rows[id]
	if !ok {
		return nil, graphdb.ErrNotFound
	}
	if in := g.in[id]; len(in) > 0 {
		return nil, &graphdb.LinkedError{In: in}
	}
	h.Text = text
	return h, nil
}

func (g *fakeGraph) Remove(_ context.Context, id string) error {
	if _, ok := g.rows[id]; !ok {
		return graphdb.ErrNotFound
	}
	if in := g.in[id]; len(in) > 0 {
		return &graphdb.LinkedError{In: in}
	}
	delete(g.rows, id)
	return nil
}

// recordedEvents is the change stream as the registry sees it: the changes of each write that
// committed, with ids 1, 2, ... as Postgres gives them.
type recordedEvents struct {
	events []domain.Event
	// failNext fails the next write after its graph write, as a Postgres commit failing after the
	// graph write committed.
	failNext bool
	// closed holds the platform experiments that are closed.
	closed map[string]bool
}

type pendingChanges struct {
	r       *recordedEvents
	changes []domain.Change
}

func (p *pendingChanges) Change(c domain.Change) { p.changes = append(p.changes, c) }

func (p *pendingChanges) ChangeUnlessAnnounced(_ context.Context, c domain.Change) error {
	for _, e := range slices.Backward(p.r.events) {
		if e.Kind == c.Kind && e.Subject == c.Subject && e.Detail == c.Detail && e.PlatformExperimentID == c.PlatformExperimentID {
			if e.Value == c.Value {
				return nil
			}
			break
		}
	}
	p.Change(c)
	return nil
}

func (r *recordedEvents) Around(_ context.Context, pe string, fn func(domain.Announcer) error) error {
	if r.closed[pe] {
		return fmt.Errorf("%w: %s", domain.ErrPlatformExperimentClosed, pe)
	}
	pending := &pendingChanges{r: r}
	if err := fn(pending); err != nil {
		return err
	}
	if r.failNext {
		r.failNext = false
		return errors.New("commit failed")
	}
	for _, c := range pending.changes {
		r.events = append(r.events, domain.Event{ID: int64(len(r.events) + 1), Change: c})
	}
	return nil
}

func poolService(store registry.Store) *registry.Service {
	return registry.New(store, newFakeGraph(), &recordedEvents{}, zap.NewNop(), nil, nil)
}

var buildOnRoot = []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-1"}}

type missingExperimentStore struct{ poolStore }

func (missingExperimentStore) PlatformExperimentExists(context.Context, string) (bool, error) {
	return false, nil
}

func (missingExperimentStore) GetPlatformExperimentMetrics(context.Context, string) ([]domain.MetricDefinition, bool, error) {
	return nil, false, nil
}

func TestHypothesisGraphOfAnUnknownPlatformExperimentIsNotFound(t *testing.T) {
	r := authtest.Router(authtest.Staff)
	registerRegistry(NewDoc(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop()), poolService(&missingExperimentStore{}), zap.NewNop())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/hypotheses/graph?platform_experiment_id=pe-missing", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func jobOn(id, hypothesisID string, status domain.ExperimentStatus) *domain.Experiment {
	return &domain.Experiment{ID: id, HypothesisID: hypothesisID, PlatformExperimentID: "pe-1", Status: status}
}

var watchTenancy = authtest.Placement{
	PlatformExperiments: map[string]domain.Tenancy{
		"pe-1":     {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a"},
		"pe-other": {PlatformExperimentID: "pe-other", ProjectID: "prj-b", OrgID: "org-b"},
	},
	Experiments: map[string]domain.Tenancy{
		"exp-1":     {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a", AgentID: "agent-1"},
		"exp-of-2":  {PlatformExperimentID: "pe-1", ProjectID: "prj-a", OrgID: "org-a", AgentID: "agent-2"},
		"exp-other": {PlatformExperimentID: "pe-other", ProjectID: "prj-b", OrgID: "org-b", AgentID: "agent-9"},
	},
	Agents: map[string]string{"agent-1": "org-a", "agent-2": "org-a", "agent-9": "org-b"},
}

// Naming a hypothesis of a platform experiment the caller cannot reach answers 404 before the
// service is asked, whose own refusal would name that hypothesis's platform experiment.
func TestAWriteNamingAHypothesisOutOfScopeIsNotFound(t *testing.T) {
	svc := poolService(newPoolStore())
	ctx := context.Background()
	own, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-9", agentActor("agent-9"), "pe-other", "decay hurts",
		[]domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-other"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	caller := authtest.As(auth.Principal{Kind: auth.KindUser, UserID: "user-1", OrgIDs: []string{"org-a"}})
	r := authtest.Router(caller)
	registerRegistry(NewDoc(r, "test", "1.0.0", "", auth.NewAccess(watchTenancy), zap.NewNop()), svc, zap.NewNop())
	post := func(path string, body map[string]any) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, authtest.Authorize(request))
		return rec
	}
	link := func(from, to string) *httptest.ResponseRecorder {
		return post("/hypotheses/"+url.PathEscape(from)+"/links", map[string]any{"agent_id": "agent-1", "type": "related", "to": to})
	}
	register := func(buildsOn string, distinctFrom ...string) *httptest.ResponseRecorder {
		return post("/hypotheses", map[string]any{
			"agent_id": "agent-1", "platform_experiment_id": "pe-1", "text": "cosine decay",
			"links": []map[string]string{{"type": "builds_on", "to": buildsOn}}, "distinct_from": distinctFrom,
		})
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"link to it":         link(own.ID, foreign.ID),
		"link from it":       link(foreign.ID, own.ID),
		"build on it":        register(foreign.ID),
		"told apart from it": register("pe-1", foreign.ID),
	} {
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "pe-other") {
			t.Errorf("%s: got = %d %s, want 404 naming nothing of the other platform experiment", name, rec.Code, rec.Body.String())
		}
	}
	if rec := register("pe-1"); rec.Code >= 300 {
		t.Errorf("a hypothesis inside the caller's platform experiment: got = %d %s", rec.Code, rec.Body.String())
	}
}

// enoughSaid is a reason long enough to carry a verdict the record does not.
const enoughSaid = "the job diverged to NaN in its first step, twice"
