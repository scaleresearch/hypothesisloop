package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
)

// poolStore is the Postgres side of the registry reduced to what hypothesis writes consult: every
// platform experiment exists with a mixed policy, every agent exists, and a comment is accepted.
// It embeds Store so any method a test did not think about panics instead of quietly answering zero.
type poolStore struct {
	Store
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

// The baseline is the objective's own measurement, so the agent that measures it has no claim to
// register: a hypothesis of its own would stay open under the objective for the whole run.
func TestABaselineAgentRegistersNoHypothesis(t *testing.T) {
	svc := poolService(newPoolStore())
	_, _, _, _, err := svc.RegisterHypothesis(context.Background(), "baseline-agent", agentActor("baseline-agent"), "pe-1", "the control", buildOnRoot, nil)
	if !errors.Is(err, ErrBaselineRegistersNoHypothesis) {
		t.Fatalf("RegisterHypothesis for a baseline agent: got = %v, want %v", err, ErrBaselineRegistersNoHypothesis)
	}
	var refusal *api.Refusal
	if answered := (&Handler{}).hypothesisWriteError(err); !errors.As(answered, &refusal) || refusal.GetStatus() != http.StatusForbidden || refusal.Reason != "baseline_registers_no_hypothesis" {
		t.Errorf("the refusal as answered: got = %#v, want 403 baseline_registers_no_hypothesis", answered)
	}
	if _, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "the control", buildOnRoot, nil); err != nil {
		t.Errorf("RegisterHypothesis for a researcher: got = %v, want nil", err)
	}
}

// A person or a key reaches the agents of every platform experiment of its org, and naming one
// was only checked to exist: a hypothesis could be registered here owned by an agent of another
// platform experiment.
func TestAnAgentWithNoSignupHereRegistersNoHypothesis(t *testing.T) {
	store := newPoolStore()
	svc := poolService(store)
	_, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-elsewhere", adaActor, "pe-1", "warmup helps", buildOnRoot, nil)
	if !errors.Is(err, db.ErrNotSignedUp) {
		t.Fatalf("RegisterHypothesis for an agent of another platform experiment: got = %v, want %v", err, db.ErrNotSignedUp)
	}
	var refusal *api.Refusal
	if answered := (&Handler{}).hypothesisWriteError(err); !errors.As(answered, &refusal) || refusal.GetStatus() != http.StatusForbidden || refusal.Reason != "not_signed_up" {
		t.Errorf("the refusal as answered: got = %#v, want 403 not_signed_up", answered)
	}
	if rows := svc.graph.(*fakeGraph).rows; len(rows) != 0 {
		t.Errorf("hypotheses after the refusal: got = %v, want none", rows)
	}
}

