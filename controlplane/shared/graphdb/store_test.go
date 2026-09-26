package graphdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// These tests run against a real Memgraph or not at all: uniqueness under concurrency, the
// rollback of a read query and the text index are Memgraph's behaviour, which a fake cannot show.
// `make test` starts a throwaway one on this port, never the stack's own on 7687.
const devGraphURL = "bolt://localhost:7688"

func testGraphURL() string {
	if url := os.Getenv("HYPOTHESISLOOP_TEST_GRAPH_DB_URL"); url != "" {
		return url
	}
	return devGraphURL
}

func testStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Open(ctx, testGraphURL())
	if err != nil {
		t.Skipf("no graph database at %s: %v", testGraphURL(), err)
	}
	t.Cleanup(func() { s.Close(context.Background()) }) //nolint:errcheck
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: got = %v, want nil", err)
	}
	return s
}

// testExperiment returns a platform experiment id no other test uses, and removes its nodes after.
func testExperiment(t *testing.T, s *Store) string {
	t.Helper()
	pe := "graph-test-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			return collect(ctx, tx, `MATCH (n) WHERE n.platform_experiment_id = $pe OR (n:Objective AND n.id = $pe) DETACH DELETE n`,
				map[string]any{"pe": pe})
		})
		if err != nil {
			t.Errorf("cleanup %s: %v", pe, err)
		}
	})
	return pe
}

func onRoot(pe string) []domain.LinkSpec {
	return []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: pe}}
}

// stampAt stamps a write at position, as an Emit that finds every earlier write's events sent.
func stampAt(position int64) db.Stamp {
	return db.Stamp{Position: position, Aborted: func(int64) (bool, error) { return false, nil }}
}

func register(t *testing.T, s *Store, pe, agentID, text string, links []domain.LinkSpec) *domain.Hypothesis {
	t.Helper()
	h, _, existed, _, err := s.Register(context.Background(), NewHypothesis{
		AgentID: agentID, Actor: agentActor(agentID), PlatformExperimentID: pe, Text: text, Links: links,
	})
	if err != nil {
		t.Fatalf("Register %q: got = %v, want nil", text, err)
	}
	if existed {
		t.Fatalf("Register %q: already_existed = true, want a new hypothesis", text)
	}
	return h
}

func TestRegisterCreatesTheHypothesisWithItsLinksInOneStep(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "warmup helps", onRoot(pe))
	b := register(t, s, pe, "agent-1", "cosine decay helps", onRoot(pe))

	h, links, existed, _, err := s.Register(context.Background(), NewHypothesis{
		AgentID: "agent-2", PlatformExperimentID: pe, Text: "warmup with cosine decay",
		Links:        []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}, {Type: domain.LinkBuildsOn, To: b.ID}, {Type: domain.LinkRelated, To: a.ID}},
		DistinctFrom: []string{b.ID},
	})
	if err != nil || existed {
		t.Fatalf("Register: got = (%v, existed=%v), want (nil, false)", err, existed)
	}
	if h.Status != domain.HypothesisOpen || h.AgentID != "agent-2" || h.PlatformExperimentID != pe {
		t.Errorf("hypothesis: got = %+v, want an open agent-2 hypothesis in %s", h, pe)
	}
	got := map[string]bool{}
	for _, l := range links {
		got[string(l.Type)+">"+l.HypothesisID] = true
		if !l.CreatedAt.Equal(h.CreatedAt) || l.AgentID != "agent-2" {
			t.Errorf("link %+v: want created with the hypothesis by agent-2", l)
		}
	}
	for _, want := range []string{"builds_on>" + a.ID, "builds_on>" + b.ID, "related>" + a.ID} {
		if !got[want] {
			t.Errorf("outgoing links: got = %v, want to include %s", got, want)
		}
	}
}

func similarIDs(t *testing.T, err error) map[string]string {
	t.Helper()
	var similar *SimilarError
	if !errors.As(err, &similar) {
		t.Fatalf("Register: got = %v, want a *SimilarError", err)
	}
	ids := map[string]string{}
	for _, h := range similar.Similar {
		ids[h.ID] = h.Reason
	}
	return ids
}

func TestRegisterHoldsBackAClaimUntilItIsToldApartFromCloseOnes(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	bs8 := register(t, s, pe, "agent-1", "batch size 8 beats batch size 32 at the same epoch budget", onRoot(pe))
	register(t, s, pe, "agent-1", "label noise caps validation accuracy", onRoot(pe))

	// A refinement reads close to the parent it names, and that is no reason to hold it.
	bs4, _, existed, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-2", PlatformExperimentID: pe,
		Text: "batch size 4 beats batch size 8 at the same epoch budget", Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: bs8.ID}}})
	if err != nil || existed {
		t.Fatalf("Register on a close parent: got = (%v, existed=%v), want (nil, false)", err, existed)
	}

	contra := NewHypothesis{AgentID: "agent-3", PlatformExperimentID: pe,
		Text: "batch size 8 does not beat batch size 32 at the same epoch budget", Links: onRoot(pe)}
	_, _, _, _, err = s.Register(ctx, contra)
	if got := similarIDs(t, err); len(got) != 2 || got[bs8.ID] != "close_text" || got[bs4.ID] != "close_text" {
		t.Fatalf("similar: got = %v, want %s and %s as close_text", got, bs8.ID, bs4.ID)
	}
	if stats, _ := s.HypothesisStats(ctx, ListFilter{PlatformExperimentID: pe}); stats.Total != 3 {
		t.Fatalf("hypotheses after a held-back registration: got = %d, want 3", stats.Total)
	}
	contra.Links = append(contra.Links, domain.LinkSpec{Type: domain.LinkContradicts, To: bs8.ID})
	contra.DistinctFrom = []string{bs4.ID}
	if _, _, existed, _, err := s.Register(ctx, contra); err != nil || existed {
		t.Fatalf("Register with a contradicts link: got = (%v, existed=%v), want (nil, false)", err, existed)
	}
}

func TestRegisterHoldsBackAClaimBuildingOnARefutedOne(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	parent := register(t, s, pe, "agent-1", "learning rate 0.2 overshoots", onRoot(pe))
	if _, _, err := s.UpdateStatus(ctx, parent.ID, "agent-1", agentActor("agent-1"), domain.HypothesisRefuted, stampAt(1)); err != nil {
		t.Fatal(err)
	}
	child := NewHypothesis{AgentID: "agent-1", PlatformExperimentID: pe,
		Text: "a larger step wins within forty epochs", Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: parent.ID}}}
	_, _, _, _, err := s.Register(ctx, child)
	if got := similarIDs(t, err); len(got) != 1 || got[parent.ID] != "refuted_parent" {
		t.Fatalf("similar: got = %v, want %s as refuted_parent", got, parent.ID)
	}
	child.Links = append(child.Links, domain.LinkSpec{Type: domain.LinkContradicts, To: parent.ID})
	if _, _, existed, _, err := s.Register(ctx, child); err != nil || existed {
		t.Fatalf("Register with a contradicts link: got = (%v, existed=%v), want (nil, false)", err, existed)
	}
}

