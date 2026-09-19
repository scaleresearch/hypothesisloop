package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
)

// poolStore is the Postgres side of the registry reduced to what hypothesis writes consult: every
// platform experiment exists with a mixed policy, every agent exists, and a comment is accepted.
// It embeds Store so any method a test did not think about panics instead of quietly answering zero.
type poolStore struct {
	Store
}

func newPoolStore() *poolStore { return &poolStore{} }

func (s *poolStore) GetPlatformExperimentSubmitPolicies(_ context.Context, _ string) (domain.SubmitterPolicy, domain.SubmitterPolicy, bool, error) {
	return domain.SubmitterPolicyMixed, domain.SubmitterPolicyMixed, true, nil
}

func (s *poolStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	return &domain.Agent{ID: id}, nil
}

// CreateHypothesisComment mirrors the store insert, which consults nothing about the hypothesis
// beyond its id — a note is attached to a row, not authorised against its owner.
func (s *poolStore) CreateHypothesisComment(_ context.Context, hypothesisID, platformExperimentID string, source domain.HypothesisSource, agentID, author, text string) (*domain.HypothesisComment, error) {
	return &domain.HypothesisComment{
		ID: "c-1", HypothesisID: hypothesisID, PlatformExperimentID: platformExperimentID, Source: source,
		AgentID: agentID, Author: author, Text: text,
	}, nil
}