// CreateHypothesisComment mirrors the store insert, which consults nothing about the hypothesis
// beyond its id — a note is attached to a row, not authorised against its owner.
func (s *poolStore) CreateHypothesisComment(_ context.Context, hypothesisID, platformExperimentID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error) {
	if s.failNextComment {
		s.failNextComment = false
		return nil, errors.New("insert failed")
	}
	if s.closed[platformExperimentID] {
		return nil, fmt.Errorf("%w: %s", db.ErrPlatformExperimentClosed, platformExperimentID)
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
// validation itself is the graph store's and is tested against Memgraph in shared/graphdb.
type fakeGraph struct {
	Graph
	rows  map[string]*domain.Hypothesis
	links map[string]bool
	// in is the links into each hypothesis, by its id.
	in    map[string][]domain.Link
	clock time.Time
	// positions is the watch stream position each write was stamped with, by hypothesis id.
	positions map[string][]int64
	// stamps is the position each announced fact holds now, by fact.
	stamps map[string]int64
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{rows: map[string]*domain.Hypothesis{}, links: map[string]bool{}, in: map[string][]domain.Link{},
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), positions: map[string][]int64{}, stamps: map[string]int64{}}
}

// stamp records the position of a fact a write announced.
func (g *fakeGraph) stamp(id, fact string, position int64) {
	g.stamps[fact] = position
	g.positions[id] = append(g.positions[id], position)
}

// restamp mirrors graphdb's: a repeated write over a fact whose transaction aborted takes the
// repeat's position and announces it again.
func (g *fakeGraph) restamp(id, fact string, stamp db.Stamp) (bool, error) {
	aborted, err := stamp.Aborted(g.stamps[fact])
	if err != nil || !aborted {
		return false, err
	}
	g.stamp(id, fact, stamp.Position)
	return true, nil
}

func (g *fakeGraph) tick() time.Time {
	g.clock = g.clock.Add(time.Second)
	return g.clock
}

func (g *fakeGraph) Register(_ context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, bool, []domain.SimilarHypothesis, error) {
	id := n.PlatformExperimentID + "\x00" + domain.NormalizeHypothesisText(n.Text)
	if existing, ok := g.rows[id]; ok {
		announce, err := g.restamp(id, "new"+id, n.Stamp)
		return existing, []domain.Link{}, true, announce, nil, err
	}
	h := &domain.Hypothesis{
		ID: id, AgentID: n.AgentID, Actor: n.Actor,
		PlatformExperimentID: n.PlatformExperimentID, Text: n.Text, Status: domain.HypothesisOpen, CreatedAt: g.tick(),
	}
	g.rows[id] = h
	g.stamp(id, "new"+id, n.Stamp.Position)
	return h, []domain.Link{}, false, true, nil, nil
}

func (g *fakeGraph) GetHypothesis(_ context.Context, id string) (*domain.Hypothesis, error) {
	return g.rows[id], nil
}

func (g *fakeGraph) UpdateStatus(_ context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus, stamp db.Stamp) (*domain.Hypothesis, bool, error) {
	h, ok := g.rows[id]
	if !ok {
		return nil, false, graphdb.ErrNotFound
	}
	if h.AgentID != "" && h.AgentID != callerAgentID {
		return nil, false, graphdb.ErrNotOwner
	}
	if h.Status == status {
		announce, err := g.restamp(id, "status"+id, stamp)
		return h, announce, err
	}
	at := g.tick()
	h.Status, h.StatusChangedAt, h.StatusActor = status, &at, &actor
	g.stamp(id, "status"+id, stamp.Position)
	return h, true, nil
}

func (g *fakeGraph) AddLink(_ context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec, stamp db.Stamp) (*graphdb.LinkResult, error) {
	h, ok := g.rows[fromID]
	if !ok {
		return nil, graphdb.ErrNotFound
	}
	if h.AgentID != "" && h.AgentID != agentID {
		return nil, graphdb.ErrNotOwner
	}
	key := fromID + string(spec.Type) + spec.To
	result := &graphdb.LinkResult{From: h, Created: !g.links[key], Announce: !g.links[key],
		Out: []domain.Link{{Type: spec.Type, HypothesisID: spec.To, AgentID: agentID, Actor: actor}}}
	g.links[key] = true
	if !result.Created {
		var err error
		result.Announce, err = g.restamp(fromID, "link"+key, stamp)
		return result, err
	}
	result.CreatedAt = g.tick()
	g.stamp(fromID, "link"+key, stamp.Position)
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

// recordedEvents is the watch stream as the registry sees it. Emit hands out positions 1, 2, ...
// and stamps them on the events, as Postgres would.
type recordedEvents struct {
	events   []db.Event
	position int64
	// failNext fails the next Emit after its write, as a Postgres commit failing after the graph
	// write committed; its position is then aborted.
	failNext bool
	aborted  map[int64]bool
	// closed holds the platform experiments that are closed.
	closed map[string]bool
}

func (r *recordedEvents) NotifyEvent(_ context.Context, e db.Event) error {
	r.events = append(r.events, e)
	return nil
}

func (r *recordedEvents) Emit(_ context.Context, write func(stamp db.Stamp) ([]db.Event, error)) error {
	r.position++
	events, err := write(db.Stamp{Position: r.position, Aborted: func(p int64) (bool, error) { return r.aborted[p], nil },
		RequireOpen: func(pe string) error {
			if r.closed[pe] {
				return fmt.Errorf("%w: %s", db.ErrPlatformExperimentClosed, pe)
			}
			return nil
		}})
	if err != nil {
		return err
	}
	if r.failNext {
		r.failNext = false
		if r.aborted == nil {
			r.aborted = map[int64]bool{}
		}
		r.aborted[r.position] = true
		return errors.New("commit failed")
	}
	for _, e := range events {
		e.Position = r.position
		r.events = append(r.events, e)
	}
	return nil
}

func poolService(store Store) *Service {
	return &Service{store: store, graph: newFakeGraph(), logger: zap.NewNop(), events: &recordedEvents{}}
}

var buildOnRoot = []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-1"}}

// A hypothesis registered for no agent has no owner, and who registered it is kept as its actor.
// Everything that distinguishes such a row later (owning no job, holding no quota, appearing in
// no standings) follows from that empty owner column rather than from a branch of its own.
func TestRegisterHypothesisStoresAHumanRowWithAnEmptyAgentIDAndItsActor(t *testing.T) {
	svc := poolService(newPoolStore())

	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	if h.AgentID != "" {
		t.Errorf("owner: got = %q, want no agent", h.AgentID)
	}
	if want := (domain.Actor{Kind: domain.ActorUser, ID: "user-ada", Name: "user-ada"}); h.Actor != want {
		t.Errorf("actor: got = %+v, want %+v", h.Actor, want)
	}
	if h.AgentID != "" {
		t.Errorf("agent_id on a human row: got = %v, want %v", h.AgentID, "")
	}
}

// Human ideas are in the same pool under the same dedup, so an agent restating a human's claim
// gets the human's row back rather than a second one. A pool that deduped per source would hold
// the same idea twice and hand every reading agent two entries to reconcile.
func TestRegisteringAnAgentRowWithAHumanRowsTextReturnsTheExistingHumanRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", "Warmup   helps.", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis (human): got = %v, want nil", err)
	}

	agent, _, existed, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps.", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis (agent): got = %v, want nil", err)
	}
	if !existed {
		t.Errorf("already_existed: got = %v, want %v", existed, true)
	}
	if agent.ID != human.ID {
		t.Errorf("deduped id: got = %v, want %v", agent.ID, human.ID)
	}
}