// The dedup rule from the Postgres days, unchanged: the same claim twice in one experiment is one
// hypothesis, and a duplicate does not get to re-place it.
func TestRegisteringAnEquivalentClaimReturnsTheExistingHypothesisWithoutApplyingItsLinks(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "Warmup helps", onRoot(pe))
	other := register(t, s, pe, "agent-1", "something else", onRoot(pe))

	h, links, existed, _, err := s.Register(context.Background(), NewHypothesis{
		Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: "  warmup   HELPS ",
		Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: other.ID}},
	})
	if err != nil || !existed || h.ID != a.ID {
		t.Fatalf("Register duplicate: got = (%v, existed=%v, id=%v), want (nil, true, %v)", err, existed, h.ID, a.ID)
	}
	if len(links) != 1 || links[0].HypothesisID != pe {
		t.Errorf("links of the existing hypothesis: got = %+v, want only builds_on the objective", links)
	}

	elsewhere := testExperiment(t, s)
	register(t, s, elsewhere, "agent-1", "warmup helps", onRoot(elsewhere))
}

// Two agents registering the same idea at the same moment must both end up holding the one
// hypothesis, as Postgres' ON CONFLICT path guaranteed.
func TestConcurrentRegistrationsOfTheSameClaimYieldOneHypothesis(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	created := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, _, existed, _, err := s.Register(context.Background(), NewHypothesis{
				AgentID: fmt.Sprintf("agent-%d", i), PlatformExperimentID: pe,
				Text: "the same idea", Links: onRoot(pe),
			})
			errs[i] = err
			if err == nil {
				ids[i], created[i] = h.ID, !existed
			}
		}()
	}
	wg.Wait()
	newCount := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Register %d: got = %v, want nil", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("Register %d id: got = %v, want %v", i, ids[i], ids[0])
		}
		if created[i] {
			newCount++
		}
	}
	if newCount != 1 {
		t.Errorf("registrations that created the hypothesis: got = %v, want 1", newCount)
	}
	if stats, err := s.HypothesisStats(context.Background(), ListFilter{PlatformExperimentID: pe}); err != nil || stats.Total != 1 {
		t.Errorf("hypotheses in the experiment: got = (%v, %v), want (1, nil)", stats, err)
	}
}

func TestRegisterRefusesAHypothesisWithNoPlaceInTheGraph(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "warmup helps", onRoot(pe))
	elsewhere := testExperiment(t, s)
	foreign := register(t, s, elsewhere, "agent-1", "foreign idea", onRoot(elsewhere))

	for name, links := range map[string][]domain.LinkSpec{
		"no links":                nil,
		"no builds_on":            {{Type: domain.LinkRelated, To: a.ID}},
		"unknown builds_on":       {{Type: domain.LinkBuildsOn, To: "no-such-hypothesis"}},
		"cross-experiment target": {{Type: domain.LinkBuildsOn, To: foreign.ID}},
		"root as related":         {{Type: domain.LinkBuildsOn, To: pe}, {Type: domain.LinkRelated, To: pe}},
		"bad type":                {{Type: "extends", To: pe}},
	} {
		_, _, _, _, err := s.Register(context.Background(), NewHypothesis{
			AgentID: "agent-1", PlatformExperimentID: pe, Text: "new idea " + name, Links: links,
		})
		if !errors.Is(err, ErrInvalidLink) {
			t.Errorf("%s: got = %v, want %v", name, err, ErrInvalidLink)
		}
	}
	if stats, _ := s.HypothesisStats(context.Background(), ListFilter{PlatformExperimentID: pe}); stats.Total != 1 {
		t.Errorf("hypotheses after refused registrations: got = %v, want 1", stats.Total)
	}
}

func TestAddLinkRefusesCyclesSelfLinksAndOtherExperiments(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "a", onRoot(pe))
	b := register(t, s, pe, "agent-1", "b", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}})
	c := register(t, s, pe, "agent-1", "c", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: b.ID}})
	elsewhere := testExperiment(t, s)
	foreign := register(t, s, elsewhere, "agent-1", "foreign", onRoot(elsewhere))
	ctx := context.Background()

	for name, spec := range map[string]struct {
		from string
		link domain.LinkSpec
	}{
		"direct cycle":     {a.ID, domain.LinkSpec{Type: domain.LinkBuildsOn, To: b.ID}},
		"indirect cycle":   {a.ID, domain.LinkSpec{Type: domain.LinkBuildsOn, To: c.ID}},
		"self link":        {a.ID, domain.LinkSpec{Type: domain.LinkRelated, To: a.ID}},
		"other experiment": {a.ID, domain.LinkSpec{Type: domain.LinkRelated, To: foreign.ID}},
		"unknown target":   {a.ID, domain.LinkSpec{Type: domain.LinkContradicts, To: "nope"}},
	} {
		if _, err := s.AddLink(ctx, spec.from, "agent-1", agentActor("agent-1"), spec.link, stampAt(1)); !errors.Is(err, ErrInvalidLink) {
			t.Errorf("%s: got = %v, want %v", name, err, ErrInvalidLink)
		}
	}
	if _, err := s.AddLink(ctx, "nope", "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkRelated, To: a.ID}, stampAt(1)); !errors.Is(err, ErrNotFound) {
		t.Errorf("link from an unknown hypothesis: got = %v, want %v", err, ErrNotFound)
	}

	// The reverse direction of a cycle is fine as a non-builds_on link, and a second builds_on parent
	// that closes no loop is a combination.
	if _, err := s.AddLink(ctx, a.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkContradicts, To: c.ID}, stampAt(1)); err != nil {
		t.Errorf("contradicts back up the chain: got = %v, want nil", err)
	}
	d := register(t, s, pe, "agent-1", "d", onRoot(pe))
	if _, err := s.AddLink(ctx, c.ID, "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: d.ID}, stampAt(1)); err != nil {
		t.Errorf("second builds_on parent: got = %v, want nil", err)
	}
}

// Same ownership rule as status: an agent's hypothesis is placed only by that agent, a human's by
// anyone. A repeated link is a no-op the caller can see.
func TestAddLinkFollowsTheOwnershipRuleAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	owned := register(t, s, pe, "agent-1", "owned", onRoot(pe))
	target := register(t, s, pe, "agent-2", "target", onRoot(pe))
	human, _, _, _, err := s.Register(ctx, NewHypothesis{Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: "human idea", Links: onRoot(pe)})
	if err != nil {
		t.Fatalf("Register human: got = %v, want nil", err)
	}
	link := domain.LinkSpec{Type: domain.LinkRelated, To: target.ID}

	if _, err := s.AddLink(ctx, owned.ID, "agent-2", agentActor("agent-2"), link, stampAt(1)); !errors.Is(err, ErrNotOwner) {
		t.Errorf("link from another agent's hypothesis: got = %v, want %v", err, ErrNotOwner)
	}
	if _, err := s.AddLink(ctx, owned.ID, "", userActor("user-ada"), link, stampAt(1)); !errors.Is(err, ErrNotOwner) {
		t.Errorf("human linking an agent's hypothesis: got = %v, want %v", err, ErrNotOwner)
	}
	first, err := s.AddLink(ctx, human.ID, "agent-2", agentActor("agent-2"), link, stampAt(1))
	if err != nil || !first.Created || first.CreatedAt.IsZero() {
		t.Fatalf("link from a human hypothesis: got = (%+v, %v), want a new link", first, err)
	}
	again, err := s.AddLink(ctx, human.ID, "agent-1", agentActor("agent-1"), link, stampAt(1))
	if err != nil || again.Created {
		t.Fatalf("repeated link: got = (%+v, %v), want an unchanged no-op", again, err)
	}
	if len(again.Out) != 2 {
		t.Errorf("outgoing links after a repeat: got = %v, want 2 (objective and target)", len(again.Out))
	}
}

