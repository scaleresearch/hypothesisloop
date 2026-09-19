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
// Start one with: podman run -d -p 7687:7687 docker.io/memgraph/memgraph:3.13.1 --telemetry-enabled=false
const devGraphURL = "bolt://localhost:7687"

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

func register(t *testing.T, s *Store, pe, agentID, text string, links []domain.LinkSpec) *domain.Hypothesis {
	t.Helper()
	h, _, existed, err := s.Register(context.Background(), NewHypothesis{
		Source: domain.HypothesisSourceAgent, AgentID: agentID, PlatformExperimentID: pe, Text: text, Links: links,
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

	h, links, existed, err := s.Register(context.Background(), NewHypothesis{
		Source: domain.HypothesisSourceAgent, AgentID: "agent-2", PlatformExperimentID: pe, Text: "warmup with cosine decay",
		Links: []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}, {Type: domain.LinkBuildsOn, To: b.ID}, {Type: domain.LinkRelated, To: a.ID}},
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

// The dedup rule from the Postgres days, unchanged: the same claim twice in one experiment is one
// hypothesis, and a duplicate does not get to re-place it.
func TestRegisteringAnEquivalentClaimReturnsTheExistingHypothesisWithoutApplyingItsLinks(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	a := register(t, s, pe, "agent-1", "Warmup helps", onRoot(pe))
	other := register(t, s, pe, "agent-1", "something else", onRoot(pe))

	h, links, existed, err := s.Register(context.Background(), NewHypothesis{
		Source: domain.HypothesisSourceHuman, Author: "Ada", PlatformExperimentID: pe, Text: "  warmup   HELPS ",
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
			h, _, existed, err := s.Register(context.Background(), NewHypothesis{
				Source: domain.HypothesisSourceAgent, AgentID: fmt.Sprintf("agent-%d", i), PlatformExperimentID: pe,
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
	if total, err := s.CountHypotheses(context.Background(), ListFilter{PlatformExperimentID: pe}); err != nil || total != 1 {
		t.Errorf("hypotheses in the experiment: got = (%v, %v), want (1, nil)", total, err)
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
		_, _, _, err := s.Register(context.Background(), NewHypothesis{
			Source: domain.HypothesisSourceAgent, AgentID: "agent-1", PlatformExperimentID: pe, Text: "new idea " + name, Links: links,
		})
		if !errors.Is(err, ErrInvalidLink) {
			t.Errorf("%s: got = %v, want %v", name, err, ErrInvalidLink)
		}
	}
	if total, _ := s.CountHypotheses(context.Background(), ListFilter{PlatformExperimentID: pe}); total != 1 {
		t.Errorf("hypotheses after refused registrations: got = %v, want 1", total)
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
		if _, err := s.AddLink(ctx, spec.from, "agent-1", "", spec.link); !errors.Is(err, ErrInvalidLink) {
			t.Errorf("%s: got = %v, want %v", name, err, ErrInvalidLink)
		}
	}
	if _, err := s.AddLink(ctx, "nope", "agent-1", "", domain.LinkSpec{Type: domain.LinkRelated, To: a.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("link from an unknown hypothesis: got = %v, want %v", err, ErrNotFound)
	}

	// The reverse direction of a cycle is fine as a non-builds_on link, and a second builds_on parent
	// that closes no loop is a combination.
	if _, err := s.AddLink(ctx, a.ID, "agent-1", "", domain.LinkSpec{Type: domain.LinkContradicts, To: c.ID}); err != nil {
		t.Errorf("contradicts back up the chain: got = %v, want nil", err)
	}
	d := register(t, s, pe, "agent-1", "d", onRoot(pe))
	if _, err := s.AddLink(ctx, c.ID, "agent-1", "", domain.LinkSpec{Type: domain.LinkBuildsOn, To: d.ID}); err != nil {
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
	human, _, _, err := s.Register(ctx, NewHypothesis{Source: domain.HypothesisSourceHuman, Author: "Ada", PlatformExperimentID: pe, Text: "human idea", Links: onRoot(pe)})
	if err != nil {
		t.Fatalf("Register human: got = %v, want nil", err)
	}
	link := domain.LinkSpec{Type: domain.LinkRelated, To: target.ID}

	if _, err := s.AddLink(ctx, owned.ID, "agent-2", "", link); !errors.Is(err, ErrNotOwner) {
		t.Errorf("link from another agent's hypothesis: got = %v, want %v", err, ErrNotOwner)
	}
	if _, err := s.AddLink(ctx, owned.ID, "", "Ada", link); !errors.Is(err, ErrNotOwner) {
		t.Errorf("human linking an agent's hypothesis: got = %v, want %v", err, ErrNotOwner)
	}
	first, err := s.AddLink(ctx, human.ID, "agent-2", "", link)
	if err != nil || !first.Created || first.CreatedAt.IsZero() {
		t.Fatalf("link from a human hypothesis: got = (%+v, %v), want a new link", first, err)
	}
	again, err := s.AddLink(ctx, human.ID, "agent-1", "", link)
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
	human, _, _, err := s.Register(ctx, NewHypothesis{Source: domain.HypothesisSourceHuman, Author: "dana",
		PlatformExperimentID: pe, Text: "human idea", Links: onRoot(pe)})
	if err != nil {
		t.Fatalf("Register human: got = %v, want nil", err)
	}
	linked, err := s.AddLink(ctx, human.ID, "", "dana", domain.LinkSpec{Type: domain.LinkRelated, To: a.ID})
	if err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}

	in, err := s.Links(ctx, a.ID)
	if err != nil || len(in.In) != 1 {
		t.Fatalf("Links: got = (%+v, %v), want one incoming", in.In, err)
	}
	if got := in.In[0]; got.AgentID != "" || got.Author != "dana" || !got.CreatedAt.Equal(linked.CreatedAt) {
		t.Errorf("human link: got = (%q, %q, %v), want (\"\", dana, %v)", got.AgentID, got.Author, got.CreatedAt, linked.CreatedAt)
	}
	_, edges, err := s.ExperimentGraph(ctx, pe)
	if err != nil {
		t.Fatalf("ExperimentGraph: got = %v, want nil", err)
	}
	for _, e := range edges {
		owner := e.AgentID + e.Author
		if owner == "" || e.CreatedAt.IsZero() {
			t.Errorf("edge %s -%s-> %s: got owner %q at %v, want both set", e.From, e.Type, e.To, owner, e.CreatedAt)
		}
	}
}

func TestUpdateStatusFollowsTheOwnershipRuleAndReportsChange(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	h := register(t, s, pe, "agent-1", "a", onRoot(pe))

	if _, _, err := s.UpdateStatus(ctx, h.ID, "agent-2", domain.HypothesisRefuted); !errors.Is(err, ErrNotOwner) {
		t.Errorf("non-owner: got = %v, want %v", err, ErrNotOwner)
	}
	if _, _, err := s.UpdateStatus(ctx, "nope", "agent-1", domain.HypothesisRefuted); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: got = %v, want %v", err, ErrNotFound)
	}
	got, changed, err := s.UpdateStatus(ctx, h.ID, "agent-1", domain.HypothesisRefuted)
	if err != nil || !changed || got.Status != domain.HypothesisRefuted || got.StatusChangedAt == nil {
		t.Fatalf("owner: got = (%+v, %v, %v), want refuted with status_changed_at", got, changed, err)
	}
	if _, changed, err := s.UpdateStatus(ctx, h.ID, "agent-1", domain.HypothesisRefuted); err != nil || changed {
		t.Errorf("same status again: got = (%v, %v), want (false, nil)", changed, err)
	}
}

func TestTheTextIndexRanksHypothesesThroughTheQueryEndpoint(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	register(t, s, pe, "agent-1", "Linear warmup stabilises early training", onRoot(pe))
	register(t, s, pe, "agent-1", "Label smoothing reduces overconfidence", onRoot(pe))

	result, err := s.Query(context.Background(), `CALL text_search.search_all('`+TextIndex+`', 'warmup training') YIELD node, score
WHERE node.platform_experiment_id = $pe RETURN node.text, score ORDER BY score DESC`, map[string]any{"pe": pe})
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
		if _, err := s.Query(ctx, q, nil); !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "read-only") {
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
		if _, err := s.Query(ctx, q, nil); !errors.Is(err, ErrInvalidQuery) {
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

	result, err := s.Query(ctx, `MATCH (b:Hypothesis {text: 'b', platform_experiment_id: $pe})-[r:BUILDS_ON]->(a) RETURN b, r, a.id AS parent`, map[string]any{"pe": pe})
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

	capped, err := s.Query(ctx, `UNWIND range(1, $n) AS i RETURN i`, map[string]any{"n": float64(QueryRowCap + 5)})
	if err != nil || len(capped.Rows) != QueryRowCap || !capped.Truncated {
		t.Errorf("capped query: got = (%d rows, truncated=%v, %v), want (%d, true, nil)", len(capped.Rows), capped != nil && capped.Truncated, err, QueryRowCap)
	}
	if literal, err := s.Query(ctx, `RETURN 'LOAD CSV and CALL mg.load_all()' AS s // CALL mg.load_all()`, nil); err != nil || literal.Rows[0][0] != "LOAD CSV and CALL mg.load_all()" {
		t.Errorf("keywords inside a string and a comment: got = (%v, %v), want the string back", literal, err)
	}
	if _, err := s.Query(ctx, `MATCH (n RETURN n`, nil); !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "mismatched input") {
		t.Errorf("syntax error: got = %v, want %v with Memgraph's message", err, ErrInvalidQuery)
	}
}

func TestAQueryIsStoppedAtItsTimeout(t *testing.T) {
	s := testStore(t)
	start := time.Now()
	_, err := s.Query(context.Background(), `UNWIND range(1, 40000) AS i UNWIND range(1, 40000) AS j WITH i, j WHERE i + j < 0 RETURN count(*)`, nil)
	if !errors.Is(err, ErrInvalidQuery) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("long query: got = %v, want %v naming the timeout", err, ErrInvalidQuery)
	}
	if elapsed := time.Since(start); elapsed > 2*QueryTimeout+time.Second {
		t.Errorf("long query ran for %v, want it stopped near %v", elapsed, QueryTimeout)
	}
}

// Replay is what turns a dropped /watch connection into a delay rather than a gap. Hypothesis
// events are derived from the graph's own timestamps, and links given at registration ride on
// hypothesis.new, so replay must not invent a hypothesis.link for them.
func TestReplayDerivesHypothesisEventsFromTheGraphsTimestamps(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	since := time.Now().UTC().Add(-time.Second)
	a := register(t, s, pe, "agent-1", "a", onRoot(pe))
	b := register(t, s, pe, "agent-2", "b", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: a.ID}})
	linked, err := s.AddLink(ctx, b.ID, "agent-2", "", domain.LinkSpec{Type: domain.LinkContradicts, To: a.ID})
	if err != nil {
		t.Fatalf("AddLink: got = %v, want nil", err)
	}
	settled, _, err := s.UpdateStatus(ctx, a.ID, "agent-1", domain.HypothesisConfirmed)
	if err != nil {
		t.Fatalf("UpdateStatus: got = %v, want nil", err)
	}

	events, err := s.ReplayEvents(ctx, pe, since)
	if err != nil {
		t.Fatalf("ReplayEvents: got = %v, want nil", err)
	}
	want := map[db.Event]bool{
		{Kind: db.EventHypothesisNew, Subject: a.ID, Value: "agent", PlatformExperimentID: pe, AgentID: "agent-1", Cursor: db.NewCursor(a.CreatedAt)}:                           true,
		{Kind: db.EventHypothesisNew, Subject: b.ID, Value: "agent", PlatformExperimentID: pe, AgentID: "agent-2", Cursor: db.NewCursor(b.CreatedAt)}:                           true,
		{Kind: db.EventHypothesisLink, Subject: b.ID, Value: "contradicts", Detail: a.ID, PlatformExperimentID: pe, AgentID: "agent-2", Cursor: db.NewCursor(linked.CreatedAt)}: true,
		{Kind: db.EventHypothesisStatus, Subject: a.ID, Value: "confirmed", PlatformExperimentID: pe, AgentID: "agent-1", Cursor: db.NewCursor(*settled.StatusChangedAt)}:       true,
	}
	if len(events) != len(want) {
		t.Errorf("replayed events: got %d (%+v), want %d", len(events), events, len(want))
	}
	for _, e := range events {
		if !want[e] {
			t.Errorf("unexpected replayed event %+v", e)
		}
	}

	later, err := s.ReplayEvents(ctx, pe, *settled.StatusChangedAt)
	if err != nil || len(later) != 0 {
		t.Errorf("replay after the last change: got = (%v, %v), want nothing", later, err)
	}
}

func TestImportLegacyPlacesOldHypothesesOnTheObjectiveAndIsRepeatable(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	rows := []*domain.Hypothesis{{
		ID: "legacy-" + pe, AgentID: "agent-1", Source: domain.HypothesisSourceAgent, PlatformExperimentID: pe,
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
	if h.ID != rows[0].ID || h.Status != domain.HypothesisConfirmed || !h.CreatedAt.Equal(created) || len(h.BuildsOn) != 1 || h.BuildsOn[0] != pe {
		t.Errorf("imported hypothesis: got = %+v builds_on=%v, want the legacy row on the objective", h.Hypothesis, h.BuildsOn)
	}
	if _, _, existed, err := s.Register(ctx, NewHypothesis{Source: domain.HypothesisSourceAgent, AgentID: "agent-2",
		PlatformExperimentID: pe, Text: "old idea", Links: onRoot(pe)}); err != nil || !existed {
		t.Errorf("registering the legacy claim again: got = (existed=%v, %v), want the imported one", existed, err)
	}
}