// Agents run jobs against human ideas, so a human idea gets settled — and with no owner to settle
// it, the row would sit open forever and the pool would lie about what is still unresolved. The
// rule is therefore the owner predicate read literally: nobody owns it, so whoever tested it may
// record the verdict, with the findings underneath as the record of who earned it.
func TestSetHypothesisStatusLetsAnyAgentSettleAnUnownedHumanRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	settled, err := svc.SetHypothesisStatus(context.Background(), human.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid})
	if err != nil {
		t.Fatalf("SetHypothesisStatus on a human row: got = %v, want nil", err)
	}
	if settled.Status != domain.HypothesisRefuted {
		t.Errorf("status: got = %v, want %v", settled.Status, domain.HypothesisRefuted)
	}
	if settled.AgentID != "" {
		t.Errorf("agent_id after settling: got = %v, want it to stay unowned", settled.AgentID)
	}
}

// The same rule from the other direction: an agent's own row stays its own to judge, and a
// second agent's verdict on it is refused rather than recorded.
func TestSetHypothesisStatusRefusesAnAgentSettingAnotherAgentsRowStatus(t *testing.T) {
	svc := poolService(newPoolStore())
	owned, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-2", agentActor("agent-2"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); !errors.Is(err, ErrHypothesisNotOwner) {
		t.Fatalf("SetHypothesisStatus by a non-owner: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); err != nil {
		t.Fatalf("SetHypothesisStatus by the owner: got = %v, want nil", err)
	}
}