func TestListAndLinksShowWhereEachHypothesisSits(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	a := register(t, s, pe, "agent-1", "a", onRoot(pe))
	b := register(t, s, pe, "agent-2", "b", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}})
	register(t, s, pe, "agent-2", "c", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}, {Type: domain.LinkContradicts, To: b.ID}})

	items, err := s.ListHypotheses(ctx, ListFilter{PlatformExperimentID: pe})
	if err != nil {
		t.Fatalf("ListHypotheses: got = %v, want nil", err)
	}
	if len(items) != 3 || items[2].ID != a.ID {
		t.Fatalf("list: got %d items, want 3 newest first ending with %s", len(items), a.ID)
	}
	if items[2].ChildCount != 2 || len(items[2].BuildsOn) != 1 || items[2].BuildsOn[0] != pe {
		t.Errorf("a: got child_count=%v builds_on=%v, want 2 and [%s]", items[2].ChildCount, items[2].BuildsOn, pe)
	}
	if items[0].ChildCount != 0 || len(items[0].BuildsOn) != 1 || items[0].BuildsOn[0] != a.ID {
		t.Errorf("c: got child_count=%v builds_on=%v, want 0 and [%s]", items[0].ChildCount, items[0].BuildsOn, a.ID)
	}
	page, err := s.ListHypotheses(ctx, ListFilter{PlatformExperimentID: pe, AgentID: "agent-2", Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != b.ID {
		t.Errorf("agent-2 page 2: got = (%v, %v), want [%s]", page, err, b.ID)
	}

	links, err := s.Links(ctx, a.ID)
	if err != nil {
		t.Fatalf("Links: got = %v, want nil", err)
	}
	if len(links.Out) != 1 || links.Out[0].HypothesisID != pe || links.Out[0].Text != "" {
		t.Errorf("a out: got = %+v, want builds_on the objective", links.Out)
	}
	if len(links.In) != 2 || links.In[0].Text != "b" || links.In[0].Type != domain.LinkBuildsOn {
		t.Errorf("a in: got = %+v, want b then c building on it", links.In)
	}

	nodes, edges, err := s.ExperimentGraph(ctx, pe)
	if err != nil || len(nodes) != 3 || len(edges) != 4 {
		t.Errorf("graph: got = (%d nodes, %d edges, %v), want (3, 4, nil)", len(nodes), len(edges), err)
	}
}

// The UI and agents show who made a link; a human's link carries its author, never an empty owner.
func TestLinksAndGraphEdgesCarryWhoMadeThemAndWhen(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	a := register(t, s, pe, "agent-1", "a", onRoot(pe))
	human, _, _, _, err := s.Register(ctx, NewHypothesis{Actor: userActor("user-dana"),
		PlatformExperimentID: pe, Text: "human idea", Links: onRoot(pe)})
	if err != nil {
		t.Fatalf("Register human: got = %v, want nil", err)
	}
	linked, err := s.AddLink(ctx, human.ID, "", userActor("user-dana"), domain.LinkSpec{Type: domain.LinkRelated, To: a.ID}, stampAt(1))
	if err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}

	in, err := s.Links(ctx, a.ID)
	if err != nil || len(in.In) != 1 {
		t.Fatalf("Links: got = (%+v, %v), want one incoming", in.In, err)
	}
	if got := in.In[0]; got.AgentID != "" || got.Actor != userActor("user-dana") || !got.CreatedAt.Equal(linked.CreatedAt) {
		t.Errorf("a person's link: got = (%q, %+v, %v), want no agent, user-dana, %v", got.AgentID, got.Actor, got.CreatedAt, linked.CreatedAt)
	}
	// A key registers and links for an agent: the agent owns both, and the key is kept as who did it.
	key := domain.Actor{Kind: domain.ActorKey, ID: "key-1"}
	forAgent, out, _, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-2", Actor: key, PlatformExperimentID: pe, Text: "for the agent", Links: onRoot(pe)})
	if err != nil || forAgent.AgentID != "agent-2" || forAgent.Source != domain.HypothesisSourceAgent || forAgent.Actor != key || len(out) != 1 || out[0].Actor != key {
		t.Fatalf("a key registering for an agent: got = %+v with links %+v, %v, want agent-2's by key-1", forAgent, out, err)
	}
	if human.AgentID != "" || human.Source != domain.HypothesisSourceHuman || human.Actor != userActor("user-dana") {
		t.Errorf("a person's hypothesis: got = %+v, want no agent, source human, by user-dana", human)
	}
	if read, err := s.GetHypothesis(ctx, forAgent.ID); err != nil || read.Actor != key {
		t.Errorf("read back: got = %+v, %v, want actor key-1", read, err)
	}

	nodes, edges, err := s.ExperimentGraph(ctx, pe)
	if err != nil {
		t.Fatalf("ExperimentGraph: got = %v, want nil", err)
	}
	wantNodes := map[string]domain.Actor{a.ID: agentActor("agent-1"), human.ID: userActor("user-dana"), forAgent.ID: key}
	for _, n := range nodes {
		if n.Actor != wantNodes[n.ID] {
			t.Errorf("node %s: got actor %+v, want %+v", n.ID, n.Actor, wantNodes[n.ID])
		}
	}
	for _, e := range edges {
		if e.Actor != wantNodes[e.From] || e.CreatedAt.IsZero() {
			t.Errorf("edge %s -%s-> %s: got actor %+v at %v, want %+v and a time", e.From, e.Type, e.To, e.Actor, e.CreatedAt, wantNodes[e.From])
		}
	}
}

// A node and a link from before the credential was kept carry no actor. They read as unverified
// under the name they stated: the author, or else the agent.
func TestAHypothesisFromBeforeActorsWereKeptReadsAsUnverified(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	if err := s.ensureObjective(ctx, pe); err != nil {
		t.Fatalf("ensureObjective: %v", err)
	}
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (o:Objective {id: $pe})
CREATE (a:Hypothesis {id: $pe + '-agent', platform_experiment_id: $pe, agent_id: 'agent-1', source: 'agent', author: '',
  text: 'an old agent idea', normalized_text: 'an old agent idea', status: 'open', created_at: $at, created_xid: 1})
CREATE (p:Hypothesis {id: $pe + '-person', platform_experiment_id: $pe, agent_id: '', source: 'human', author: 'Grace',
  text: 'an old human idea', normalized_text: 'an old human idea', status: 'open', created_at: $at, created_xid: 1})
CREATE (a)-[:BUILDS_ON {agent_id: 'agent-1', author: '', created_at: $at, created_xid: 1}]->(o)
CREATE (p)-[:BUILDS_ON {agent_id: '', author: 'Grace', created_at: $at, created_xid: 1}]->(o)`, map[string]any{"pe": pe, "at": now()})
	})
	if err != nil {
		t.Fatalf("write old rows: %v", err)
	}
	if _, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (a:Hypothesis {id: $pe + '-agent'}) SET a.status = 'refuted', a.status_changed_at = $at, a.status_changed_xid = 2`,
			map[string]any{"pe": pe, "at": now()})
	}); err != nil {
		t.Fatalf("write an old verdict: %v", err)
	}
	ruled, err := s.GetHypothesis(ctx, pe+"-agent")
	if err != nil || ruled.StatusActor == nil || *ruled.StatusActor != (domain.Actor{Kind: domain.ActorUnverified, Name: "agent-1"}) {
		t.Errorf("an old verdict: got = %+v, %v, want an unverified status actor named by the owning agent", ruled, err)
	}
	if never, err := s.GetHypothesis(ctx, pe+"-person"); err != nil || never.StatusActor != nil {
		t.Errorf("a hypothesis never ruled on: got status actor %+v, %v, want none", never.StatusActor, err)
	}
	want := map[string]domain.Actor{
		pe + "-agent":  {Kind: domain.ActorUnverified, Name: "agent-1"},
		pe + "-person": {Kind: domain.ActorUnverified, Name: "Grace"},
	}
	nodes, edges, err := s.ExperimentGraph(ctx, pe)
	if err != nil || len(nodes) != 2 || len(edges) != 2 {
		t.Fatalf("ExperimentGraph: got = %d nodes, %d edges, %v, want 2 and 2", len(nodes), len(edges), err)
	}
	for _, n := range nodes {
		if n.Actor != want[n.ID] {
			t.Errorf("node %s: got actor %+v, want %+v", n.ID, n.Actor, want[n.ID])
		}
		links, err := s.Links(ctx, n.ID)
		if err != nil || len(links.Out) != 1 || links.Out[0].Actor != want[n.ID] {
			t.Errorf("links of %s: got = %+v, %v, want one by %+v", n.ID, links.Out, err, want[n.ID])
		}
	}
	for _, e := range edges {
		if e.Actor != want[e.From] {
			t.Errorf("edge from %s: got actor %+v, want %+v", e.From, e.Actor, want[e.From])
		}
	}
	_, _, _, _, err = s.Register(ctx, NewHypothesis{AgentID: "agent-2", Actor: agentActor("agent-2"), PlatformExperimentID: pe,
		Text: "an old human idea, restated", Links: onRoot(pe)})
	var similar *SimilarError
	if !errors.As(err, &similar) || len(similar.Similar) == 0 {
		t.Fatalf("a registration close to old ones: got = %v, want it held back", err)
	}
	for _, close := range similar.Similar {
		if close.Actor != want[close.ID] {
			t.Errorf("similar %s: got actor %+v, want %+v", close.ID, close.Actor, want[close.ID])
		}
	}
}