// fakeGraph is the hypothesis graph reduced to the rules the registry relies on it for: dedup on
// (platform_experiment_id, normalized text), and the owner rule on status and links. Link
// validation itself is the graph store's and is tested against Memgraph in shared/graphdb.
type fakeGraph struct {
	Graph
	rows  map[string]*domain.Hypothesis
	links map[string]bool
	clock time.Time
	// positions is the watch stream position each write was stamped with, by hypothesis id.
	positions map[string][]int64
	// stamps is the position each announced fact holds now, by fact.
	stamps map[string]int64
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{rows: map[string]*domain.Hypothesis{}, links: map[string]bool{},
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

func (g *fakeGraph) Register(_ context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, bool, error) {
	id := n.PlatformExperimentID + "\x00" + domain.NormalizeHypothesisText(n.Text)
	if existing, ok := g.rows[id]; ok {
		announce, err := g.restamp(id, "new"+id, n.Stamp)
		return existing, []domain.Link{}, true, announce, err
	}
	h := &domain.Hypothesis{
		ID: id, AgentID: n.AgentID, Source: n.Source, Author: n.Author,
		PlatformExperimentID: n.PlatformExperimentID, Text: n.Text, Status: domain.HypothesisOpen, CreatedAt: g.tick(),
	}
	g.rows[id] = h
	g.stamp(id, "new"+id, n.Stamp.Position)
	return h, []domain.Link{}, false, true, nil
}

func (g *fakeGraph) GetHypothesis(_ context.Context, id string) (*domain.Hypothesis, error) {
	return g.rows[id], nil
}

func (g *fakeGraph) UpdateStatus(_ context.Context, id, callerAgentID string, status domain.HypothesisStatus, stamp db.Stamp) (*domain.Hypothesis, bool, error) {
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
	h.Status, h.StatusChangedAt = status, &at
	g.stamp(id, "status"+id, stamp.Position)
	return h, true, nil
}

func (g *fakeGraph) AddLink(_ context.Context, fromID, agentID, _ string, spec domain.LinkSpec, stamp db.Stamp) (*graphdb.LinkResult, error) {
	h, ok := g.rows[fromID]
	if !ok {
		return nil, graphdb.ErrNotFound
	}
	if h.AgentID != "" && h.AgentID != agentID {
		return nil, graphdb.ErrNotOwner
	}
	key := fromID + string(spec.Type) + spec.To
	result := &graphdb.LinkResult{From: h, Created: !g.links[key], Announce: !g.links[key], Out: []domain.Link{}}
	g.links[key] = true
	if !result.Created {
		var err error
		result.Announce, err = g.restamp(fromID, "link"+key, stamp)
		return result, err
	}
	result.CreatedAt = g.tick()
	g.stamp(fromID, "link"+key, stamp.Position)
	return result, nil
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
}

func (r *recordedEvents) NotifyEvent(_ context.Context, e db.Event) error {
	r.events = append(r.events, e)
	return nil
}

func (r *recordedEvents) Emit(_ context.Context, write func(stamp db.Stamp) ([]db.Event, error)) error {
	r.position++
	events, err := write(db.Stamp{Position: r.position, Aborted: func(p int64) (bool, error) { return r.aborted[p], nil }})
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

// A row carrying both an agent_id and a human author would be owned and unowned at the same
// time: quota, standings and the job-ownership check all key on agent_id, so the row would spend
// an agent's budget while presenting as someone's idea. Registration is where this is decided,
// and the only honest answer is to refuse.
func TestRegisterHypothesisRejectsARowClaimingBothAnAgentAndAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "Ada", "pe-1", "warmup helps", buildOnRoot, nil)
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("RegisterHypothesis with both agent_id and author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}

// A row with neither an agent_id nor an author is attributable to nobody. There is no default to
// fall back on — inventing one would put an unattributed claim in a pool whose entire value is
// that every claim has someone behind it.
func TestRegisterHypothesisRejectsARowWithNeitherAnAgentNorAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, _, _, err := svc.RegisterHypothesis(context.Background(), "", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("RegisterHypothesis with neither agent_id nor author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}

// The name a human types is a claim, not an identity — there is no auth — so it must land on the
// row as an author, leaving agent_id empty. Everything that distinguishes a human row later
// (owning no job, holding no quota, appearing in no standings) follows from that empty owner
// column rather than from a branch of its own.
func TestRegisterHypothesisStoresAHumanRowWithAnEmptyAgentIDAndTheTypedName(t *testing.T) {
	svc := poolService(newPoolStore())

	h, _, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	if h.Source != domain.HypothesisSourceHuman {
		t.Errorf("source: got = %v, want %v", h.Source, domain.HypothesisSourceHuman)
	}
	if h.Author != "Ada" {
		t.Errorf("author: got = %v, want %v", h.Author, "Ada")
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
	human, _, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "Warmup   helps.", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis (human): got = %v, want nil", err)
	}

	agent, _, existed, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps.", buildOnRoot, nil)
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
	human, _, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	settled, err := svc.SetHypothesisStatus(context.Background(), human.ID, "agent-1", domain.HypothesisRefuted)
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
	owned, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-2", domain.HypothesisRefuted); !errors.Is(err, ErrHypothesisNotOwner) {
		t.Fatalf("SetHypothesisStatus by a non-owner: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
	if _, err := svc.SetHypothesisStatus(context.Background(), owned.ID, "agent-1", domain.HypothesisRefuted); err != nil {
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
	human, _, _, err := svc.RegisterHypothesis(context.Background(), "", "Ada", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	c, err := svc.AddHypothesisComment(context.Background(), human.ID, "agent-1", "", "retested at 500 steps")
	if err != nil {
		t.Fatalf("AddHypothesisComment on a human row: got = %v, want nil", err)
	}
	if c.AgentID != "agent-1" || c.Source != domain.HypothesisSourceAgent {
		t.Errorf("comment attribution: got = (%v, %v), want (agent-1, %v)", c.AgentID, c.Source, domain.HypothesisSourceAgent)
	}
	if c.HypothesisID != human.ID {
		t.Errorf("comment hypothesis_id: got = %v, want %v", c.HypothesisID, human.ID)
	}
}

// A comment carries the same origin pair as a hypothesis, and for the same reason: a note signed
// by both an agent and a person is signed by neither.
func TestAddHypothesisCommentRejectsARowClaimingBothAnAgentAndAHumanAuthor(t *testing.T) {
	svc := poolService(newPoolStore())

	_, err := svc.AddHypothesisComment(context.Background(), "h-1", "agent-1", "Ada", "try a shorter warmup")
	if !errors.Is(err, domain.ErrHypothesisOrigin) {
		t.Fatalf("AddHypothesisComment with both agent_id and author: got = %v, want %v", err, domain.ErrHypothesisOrigin)
	}
}

// The graph has no NOTIFY, so the registry is what tells /watch about a new idea. The graph write
// has to carry the position its event is emitted at: replay derives the same event from that
// stored value, and a reconnecting watcher must see one event, not two.
func TestRegisteringANewHypothesisStampsTheGraphWriteWithItsEventsPosition(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore()).WithEvents(events)
	graph := svc.graph.(*fakeGraph)

	h, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	want := db.Event{Kind: db.EventHypothesisNew, Subject: h.ID, Value: "agent", PlatformExperimentID: "pe-1",
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
	svc := poolService(newPoolStore()).WithEvents(events)
	if _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil); err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	if _, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-2", "", "pe-1", "Warmup helps", buildOnRoot, nil); err != nil {
		t.Fatalf("RegisterHypothesis (duplicate): got = %v, want nil", err)
	}
	if len(events.events) != 1 {
		t.Errorf("events after a dedup hit: got = %v, want = %v", len(events.events), 1)
	}
}

// A verdict is announced once, when it changes; re-sending the same verdict is not news.
func TestSettlingAHypothesisAnnouncesTheVerdictOnlyWhenItChanges(t *testing.T) {
	events := &recordedEvents{}
	svc := poolService(newPoolStore()).WithEvents(events)
	h, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	for range 2 {
		if _, err := svc.SetHypothesisStatus(context.Background(), h.ID, "agent-1", domain.HypothesisRefuted); err != nil {
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
	svc := poolService(newPoolStore()).WithEvents(events)
	a, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}
	b, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-2", "", "pe-1", "cooldown helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	link := domain.LinkSpec{Type: domain.LinkContradicts, To: b.ID}
	for range 2 {
		if _, err := svc.AddHypothesisLink(context.Background(), a.ID, "agent-1", "", link); err != nil {
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
	svc := poolService(newPoolStore()).WithEvents(events)
	ctx := context.Background()
	b, _, _, err := svc.RegisterHypothesis(ctx, "agent-2", "", "pe-1", "cooldown helps", buildOnRoot, nil)
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
			a, _, _, err = svc.RegisterHypothesis(ctx, "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
			return err
		}},
		{db.EventHypothesisStatus, func() error {
			_, err := svc.SetHypothesisStatus(ctx, a.ID, "agent-1", domain.HypothesisRefuted)
			return err
		}},
		{db.EventHypothesisLink, func() error {
			_, err := svc.AddHypothesisLink(ctx, a.ID, "agent-1", "", link)
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
	a, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	_, err = svc.AddHypothesisLink(context.Background(), a.ID, "agent-2", "", domain.LinkSpec{Type: domain.LinkBuildsOn, To: "pe-1"})
	if !errors.Is(err, ErrHypothesisNotOwner) {
		t.Fatalf("AddHypothesisLink by a non-owner: got = %v, want %v", err, ErrHypothesisNotOwner)
	}
}

// A comment carries its hypothesis's platform experiment so its replay and its notify trigger need
// nothing outside its own row; a comment on a hypothesis that does not exist is a 404, not a row.
func TestAddHypothesisCommentCarriesTheHypothesisPlatformExperimentAndRefusesAnUnknownOne(t *testing.T) {
	svc := poolService(newPoolStore())
	h, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-1", "warmup helps", buildOnRoot, nil)
	if err != nil {
		t.Fatalf("RegisterHypothesis: got = %v, want nil", err)
	}

	c, err := svc.AddHypothesisComment(context.Background(), h.ID, "agent-2", "", "retested")
	if err != nil {
		t.Fatalf("AddHypothesisComment: got = %v, want nil", err)
	}
	if c.PlatformExperimentID != "pe-1" {
		t.Errorf("comment platform_experiment_id: got = %v, want %v", c.PlatformExperimentID, "pe-1")
	}
	if _, err := svc.AddHypothesisComment(context.Background(), "no-such-hypothesis", "agent-2", "", "x"); !errors.Is(err, ErrHypothesisNotFound) {
		t.Errorf("AddHypothesisComment on an unknown hypothesis: got = %v, want %v", err, ErrHypothesisNotFound)
	}
}

type missingExperimentStore struct{ poolStore }

func (missingExperimentStore) GetPlatformExperimentSubmitPolicies(context.Context, string) (domain.SubmitterPolicy, domain.SubmitterPolicy, bool, error) {
	return "", "", false, nil
}

// The graph does not know which platform experiments exist; Postgres does, and a hypothesis under
// an id nobody created would be an orphan no job could ever run against.
func TestRegisterHypothesisRefusesAnUnknownPlatformExperiment(t *testing.T) {
	svc := poolService(&missingExperimentStore{})

	_, _, _, err := svc.RegisterHypothesis(context.Background(), "agent-1", "", "pe-missing", "warmup helps",
		[]domain.LinkSpec{{Type: domain.LinkBuildsOn, To: "pe-missing"}}, nil)
	if !errors.Is(err, ErrUnknownPlatformExperiment) {
		t.Fatalf("RegisterHypothesis under an unknown platform experiment: got = %v, want %v", err, ErrUnknownPlatformExperiment)
	}
}