// Discussion of an idea has to reach the row the idea is on. If an agent could not comment on a
// human's hypothesis, its observations would have to land somewhere else, and the evidence for one
// claim would end up split across two rows — the same fragmentation that refusing to run jobs
// against human rows caused. Nothing about the hypothesis's source is consulted when a note is
// attached, and this test holds that open.
func TestAddHypothesisCommentAcceptsAnAgentsNoteOnAHumanSubmittedRow(t *testing.T) {
	svc := poolService(newPoolStore())
	human, _, _, _, err := svc.RegisterHypothesis(context.Background(), "", adaActor, "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	c, err := svc.AddHypothesisComment(context.Background(), human.ID, "agent-1", agentActor("agent-1"), "retested at 500 steps")
	if err != nil {
		t.Fatalf("AddHypothesisComment on a human row: got = %v, want nil", err)
	}
	if c.AgentID != "agent-1" {
		t.Errorf("comment owner: got = %v, want agent-1", c.AgentID)
	}
	if c.HypothesisID != human.ID {
		t.Errorf("comment hypothesis_id: got = %v, want %v", c.HypothesisID, human.ID)
	}
}

// Researchers were told to post the literature as a comment on the objective, and each try was
// 404: the objective is no hypothesis node. It holds comments under its platform experiment's
// id, the id that names it in every link and in the baseline's jobs, and a platform experiment
// that does not exist holds none.
func TestACommentOnTheObjectiveIsKeptUnderThePlatformExperimentsID(t *testing.T) {
	svc := poolService(newPoolStore())
	c, err := svc.AddHypothesisComment(context.Background(), "pe-1", "agent-1", agentActor("agent-1"), "published: 4.3x, link")
	if err != nil {
		t.Fatalf("AddHypothesisComment on the objective: got = %v, want nil", err)
	}
	if c.HypothesisID != "pe-1" || c.PlatformExperimentID != "pe-1" {
		t.Errorf("comment on the objective: hypothesis_id %q, platform_experiment_id %q, want both pe-1", c.HypothesisID, c.PlatformExperimentID)
	}
	comments, total, err := svc.ListHypothesisComments(context.Background(), "pe-1", 20, 0)
	if err != nil || total != 1 || len(comments) != 1 || comments[0].Text != "published: 4.3x, link" {
		t.Errorf("ListHypothesisComments(pe-1): got = %v, %d, %v, want the comment", comments, total, err)
	}
	if _, err := svc.AddHypothesisComment(context.Background(), "pe-none", "agent-1", agentActor("agent-1"), "x"); !errors.Is(err, ErrHypothesisNotFound) {
		t.Errorf("AddHypothesisComment on no platform experiment: got = %v, want %v", err, ErrHypothesisNotFound)
	}
}

// The graph has no NOTIFY, so the registry is what tells /watch about a new idea. The graph write
// has to carry the position its event is emitted at: replay derives the same event from that
// stored value, and a reconnecting watcher must see one event, not two.
func TestRegisteringANewHypothesisStampsTheGraphWriteWithItsEventsPosition(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.events = events
	graph := svc.graph.(*fakeGraph)

	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	want := db.Event{Kind: db.EventHypothesisNew, Subject: h.ID, Value: string(domain.ActorAgent), PlatformExperimentID: "pe-1",
		AgentID: "agent-1", Position: 1}
	if len(events.events) != 1 || events.events[0] != want {
		t.Errorf("events: got = %+v, want = [%+v]", events.events, want)
	}
	if got := graph.positions[h.ID]; len(got) != 1 || got[0] != 1 {
		t.Errorf("graph write positions: got = %v, want = [1]", got)
	}
}

// A dedup hit is the existing idea handed back, and announcing it again would wake every agent for
// a claim they already know about.
func TestRegisteringAnExistingHypothesisAnnouncesNothing(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.events = events
	if _, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil); err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	if _, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-2", agentActor("agent-2"), "pe-1", "Warmup helps", buildOnRoot, nil); err != nil {
		t.Fatalf("RegisterHypothesis (duplicate): got = %v, want nil", err)
	}
	if len(events.events) != 1 {
		t.Errorf("events after a dedup hit: got = %v, want = %v", len(events.events), 1)
	}
}

// A verdict is announced once, when it changes; re-sending the same verdict is not news.
func TestSettlingAHypothesisAnnouncesTheVerdictOnlyWhenItChanges(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.events = events
	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	for range 2 {
		if _, err := svc.SetHypothesisStatus(context.Background(), h.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid}); err != nil {
			t.Fatalf("SetHypothesisStatus: got = %v, want nil", err)
		}
	}
	if len(events.events) != 2 {
		t.Fatalf("events: got = %v, want = %v (hypothesis.new, then one hypothesis.status)", len(events.events), 2)
	}
	got := events.events[1]
	positions := svc.graph.(*fakeGraph).positions[h.ID]
	if got.Kind != db.EventHypothesisStatus || got.Value != "refuted" || len(positions) != 2 || got.Position != positions[1] {
		t.Errorf("status event: got = %+v (graph positions %v), want = hypothesis.status refuted at the status write's position", got, positions)
	}
}