// The accounts the retired human_writes table held reach the old records it names, and nothing
// else: a record that already has an actor keeps it, and one it does not name stays unverified.
func TestCreditUsersGivesOldRecordsTheAccountKeptForThem(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	if err := s.ensureObjective(ctx, pe); err != nil {
		t.Fatalf("ensureObjective: %v", err)
	}
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (o:Objective {id: $pe})
CREATE (p:Hypothesis {id: $pe + '-person', platform_experiment_id: $pe, agent_id: '', source: 'human', author: 'Ada Lovelace',
  text: 'an old human idea', normalized_text: 'an old human idea', status: 'open', created_at: $at, created_xid: 1})
CREATE (q:Hypothesis {id: $pe + '-typed', platform_experiment_id: $pe, agent_id: '', source: 'human', author: 'Mallory',
  text: 'a typed name', normalized_text: 'a typed name', status: 'open', created_at: $at, created_xid: 1})
CREATE (p)-[:BUILDS_ON {agent_id: '', author: 'Ada Lovelace', created_at: $at, created_xid: 1}]->(o)
CREATE (q)-[:BUILDS_ON {agent_id: '', author: 'Mallory', created_at: $at, created_xid: 1}]->(o)
CREATE (p)-[:RELATED {agent_id: '', author: 'Ada Lovelace', created_at: $later, created_xid: 2}]->(q)
CREATE (q)-[:RELATED {agent_id: '', author: 'Mallory', created_at: $later, created_xid: 2}]->(p)`,
			map[string]any{"pe": pe, "at": now(), "later": now().Add(time.Minute)})
	})
	if err != nil {
		t.Fatalf("write old rows: %v", err)
	}
	fresh := register(t, s, pe, "agent-1", "a new one entirely", onRoot(pe))
	writes := []db.HumanWrite{
		{UserID: "user-ada", Kind: "hypothesis", Subject: pe + "-person"},
		{UserID: "user-ada", Kind: "link", Subject: pe + "-person related " + pe + "-typed"},
		{UserID: "user-ada", Kind: "hypothesis", Subject: fresh.ID},
		{UserID: "user-ada", Kind: "hypothesis", Subject: "one-that-is-gone"},
	}
	for range 2 {
		if err := s.CreditUsers(ctx, writes); err != nil {
			t.Fatalf("CreditUsers: got = %v, want nil", err)
		}
	}
	ada, typed := userActor("user-ada"), domain.Actor{Kind: domain.ActorUnverified, Name: "Mallory"}
	wantNodes := map[string]domain.Actor{pe + "-person": ada, pe + "-typed": typed, fresh.ID: agentActor("agent-1")}
	nodes, edges, err := s.ExperimentGraph(ctx, pe)
	if err != nil || len(nodes) != 3 || len(edges) != 5 {
		t.Fatalf("ExperimentGraph: got = %d nodes, %d edges, %v, want 3 and 5", len(nodes), len(edges), err)
	}
	for _, n := range nodes {
		if n.Actor != wantNodes[n.ID] {
			t.Errorf("node %s: got actor %+v, want %+v", n.ID, n.Actor, wantNodes[n.ID])
		}
	}
	for _, e := range edges {
		if e.Actor != wantNodes[e.From] {
			t.Errorf("edge %s -%s-> %s: got actor %+v, want %+v", e.From, e.Type, e.To, e.Actor, wantNodes[e.From])
		}
	}
	for _, bad := range []db.HumanWrite{{Kind: "link", Subject: "two parts"}, {Kind: "link", Subject: "a likes b"}, {Kind: "comment", Subject: "c"}} {
		if err := s.CreditUsers(ctx, []db.HumanWrite{bad}); err == nil {
			t.Errorf("CreditUsers of %+v: got = nil, want an error", bad)
		}
	}
}

func agentActor(id string) domain.Actor { return domain.Actor{Kind: domain.ActorAgent, ID: id} }
func userActor(id string) domain.Actor  { return domain.Actor{Kind: domain.ActorUser, ID: id} }

func TestUpdateStatusFollowsTheOwnershipRuleAndReportsChange(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	h := register(t, s, pe, "agent-1", "a", onRoot(pe))

	if _, _, err := s.UpdateStatus(ctx, h.ID, "agent-2", agentActor("agent-2"), domain.HypothesisRefuted, stampAt(1)); !errors.Is(err, ErrNotOwner) {
		t.Errorf("non-owner: got = %v, want %v", err, ErrNotOwner)
	}
	if _, _, err := s.UpdateStatus(ctx, "nope", "agent-1", agentActor("agent-1"), domain.HypothesisRefuted, stampAt(1)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: got = %v, want %v", err, ErrNotFound)
	}
	got, changed, err := s.UpdateStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), domain.HypothesisRefuted, stampAt(1))
	if err != nil || !changed || got.Status != domain.HypothesisRefuted || got.StatusChangedAt == nil {
		t.Fatalf("owner: got = (%+v, %v, %v), want refuted with status_changed_at", got, changed, err)
	}
	if h.StatusActor != nil || got.StatusActor == nil || *got.StatusActor != agentActor("agent-1") {
		t.Fatalf("who gave the verdict: got = %+v before and %+v after, want nobody and then agent-1", h.StatusActor, got.StatusActor)
	}
	// Saying again what it already is changes nothing, whoever says it.
	again, changed, err := s.UpdateStatus(ctx, h.ID, "agent-1", userActor("user-ada"), domain.HypothesisRefuted, stampAt(1))
	if err != nil || changed || *again.StatusActor != agentActor("agent-1") {
		t.Errorf("same status again: got = (%+v, %v, %v), want it unchanged and still agent-1's verdict", again.StatusActor, changed, err)
	}
	// A person rules for the owning agent: the verdict is the agent's and the person gave it.
	reopened, _, err := s.UpdateStatus(ctx, h.ID, "agent-1", userActor("user-ada"), domain.HypothesisInconclusive, stampAt(1))
	if err != nil || reopened.AgentID != "agent-1" || *reopened.StatusActor != userActor("user-ada") {
		t.Errorf("a person ruling for the agent: got = %+v, %v, want agent-1's hypothesis with user-ada as status actor", reopened, err)
	}
	if read, err := s.GetHypothesis(ctx, h.ID); err != nil || read.StatusActor == nil || *read.StatusActor != userActor("user-ada") || read.Actor != agentActor("agent-1") {
		t.Errorf("read back: got = %+v, %v, want registered by agent-1 and ruled by user-ada", read, err)
	}
	// Nobody owns this one, so anyone settles it, naming no agent.
	unowned, _, _, _, err := s.Register(ctx, NewHypothesis{Actor: userActor("user-ada"), PlatformExperimentID: pe, Text: "nobody's", Links: onRoot(pe)})
	if err != nil {
		t.Fatalf("Register unowned: %v", err)
	}
	key := domain.Actor{Kind: domain.ActorKey, ID: "key-1"}
	if settled, _, err := s.UpdateStatus(ctx, unowned.ID, "", key, domain.HypothesisInconclusive, stampAt(1)); err != nil || *settled.StatusActor != key {
		t.Errorf("settling an unowned hypothesis as nobody: got = %+v, %v, want key-1 as status actor", settled, err)
	}
	if _, _, err := s.UpdateStatus(ctx, h.ID, "", key, domain.HypothesisOpen, stampAt(1)); !errors.Is(err, ErrNotOwner) {
		t.Errorf("settling an agent's hypothesis as nobody: got = %v, want %v", err, ErrNotOwner)
	}

	register(t, s, pe, "agent-1", "b", onRoot(pe))
	stats, err := s.HypothesisStats(ctx, ListFilter{PlatformExperimentID: pe})
	if err != nil || stats.Total != 3 || len(stats.ByStatus) != 2 ||
		stats.ByStatus[domain.HypothesisInconclusive] != 2 || stats.ByStatus[domain.HypothesisOpen] != 1 {
		t.Errorf("HypothesisStats: got = (%+v, %v), want total 3 with 2 inconclusive and 1 open", stats, err)
	}
}

func TestTheTextIndexRanksHypothesesThroughTheQueryEndpoint(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	register(t, s, pe, "agent-1", "Linear warmup stabilises early training", onRoot(pe))
	register(t, s, pe, "agent-1", "Label smoothing reduces overconfidence", onRoot(pe))

	result, err := s.Query(context.Background(), pe, `CALL text_search.search_all('`+TextIndex+`', 'warmup training') YIELD node, score
WHERE node.platform_experiment_id = $pe RETURN node.text, score ORDER BY score DESC`, nil)
	if err != nil {
		t.Fatalf("Query: got = %v, want nil", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "Linear warmup stabilises early training" {
		t.Errorf("search rows: got = %v, want the warmup training hypothesis only", result.Rows)
	}
}

func count(t *testing.T, s *Store, cypher string) int64 {
	t.Helper()
	out, err := s.read(context.Background(), func(tx neo4j.ManagedTransaction) (any, error) {
		records, err := collect(context.Background(), tx, cypher, nil)
		if err != nil {
			return nil, err
		}
		return records[0].Values[0], nil
	})
	if err != nil {
		t.Fatalf("count %q: %v", cypher, err)
	}
	return out.(int64)
}

// The query endpoint gives agents native Cypher, so read-only has to be a property of how it runs,
// not of what agents promise to send. Memgraph refuses these in a read-mode transaction; the
// rollback behind it would discard them if it ever stopped doing so.
func TestAQueryCannotLeaveATraceInTheGraph(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	h := register(t, s, pe, "agent-1", "a", onRoot(pe))
	ctx := context.Background()

	for _, q := range []string{
		`CREATE (:QueryTrace {pe: '` + pe + `'})`,
		`MERGE (:QueryTrace {pe: '` + pe + `'}) RETURN 1`,
		`MATCH (h:Hypothesis {id: '` + h.ID + `'}) SET h.status = 'confirmed', h:QueryTrace RETURN h`,
		`MATCH (h:Hypothesis {id: '` + h.ID + `'}) DETACH DELETE h`,
		`MATCH (o:Objective {id: '` + pe + `'}) CREATE (o)-[:QUERY_TRACE]->(o)`,
	} {
		if _, err := s.Query(ctx, pe, q, nil); !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("Query %q: got = %v, want %v saying the endpoint is read-only", q, err, ErrInvalidQuery)
		}
	}
	if n := count(t, s, `MATCH (n:QueryTrace) RETURN count(n)`); n != 0 {
		t.Errorf("QueryTrace nodes after the queries: got = %v, want 0", n)
	}
	if n := count(t, s, `MATCH ()-[r:QUERY_TRACE]->() RETURN count(r)`); n != 0 {
		t.Errorf("QUERY_TRACE edges after the queries: got = %v, want 0", n)
	}
	if got, _ := s.GetHypothesis(ctx, h.ID); got == nil || got.Status != domain.HypothesisOpen {
		t.Errorf("hypothesis after the queries: got = %+v, want it still there and open", got)
	}
}

func TestAQueryRefusesWhatARollbackCannotUndo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, q := range []string{
		"CREATE INDEX ON :Hypothesis(text)",
		"DROP CONSTRAINT ON (h:Hypothesis) ASSERT h.id IS UNIQUE",
		"DROP TEXT INDEX " + TextIndex,
		"CREATE USER intruder",
		"STORAGE MODE IN_MEMORY_ANALYTICAL",
		"CREATE TRIGGER t ON CREATE BEFORE COMMIT EXECUTE CREATE (:QueryTrace)",
		"DROP GRAPH",
		"FREE MEMORY",
		"CREATE SNAPSHOT",
		"LOAD CSV FROM '/etc/passwd' NO HEADER AS row RETURN row",
		"load /* split */ csv FROM '/etc/passwd' NO HEADER AS row RETURN row",
		"USING PERIODIC COMMIT 1 UNWIND range(1, 3) AS i CREATE (:QueryTrace {i: i})",
		"UNWIND range(1, 3) AS i CALL { WITH i CREATE (:QueryTrace {i: i}) } IN TRANSACTIONS OF 1 ROWS",
		"CALL mg.create_module_file('trace.py', 'import mgp') YIELD path RETURN path",
		"CALL `mg`.`create_module_file`('trace.py', 'import mgp') YIELD path RETURN path",
		"CALL `mg.create_module_file`('trace.py', 'import mgp') YIELD path RETURN path",
		"CALL mg /* x */ . create_module_file('trace.py', 'import mgp') YIELD path RETURN path",
		"CALL mg.load_all()",
		"RETURN 1; CREATE (:QueryTrace)",
	} {
		if _, err := s.Query(ctx, "pe-1", q, nil); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("Query %q: got = %v, want %v", q, err, ErrInvalidQuery)
		}
	}
	if n := count(t, s, `MATCH (n:QueryTrace) RETURN count(n)`); n != 0 {
		t.Errorf("QueryTrace nodes after refused queries: got = %v, want 0", n)
	}
	files, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `CALL mg.get_module_files() YIELD path WHERE path CONTAINS 'trace.py' RETURN path`, nil)
	})
	if err != nil || len(files.([]*neo4j.Record)) != 0 {
		t.Errorf("module files named trace.py: got = (%v, %v), want none", files, err)
	}
}

func TestAQueryMapsGraphValuesAndCapsRows(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "a", onRoot(pe))
	register(t, s, pe, "agent-1", "b", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}})
	ctx := context.Background()

	result, err := s.Query(ctx, pe, `MATCH (b:Hypothesis {text: 'b', platform_experiment_id: $pe})-[r:BUILDS_ON]->(a) RETURN b, r, a.id AS parent`, nil)
	if err != nil {
		t.Fatalf("Query: got = %v, want nil", err)
	}
	if strings.Join(result.Columns, ",") != "b,r,parent" || len(result.Rows) != 1 {
		t.Fatalf("result: got = %+v, want one row of b,r,parent", result)
	}
	node := result.Rows[0][0].(map[string]any)
	rel := result.Rows[0][1].(map[string]any)
	if node["text"] != "b" || fmt.Sprint(node["_labels"]) != "[Hypothesis]" {
		t.Errorf("node: got = %v, want b's properties with _labels", node)
	}
	if rel["_type"] != "BUILDS_ON" || rel["_to"] != a.ID || rel["_from"] == "" || rel["agent_id"] != "agent-1" {
		t.Errorf("relationship: got = %v, want BUILDS_ON from b to %s", rel, a.ID)
	}

	capped, err := s.Query(ctx, "pe-1", `UNWIND range(1, $n) AS i RETURN i`, map[string]any{"n": float64(QueryRowCap + 5)})
	if err != nil || len(capped.Rows) != QueryRowCap || !capped.Truncated {
		t.Errorf("capped query: got = (%d rows, truncated=%v, %v), want (%d, true, nil)", len(capped.Rows), capped != nil && capped.Truncated, err, QueryRowCap)
	}
	// Relationships in the capped rows force the endpoint lookup, a second query in the same
	// transaction, while the first result still has rows unread.
	cappedRels, err := s.Query(ctx, pe, `MATCH (b:Hypothesis {text: 'b', platform_experiment_id: $pe})-[r:BUILDS_ON]->() UNWIND range(1, $n) AS i RETURN r`, map[string]any{"n": float64(QueryRowCap + 5)})
	if err != nil || len(cappedRels.Rows) != QueryRowCap || !cappedRels.Truncated {
		t.Fatalf("capped relationship query: got = (%+v, %v), want (%d rows, truncated, nil)", cappedRels, err, QueryRowCap)
	}
	if rel := cappedRels.Rows[0][0].(map[string]any); rel["_to"] != a.ID {
		t.Errorf("capped relationship: got = %v, want its endpoints resolved to %s", rel, a.ID)
	}
	if literal, err := s.Query(ctx, "pe-1", `RETURN 'LOAD CSV and CALL mg.load_all()' AS s // CALL mg.load_all()`, nil); err != nil || literal.Rows[0][0] != "LOAD CSV and CALL mg.load_all()" {
		t.Errorf("keywords inside a string and a comment: got = (%v, %v), want the string back", literal, err)
	}
	if _, err := s.Query(ctx, "pe-1", `MATCH (n RETURN n`, nil); !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "mismatched input") {
		t.Errorf("syntax error: got = %v, want %v with Memgraph's message", err, ErrInvalidQuery)
	}
}

func TestAQueryIsStoppedAtItsTimeout(t *testing.T) {
	s := testStore(t)
	start := time.Now()
	_, err := s.Query(context.Background(), "pe-1", `UNWIND range(1, 40000) AS i UNWIND range(1, 40000) AS j WITH i, j WHERE i + j < 0 RETURN count(*)`, nil)
	if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("long query: got = %v, want %v naming the timeout", err, ErrInvalidQuery)
	}
	if elapsed := time.Since(start); elapsed > 2*QueryTimeout+time.Second {
		t.Errorf("long query ran for %v, want it stopped near %v", elapsed, QueryTimeout)
	}
}

func TestAQueryIsStoppedAtItsMemoryLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, err := s.Query(ctx, "pe-1", `UNWIND range(1, 100000000) AS i WITH collect(i) AS c RETURN size(c)`, nil)
	if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "Memory limit exceeded") {
		t.Fatalf("large query: got = %v, want %v naming the memory limit", err, ErrInvalidQuery)
	}
	for _, q := range []string{"RETURN 1;", "RETURN 1 // trailing comment"} {
		if _, err := s.Query(ctx, "pe-1", q, nil); err != nil {
			t.Errorf("Query(%q): got = %v, want nil", q, err)
		}
	}
	if _, err := s.Query(ctx, "pe-1", "RETURN 1 QUERY MEMORY UNLIMITED", nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("query lifting the limit: got = %v, want %v", err, ErrInvalidQuery)
	}
}