// A link added after registration is the one way an agent places its idea further in the graph,
// and other agents need to hear about it; a link that already existed changed nothing.
func TestAddingALinkAnnouncesItOnceWithTheTargetAsDetail(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.events = events
	a, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	b, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-2", agentActor("agent-2"), "pe-1", "cooldown helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	link := domain.LinkSpec{Type: domain.LinkContradicts, To: b.ID}
	for range 2 {
		if _, err := svc.AddHypothesisLink(context.Background(), a.ID, "agent-1", agentActor("agent-1"), link); err != nil {
			t.Fatalf("AddHypothesisLink: got = %v, want nil", err)
		}
	}
	if len(events.events) != 3 {
		t.Fatalf("events: got = %v, want = %v (two hypothesis.new, one hypothesis.link)", len(events.events), 3)
	}
	got := events.events[2]
	if got.Kind != db.EventHypothesisLink || got.Subject != a.ID || got.Value != "contradicts" || got.Detail != b.ID {
		t.Errorf("link event: got = %+v, want = hypothesis.link %s contradicts %s", got, a.ID, b.ID)
	}
}

// The graph write commits before the Postgres transaction that emits its event, so that commit
// can fail with the write already made. The caller's retry finds the write done and has to send
// the event the failed attempt never did, once, at the retry's own position.
func TestARetryAfterAFailedEmitSendsTheEventTheFailureLost(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore())
	svc.events = events
	ctx := context.Background()
	b, _, _, _, err := svc.RegisterHypothesis(ctx, "agent-2", agentActor("agent-2"), "pe-1", "cooldown helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	var a *domain.Hypothesis
	link := domain.LinkSpec{Type: domain.LinkContradicts, To: b.ID}
	for _, write := range []struct {
		kind string
		do   func() error
	}{
		{db.EventHypothesisNew, func() error {
			a, _, _, _, err = svc.RegisterHypothesis(ctx, "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
			return err
		}},
		{db.EventHypothesisStatus, func() error {
			_, err := svc.SetHypothesisStatus(ctx, a.ID, "agent-1", agentActor("agent-1"), Verdict{Status: domain.HypothesisRefuted, Despite: enoughSaid})
			return err
		}},
		{db.EventHypothesisLink, func() error {
			_, err := svc.AddHypothesisLink(ctx, a.ID, "agent-1", agentActor("agent-1"), link)
			return err
		}},
	} {
		sent := len(events.events)
		events.failNext = true
		if err := write.do(); err == nil {
			t.Fatalf("%s with a failing commit: got = nil, want an error", write.kind)
		}
		for range 2 {
			if err := write.do(); err != nil {
				t.Fatalf("%s retry: got = %v, want nil", write.kind, err)
			}
		}
		got := events.events[sent:]
		if len(got) != 1 || got[0].Kind != write.kind || got[0].Position != events.position-1 {
			t.Errorf("%s events after a failed commit and two retries: got = %+v, want one at the first retry's position %d",
				write.kind, got, events.position-1)
		}
	}
}

// Linking from another agent's hypothesis would put words in its mouth; the registry passes the
// graph's refusal through as the owner error the HTTP layer turns into 403.
func TestAddingALinkFromAnotherAgentsHypothesisIsRefused(t *testing.T) {
	svc := poolService(newPoolStore())
	a, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	_, err = svc.AddHypothesisLink(context.Background(), a.ID, "agent-2", agentActor("agent-2"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: "pe-1"})
	if !errors.Is(err, ErrHypothesisNotOwner) {
		t.Fatalf("AddHypothesisLink by a non-owner: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
}

// A comment carries its hypothesis's platform experiment so its replay and its notify trigger need
// nothing outside its own row; a comment on a hypothesis that does not exist is a 404, not a row.
func TestAddHypothesisCommentCarriesTheHypothesisPlatformExperimentAndRefusesAnUnknownOne(t *testing.T) {
	svc := poolService(newPoolStore())
	h, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	c, err := svc.AddHypothesisComment(context.Background(), h.ID, "agent-2", agentActor("agent-2"), "retested")
	if err != nil {
		t.Fatalf("AddHypothesisComment: got = %v, want nil", err)
	}
	if c.PlatformExperimentID != "pe-1" {
		t.Errorf("comment platform_experiment_id: got = %v, want %v", c.PlatformExperimentID, "pe-1")
	}
	if _, err := svc.AddHypothesisComment(context.Background(), "no-such-hypothesis", "agent-2", agentActor("agent-2"), "x"); !errors.Is(err, ErrHypothesisNotFound) {
		t.Errorf("AddHypothesisComment on an unknown hypothesis: got = %v, want %v", err, ErrHypothesisNotFound)
	}
}

type missingExperimentStore struct{ poolStore }

func (missingExperimentStore) PlatformExperimentExists(context.Context, string) (bool, error) {
	return false, nil
}

func (missingExperimentStore) GetPlatformExperimentMetrics(context.Context, string) ([]domain.MetricDefinition, bool, error) {
	return nil, false, nil
}

// The graph does not know which platform experiments exist; Postgres does, and a hypothesis under
// an id nobody created would be an orphan no job could ever run against.
func TestRegisterHypothesisRefusesAnUnknownPlatformExperiment(t *testing.T) {
	svc := poolService(&missingExperimentStore{})

	_, _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", agentActor("agent-1"), "pe-missing", "warmup helps",
		[]domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-missing"}}, nil)
	if !errors.Is(err, ErrUnknownPlatformExperiment) {
		t.Fatalf("RegisterHypothesis under an unknown platform experiment: got = %v, want %v", err, ErrUnknownPlatformExperiment)
	}
}

func TestHypothesisGraphOfAnUnknownPlatformExperimentIsNotFound(t *testing.T) {
	r := chi.NewRouter()
	RegisterHuma(apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop()), NewHandler(poolService(&missingExperimentStore{}), zap.NewNop()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/hypotheses/graph?platform_experiment_id=pe-missing", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

type evidenceStore struct {
	poolStore
	evidence map[string]any
}

func (s *evidenceStore) Evidence(_ context.Context, _ string) (map[string]any, error) {
	return s.evidence, nil
}

type queryGraph struct {
	fakeGraph
	params map[string]any
}

func (g *queryGraph) Query(_ context.Context, _, _ string, params map[string]any) (*graphdb.QueryResult, error) {
	g.params = params
	return &graphdb.QueryResult{}, nil
}

// Jobs, findings and comments live in Postgres, so a query sees them only through the map bound
// as $evidence, and a caller's own $evidence must not stand in for it.
func TestQueryGraphBindsTheEvidence(t *testing.T) {
	graph := &queryGraph{}
	evidence := map[string]any{"h-1": map[string]any{"jobs": int64(1)}}
	svc := &Service{store: &evidenceStore{evidence: evidence}, graph: graph, logger: zap.NewNop()}

	if _, err := svc.QueryGraph(context.Background(), "pe-1", "RETURN $evidence", map[string]any{"id": "h-2"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.params, map[string]any{"id": "h-2", "evidence": evidence}) {
		t.Fatalf("params = %v", graph.params)
	}
	if _, err := svc.QueryGraph(context.Background(), "pe-1", "RETURN 1", map[string]any{"evidence": "x"}); !errors.Is(err, graphdb.ErrInvalidQuery) {
		t.Fatalf("caller $evidence: err = %v, want ErrInvalidQuery", err)
	}
}

// In r7 a held agent ran its jobs under the close hypothesis and was then refused setting its
// status. The hold has to say whose verdict that is and that a narrower claim is its own.
func TestASimilarityHoldSaysWhoseVerdictTheCloseHypothesisIsAndThatANarrowerClaimIsNew(t *testing.T) {
	err := (&Handler{}).hypothesisWriteError(&graphdb.SimilarError{Similar: []domain.SimilarHypothesis{{ID: "h-1"}}})
	var hold *similarHypothesesError
	if !errors.As(err, &hold) || hold.GetStatus() != http.StatusConflict || len(hold.Similar) != 1 {
		t.Fatalf("hypothesisWriteError(SimilarError) = %#v, want a 409 carrying the similar hypotheses", err)
	}
	for _, want := range []string{"only its owner sets its status", "narrower, broader", "distinct_from"} {
		if !strings.Contains(hold.Message, want) {
			t.Errorf("message %q does not say %q", hold.Message, want)
		}
	}
	// The answer is the one envelope every refusal has, with the hypotheses beside it.
	encoded, jsonErr := json.Marshal(err)
	var body map[string]json.RawMessage
	if jsonErr != nil || json.Unmarshal(encoded, &body) != nil || len(body) != 3 ||
		string(body["reason"]) != `"similar_hypotheses"` || body["message"] == nil || body["similar"] == nil {
		t.Errorf("the hold as answered: got = %s, want reason similar_hypotheses, message and similar only", encoded)
	}
}

// findingsStore and findingsGraph hold one platform experiment that minimizes loss, with four
// hypotheses: one confirmed, tested by three jobs and written up; one judged with no job; one
// open with a finding; one open with nothing.
type findingsStore struct{ *poolStore }

func (findingsStore) GetPlatformExperimentMetrics(_ context.Context, id string) ([]domain.MetricDefinition, bool, error) {
	return []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}}, id == "pe-1", nil
}

func (findingsStore) NewestFindings(_ context.Context, _ []string) (map[string]*domain.HypothesisFinding, error) {
	return map[string]*domain.HypothesisFinding{
		"hyp-tested": {ExperimentID: "job-b", Summary: "the newest write-up"},
		"hyp-open":   {ExperimentID: "job-c", Summary: "no verdict yet"},
		"pe-1":       {ExperimentID: "job-baseline", Summary: "the baseline measured"},
	}, nil
}

func (findingsStore) FindingSummaries(_ context.Context, ids []string) (map[string]string, error) {
	if !reflect.DeepEqual(ids, []string{"job-b"}) {
		return nil, errors.New("asked for the summaries of other than the best jobs")
	}
	return map[string]string{"job-b": "the best job's write-up"}, nil
}

func (findingsStore) ListExperimentsTesting(_ context.Context, ids []string) ([]*domain.Experiment, error) {
	// The graph asks for the objective's jobs too, under the platform experiment's id.
	if hypotheses := []string{"hyp-tested", "hyp-untested", "hyp-open", "hyp-idle"}; !reflect.DeepEqual(ids, hypotheses) && !reflect.DeepEqual(ids, append(hypotheses, "pe-1")) {
		return nil, errors.New("asked for the jobs of other hypotheses")
	}
	created := time.Now().Add(-time.Hour)
	jobs := []*domain.Experiment{{ID: "job-a"}, {ID: "job-b"}, {ID: "job-rescaled"}, {ID: "job-failed", Status: domain.StatusFailed}}
	for _, job := range jobs {
		if job.Status == "" {
			job.Status = domain.StatusCompleted
		}
		job.PlatformExperimentID, job.HypothesisID, job.CreatedAt = "pe-1", "hyp-tested", created
	}
	return jobs, nil
}

func (findingsStore) CountHypothesisActivity(context.Context, []string) (map[string]db.HypothesisActivity, error) {
	return map[string]db.HypothesisActivity{}, nil
}

type findingsGraph struct{ Graph }

func (g findingsGraph) ExperimentGraph(ctx context.Context, platformExperimentID string) ([]*domain.HypothesisListItem, []domain.GraphEdge, error) {
	nodes, err := g.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExperimentID})
	return nodes, nil, err
}

func (findingsGraph) ListHypotheses(_ context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.PlatformExperimentID != "pe-1" || f.Status != "" {
		return nil, errors.New("asked for other than every hypothesis of pe-1")
	}
	return []*domain.HypothesisListItem{
		{Hypothesis: &domain.Hypothesis{ID: "hyp-tested", Status: domain.HypothesisConfirmed}},
		{Hypothesis: &domain.Hypothesis{ID: "hyp-untested", Status: domain.HypothesisInconclusive}},
		{Hypothesis: &domain.Hypothesis{ID: "hyp-open", Status: domain.HypothesisOpen}},
		{Hypothesis: &domain.Hypothesis{ID: "hyp-idle", Status: domain.HypothesisOpen}},
	}, nil
}

// The experiment page answers "what was found" from one read: each hypothesis with a verdict or
// evidence, whatever its status, with its newest finding and the job that did best on the ranking
// metric, a rescaled value and a job that failed never winning. A hypothesis with neither is
// left out.
func TestFindingsPairEachHypothesisWithItsNewestFindingAndBestJob(t *testing.T) {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"job_id":"job-a","metric_name":"loss","metric_basis":"raw"},"value":[1,"0.30"]},
			{"metric":{"job_id":"job-b","metric_name":"loss","metric_basis":"raw"},"value":[1,"0.10"]},
			{"metric":{"job_id":"job-failed","metric_name":"loss","metric_basis":"raw"},"value":[1,"0.02"]},
			{"metric":{"job_id":"job-rescaled","metric_name":"loss","metric_basis":"per_token"},"value":[1,"0.01"]}]}}`))
	}))
	defer metrics.Close()
	svc := New(findingsStore{newPoolStore()}, findingsGraph{}, &recordedEvents{}, zap.NewNop(), metrics.URL, nil)

	got, err := svc.Findings(context.Background(), "pe-1")
	if err != nil || len(got) != 3 {
		t.Fatalf("Findings: got = (%+v, %v), want 3 hypotheses", got, err)
	}
	tested, untested, open := got[0], got[1], got[2]
	if tested.ID != "hyp-tested" || tested.Finding == nil || tested.Finding.Summary != "the newest write-up" || tested.BestJob == nil ||
		tested.BestJob.ID != "job-b" || *tested.BestJob.Metric != (domain.JobMetric{Name: "loss", Value: 0.10, Basis: "raw"}) ||
		tested.BestJob.Summary != "the best job's write-up" {
		t.Errorf("tested hypothesis: got = %+v (best job %+v), want its newest finding and job-b at loss 0.10 with its own summary", tested, tested.BestJob)
	}
	if untested.ID != "hyp-untested" || untested.Finding != nil || untested.BestJob != nil {
		t.Errorf("untested hypothesis: got = %+v, want no finding and no best job", untested)
	}
	if open.ID != "hyp-open" || open.Finding == nil || open.Finding.Summary != "no verdict yet" {
		t.Errorf("open hypothesis: got = %+v, want it listed with its finding", open)
	}

	// The graph carries the same on its nodes, and keeps the hypothesis nothing was found for.
	graph, err := svc.HypothesisGraph(context.Background(), "pe-1")
	if err != nil || len(graph.Nodes) != 4 || graph.Root != "pe-1" {
		t.Fatalf("HypothesisGraph: got = (%+v, %v), want 4 nodes under pe-1", graph, err)
	}
	// The baseline's job tests no hypothesis: what it found hangs on the objective.
	if graph.Objective.Finding == nil || graph.Objective.Finding.Summary != "the baseline measured" {
		t.Errorf("objective: got = %+v, want the baseline's finding", graph.Objective)
	}
	nodes := graph.Nodes
	if node := nodes[0]; node.ID != "hyp-tested" || node.Finding == nil || node.Finding.Summary != "the newest write-up" ||
		node.BestJob == nil || node.BestJob.ID != "job-b" || node.BestJob.Summary != "the best job's write-up" {
		t.Errorf("tested node: got = %+v, want the finding and best job the findings read answers", node)
	}
	if idle := nodes[3]; idle.ID != "hyp-idle" || idle.Finding != nil || idle.BestJob != nil {
		t.Errorf("idle node: got = %+v, want no finding and no best job", idle)
	}

	if _, err := svc.Findings(context.Background(), "pe-none"); !errors.Is(err, ErrUnknownPlatformExperiment) {
		t.Errorf("Findings of an unknown platform experiment: got = %v, want %v", err, ErrUnknownPlatformExperiment)
	}
}

// enoughSaid is a reason long enough to carry a verdict the record does not.
const enoughSaid = "the job diverged to NaN in its first step, twice"