func TestAPropertyOfTheWrongTypeFailsTheRead(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	h := register(t, s, pe, "agent-a", "a claim whose timestamp gets broken", onRoot(pe))
	ctx := context.Background()
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.created_at = 'yesterday'`, map[string]any{"id": h.ID})
	})
	if err != nil {
		t.Fatalf("break created_at: got = %v, want nil", err)
	}
	if got, err := s.GetHypothesis(ctx, h.ID); err == nil || !strings.Contains(err.Error(), "want a timestamp, got string") {
		t.Errorf("GetHypothesis: got = %v, %v, want an error naming the wrong type", got, err)
	}
}

// Replay is what turns a dropped /watch connection into a delay rather than a gap. Hypothesis
// events are derived from the positions each graph write was stamped with, and links given at
// registration ride on hypothesis.new, so replay must not invent a hypothesis.link for them.
func TestReplayDerivesHypothesisEventsFromThePositionsTheGraphStores(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	registerAt := func(agentID, text string, links []domain.LinkSpec, position int64) *domain.Hypothesis {
		h, _, _, _, err := s.Register(ctx, NewHypothesis{AgentID: agentID,
			PlatformExperimentID: pe, Text: text, Links: links, Stamp: stampAt(position)})
		if err != nil {
			t.Fatalf("Register %q: got = %v, want nil", text, err)
		}
		return h
	}
	a := registerAt("agent-1", "a", onRoot(pe), 11)
	b := registerAt("agent-2", "b", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}}, 10)
	if _, err := s.AddLink(ctx, b.ID, "agent-2", agentActor("agent-2"), domain.LinkSpec{Type: domain.LinkContradicts, To: a.ID}, stampAt(12)); err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}
	if _, _, err := s.UpdateStatus(ctx, a.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed, stampAt(13)); err != nil {
		t.Fatalf("UpdateStatus: got = %v, want nil", err)
	}

	events, err := s.ReplayEvents(ctx, pe, 10)
	if err != nil {
		t.Fatalf("ReplayEvents: got = %v, want nil", err)
	}
	want := map[db.Event]bool{
		{Kind: db.EventHypothesisNew, Subject: a.ID, Value: "agent", PlatformExperimentID: pe, AgentID: "agent-1", Position: 11}:                      true,
		{Kind: db.EventHypothesisNew, Subject: b.ID, Value: "agent", PlatformExperimentID: pe, AgentID: "agent-2", Position: 10}:                      true,
		{Kind: db.EventHypothesisLink, Subject: b.ID, Value: "contradicts", Detail: a.ID, PlatformExperimentID: pe, AgentID: "agent-2", Position: 12}: true,
		{Kind: db.EventHypothesisStatus, Subject: a.ID, Value: "confirmed", PlatformExperimentID: pe, AgentID: "agent-1", Position: 13}:               true,
	}
	if len(events) != len(want) {
		t.Errorf("replayed events: got %d (%+v), want %d", len(events), events, len(want))
	}
	for _, e := range events {
		if !want[e] {
			t.Errorf("unexpected replayed event %+v", e)
		}
	}

	at, err := s.ReplayEvents(ctx, pe, 13)
	if err != nil || len(at) != 1 || at[0].Kind != db.EventHypothesisStatus {
		t.Errorf("replay from the last write's position: got = (%v, %v), want that write's hypothesis.status", at, err)
	}
	later, err := s.ReplayEvents(ctx, pe, 14)
	if err != nil || len(later) != 0 {
		t.Errorf("replay after the last write: got = (%v, %v), want nothing", later, err)
	}
}

// A write commits in the graph before its Postgres transaction does, so that transaction can still
// abort and its events go unsent. Repeating the write then announces again, at the repeat's
// position, which is also where replay finds it; repeating a write whose events were sent
// announces nothing.
func TestRepeatingAWriteWhoseTransactionAbortedAnnouncesItAgain(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	aborted := map[int64]bool{20: true}
	stamp := func(position int64) db.Stamp {
		return db.Stamp{Position: position, Aborted: func(p int64) (bool, error) { return aborted[p], nil }}
	}
	parent := register(t, s, pe, "agent-1", "warmup helps", onRoot(pe))
	n := NewHypothesis{AgentID: "agent-1", PlatformExperimentID: pe,
		Text: "batch size 512 beats 128 on the small model", Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: parent.ID}}}
	link := domain.LinkSpec{Type: domain.LinkRelated, To: parent.ID}

	n.Stamp = stamp(20)
	h, _, _, announce, err := s.Register(ctx, n)
	if err != nil || !announce {
		t.Fatalf("Register: got = (announce=%v, %v), want (true, nil)", announce, err)
	}
	if _, announce, err := s.UpdateStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed, stamp(20)); err != nil || !announce {
		t.Fatalf("UpdateStatus: got = (announce=%v, %v), want (true, nil)", announce, err)
	}
	if result, err := s.AddLink(ctx, h.ID, "agent-1", agentActor("agent-1"), link, stamp(20)); err != nil || !result.Announce {
		t.Fatalf("AddLink: got = (%+v, %v), want an announced link", result, err)
	}

	n.Stamp = stamp(21)
	if _, _, existed, announce, err := s.Register(ctx, n); err != nil || !existed || !announce {
		t.Errorf("Register again after an abort: got = (existed=%v, announce=%v, %v), want (true, true, nil)", existed, announce, err)
	}
	if _, announce, err := s.UpdateStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed, stamp(21)); err != nil || !announce {
		t.Errorf("UpdateStatus again after an abort: got = (announce=%v, %v), want (true, nil)", announce, err)
	}
	if result, err := s.AddLink(ctx, h.ID, "agent-1", agentActor("agent-1"), link, stamp(21)); err != nil || result.Created || !result.Announce {
		t.Errorf("AddLink again after an abort: got = (%+v, %v), want an announced existing link", result, err)
	}
	events, err := s.ReplayEvents(ctx, pe, 20)
	if err != nil || len(events) != 3 {
		t.Fatalf("ReplayEvents: got = (%+v, %v), want hypothesis.new, hypothesis.status and hypothesis.link", events, err)
	}
	for _, e := range events {
		if e.Position != 21 {
			t.Errorf("replayed %s: got = position %d, want 21, the repeat's", e.Kind, e.Position)
		}
	}

	n.Stamp = stamp(22)
	if _, _, _, announce, err := s.Register(ctx, n); err != nil || announce {
		t.Errorf("Register again after a commit: got = (announce=%v, %v), want (false, nil)", announce, err)
	}
	if _, announce, err := s.UpdateStatus(ctx, h.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed, stamp(22)); err != nil || announce {
		t.Errorf("UpdateStatus again after a commit: got = (announce=%v, %v), want (false, nil)", announce, err)
	}
	if result, err := s.AddLink(ctx, h.ID, "agent-1", agentActor("agent-1"), link, stamp(22)); err != nil || result.Announce {
		t.Errorf("AddLink again after a commit: got = (%+v, %v), want nothing announced", result, err)
	}
}

// A link given at registration rides on hypothesis.new, so re-adding it never announces a link,
// even when the registration's transaction aborted.
func TestReAddingARegistrationLinkAnnouncesNothing(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	h, _, _, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-1",
		PlatformExperimentID: pe, Text: "warmup helps", Links: onRoot(pe), Stamp: stampAt(30)})
	if err != nil {
		t.Fatalf("Register: got = %v, want nil", err)
	}
	abortedAll := db.Stamp{Position: 31, Aborted: func(int64) (bool, error) { return true, nil }}
	if result, err := s.AddLink(ctx, h.ID, "agent-1", agentActor("agent-1"), onRoot(pe)[0], abortedAll); err != nil || result.Announce {
		t.Errorf("AddLink of the registration link: got = (%+v, %v), want nothing announced", result, err)
	}
}

// A hypothesis written before writes carried a position must still be in a snapshot replay.
func TestEnsureSchemaGivesAHypothesisWrittenWithoutAPositionPositionZero(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	h := register(t, s, pe, "agent-1", "written before positions", onRoot(pe))
	ctx := context.Background()
	if _, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) REMOVE h.created_xid`, map[string]any{"id": h.ID})
	}); err != nil {
		t.Fatalf("remove created_xid: got = %v, want nil", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: got = %v, want nil", err)
	}

	events, err := s.ReplayEvents(ctx, pe, 0)
	if err != nil || len(events) != 1 || events[0].Subject != h.ID || events[0].Position != 0 {
		t.Errorf("snapshot replay: got = (%+v, %v), want = hypothesis.new for %s at position 0", events, err, h.ID)
	}
}

func TestImportLegacyPlacesOldHypothesesOnTheObjectiveAndIsRepeatable(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	rows := []*domain.Hypothesis{{
		ID: "legacy-" + pe, AgentID: "agent-1", PlatformExperimentID: pe,
		Text: "Old Idea", Status: domain.HypothesisConfirmed, CreatedAt: created,
	}}
	for range 2 {
		if err := s.ImportLegacy(ctx, rows); err != nil {
			t.Fatalf("ImportLegacy: got = %v, want nil", err)
		}
	}
	items, err := s.ListHypotheses(ctx, ListFilter{PlatformExperimentID: pe})
	if err != nil || len(items) != 1 {
		t.Fatalf("list after import: got = (%v, %v), want one hypothesis", items, err)
	}
	h := items[0]
	if want := (domain.Actor{Kind: domain.ActorUnverified, Name: "agent-1"}); h.Actor != want {
		t.Errorf("imported hypothesis: got actor %+v, want %+v", h.Actor, want)
	}
	if h.ID != rows[0].ID || h.Status != domain.HypothesisConfirmed || !h.CreatedAt.Equal(created) || len(h.BuildsOn) != 1 || h.BuildsOn[0] != pe {
		t.Errorf("imported hypothesis: got = %+v builds_on=%v, want the legacy row on the objective", h.Hypothesis, h.BuildsOn)
	}
	if _, _, existed, _, err := s.Register(ctx, NewHypothesis{AgentID: "agent-2",
		PlatformExperimentID: pe, Text: "old idea", Links: onRoot(pe)}); err != nil || !existed {
		t.Errorf("registering the legacy claim again: got = (existed=%v, %v), want the imported one", existed, err)
	}
}

// Each experiment's graph is closed (links stay inside it), so a result can only reach another
// experiment through an unfiltered scan or text search; such a result is refused.
func TestAQueryReadsOnePlatformExperiment(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	other := testExperiment(t, s)
	mine := register(t, s, pe, "agent-1", "cosine decay helps", onRoot(pe))
	register(t, s, other, "agent-1", "cosine decay hurts", onRoot(other))
	ctx := context.Background()

	for _, q := range []string{
		`MATCH (h:Hypothesis) RETURN h`,
		`MATCH (o:Objective) RETURN o.id, o`,
		`MATCH ()-[r:BUILDS_ON]->() RETURN r`,
		`MATCH p = (:Hypothesis)-[:BUILDS_ON]->(:Objective) RETURN p`,
		`MATCH (h:Hypothesis) RETURN collect(h) AS all`,
		`CALL text_search.search_all('` + TextIndex + `', 'cosine') YIELD node RETURN node`,
	} {
		if _, err := s.Query(ctx, pe, q, nil); !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "platform_experiment_id = $pe") {
			t.Errorf("Query %q: got = %v, want %v asking to filter on $pe", q, err, ErrInvalidQuery)
		}
	}

	for _, q := range []string{
		`MATCH p = (h:Hypothesis {platform_experiment_id: $pe})-[:BUILDS_ON]->(o:Objective) RETURN h, p, o`,
		`MATCH (h:Hypothesis)-[r:BUILDS_ON]->(:Objective {id: $pe}) RETURN r, h.id`,
		`CALL text_search.search_all('` + TextIndex + `', 'cosine') YIELD node WHERE node.platform_experiment_id = $pe RETURN node`,
	} {
		result, err := s.Query(ctx, pe, q, nil)
		if err != nil || len(result.Rows) != 1 {
			t.Errorf("Query %q: got = (%v, %v), want one row", q, result, err)
		}
	}
	if result, err := s.Query(ctx, pe, `MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN h.id`, nil); err != nil || result.Rows[0][0] != mine.ID {
		t.Errorf("Query with $pe: got = (%v, %v), want %s", result, err, mine.ID)
	}
	if _, err := s.Query(ctx, pe, `RETURN $pe`, map[string]any{"pe": other}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Query rebinding $pe: got = %v, want %v", err, ErrInvalidQuery)
	}
	if _, err := s.Query(ctx, "", `RETURN 1`, nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("Query without a platform experiment: got = %v, want %v", err, ErrInvalidQuery)
	}
}

// Evidence reaches a query as a map parameter keyed by hypothesis id. A hypothesis without an
// entry reads as null, so coalesce is what lets one query filter and rank all of them.
func TestAQueryFiltersAndRanksHypothesesByABoundEvidenceMap(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	once := register(t, s, pe, "agent-1", "cosine decay helps", onRoot(pe))
	twice := register(t, s, pe, "agent-1", "label smoothing hurts small models", onRoot(pe))
	untested := register(t, s, pe, "agent-1", "wider layers beat deeper ones", onRoot(pe))
	discussed := register(t, s, pe, "agent-1", "dropout is redundant with augmentation", onRoot(pe))
	newest := time.Now().UTC()
	params := map[string]any{"since": newest.Add(-30 * time.Minute).Format(time.RFC3339), "evidence": map[string]any{
		once.ID:      map[string]any{"jobs": int64(1), "completed": int64(1), "last_finding_at": newest.Add(-time.Hour)},
		twice.ID:     map[string]any{"jobs": int64(2), "completed": int64(2), "last_finding_at": newest},
		discussed.ID: map[string]any{"jobs": int64(0), "completed": int64(0), "comments": int64(3)},
	}}
	ids := func(cypher string) string {
		t.Helper()
		result, err := s.Query(context.Background(), pe, cypher, params)
		if err != nil {
			t.Fatalf("Query %q: got = %v, want nil", cypher, err)
		}
		var out []string
		for _, row := range result.Rows {
			out = append(out, row[0].(string))
		}
		return strings.Join(out, ",")
	}

	const match = `MATCH (h:Hypothesis {platform_experiment_id: $pe}) `
	if got, want := ids(match+`WHERE coalesce($evidence[h.id].jobs, 0) = 0 RETURN h.id ORDER BY h.created_at`), untested.ID+","+discussed.ID; got != want {
		t.Errorf("untested: got = %s, want %s", got, want)
	}
	if got, want := ids(match+`WITH h, $evidence[h.id] AS e WHERE e.completed > 0 RETURN h.id ORDER BY e.completed DESC`), twice.ID+","+once.ID; got != want {
		t.Errorf("most completed first: got = %s, want %s", got, want)
	}
	if got, want := ids(match+`RETURN h.id, coalesce($evidence[h.id].jobs, 0) AS jobs ORDER BY jobs DESC, h.created_at LIMIT 2`), twice.ID+","+once.ID; got != want {
		t.Errorf("ranked by jobs: got = %s, want %s", got, want)
	}
	if got := ids(match + `WHERE $evidence[h.id].last_finding_at > datetime($since) RETURN h.id`); got != twice.ID {
		t.Errorf("finding newer than half an hour ago: got = %s, want %s", got, twice.ID)
	}
}

// An agent's watch carries other people's activity on hypotheses it registered: a comment or
// finding on one, a link to one, a new hypothesis building on one. The graph is the only place
// that knows who registered what. A hypothesis linked to one of the agent's only later stays out
// of it: the later link is its own event.
func TestConcernsAgentFindsActivityOnTheAgentsOwnHypotheses(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	mine := register(t, s, pe, "agent-1", "label smoothing helps", onRoot(pe))
	theirs := register(t, s, pe, "agent-2", "gradient clipping stabilises training", onRoot(pe))
	child := register(t, s, pe, "agent-2", "a smoothing factor of 0.2 beats 0.1",
		[]domain.LinkSpec{{Type: domain.LinkBuildsOn, To: mine.ID}})
	if _, err := s.AddLink(context.Background(), theirs.ID, "agent-2", agentActor("agent-2"), domain.LinkSpec{Type: domain.LinkRelated, To: mine.ID}, stampAt(1)); err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	cases := []struct {
		e    db.Event
		want bool
	}{
		{db.Event{Kind: db.EventCommentNew, Subject: mine.ID}, true},
		{db.Event{Kind: db.EventFindingNew, Subject: theirs.ID}, false},
		{db.Event{Kind: db.EventHypothesisLink, Subject: theirs.ID, Detail: mine.ID}, true},
		{db.Event{Kind: db.EventHypothesisLink, Subject: theirs.ID, Detail: pe}, false},
		{db.Event{Kind: db.EventHypothesisNew, Subject: child.ID}, true},
		{db.Event{Kind: db.EventHypothesisNew, Subject: theirs.ID}, false},
	}
	events := make([]db.Event, len(cases))
	for i, c := range cases {
		events[i] = c.e
	}
	got, err := s.ConcernsAgent(context.Background(), "agent-1", events)
	if err != nil || len(got) != len(cases) {
		t.Fatalf("ConcernsAgent(agent-1, %d events): got = %v, %v", len(cases), got, err)
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("ConcernsAgent(agent-1, %+v): got = %v, want = %v", c.e, got[i], c.want)
		}
	}
}
