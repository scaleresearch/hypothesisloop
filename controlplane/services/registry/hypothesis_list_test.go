package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
)

// listGraph holds hyp-1 to hyp-N and pages them as the graph store does, dropping ExcludeIDs first.
type listGraph struct {
	Graph
	ids      []string
	taken    []string
	gotTaken string
}

func (g *listGraph) match(f graphdb.ListFilter) []string {
	var out []string
	for _, id := range g.ids {
		if !slices.Contains(f.ExcludeIDs, id) {
			out = append(out, id)
		}
	}
	return out
}

func (g *listGraph) ListHypotheses(_ context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	matching := g.match(f)
	matching = matching[min(f.Offset, len(matching)):]
	matching = matching[:min(f.Limit, len(matching))]
	items := make([]*domain.HypothesisListItem, len(matching))
	for i, id := range matching {
		items[i] = &domain.HypothesisListItem{Hypothesis: &domain.Hypothesis{ID: id}}
	}
	return items, nil
}

func (g *listGraph) HypothesisStats(_ context.Context, f graphdb.ListFilter) (*domain.HypothesisStats, error) {
	return &domain.HypothesisStats{Total: len(g.match(f))}, nil
}

type listStore struct {
	poolStore
	graph *listGraph
}

func (s *listStore) TakenHypotheses(_ context.Context, pe string) ([]string, error) {
	s.graph.gotTaken = pe
	return s.graph.taken, nil
}

func (s *listStore) CountHypothesisActivity(context.Context, []string) (map[string]db.HypothesisActivity, error) {
	return nil, nil
}

func (s *listStore) Working(context.Context, []string) (map[string][]domain.HypothesisWorker, error) {
	return nil, nil
}

func (s *listStore) NameActors(context.Context, []*domain.Actor) error { return nil }

func listHypotheses(t *testing.T, graph *listGraph, query string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	r := authtest.Router(authtest.Staff)
	svc := &Service{store: &listStore{graph: graph}, graph: graph, logger: zap.NewNop()}
	RegisterHuma(apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.NewNop()), NewHandler(svc, zap.NewNop()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/hypotheses?"+query, nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hypotheses?%s: status %d: %s", query, rec.Code, rec.Body)
	}
	var items []struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return rec, ids
}

func hypothesisIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("hyp-%02d", i+1)
	}
	return ids
}

// A coordinator read 20 of 28 hypotheses and never knew eight more existed: every page says the
// whole count of what matches.
func TestAHypothesisPageReportsTheTotalThatMatches(t *testing.T) {
	graph := &listGraph{ids: hypothesisIDs(28)}
	rec, ids := listHypotheses(t, graph, "platform_experiment_id=pe-1")
	if len(ids) != 20 || rec.Header().Get("X-Total-Count") != "28" {
		t.Errorf("first page: %d rows, X-Total-Count %q, want 20 of 28", len(ids), rec.Header().Get("X-Total-Count"))
	}
	rec, ids = listHypotheses(t, graph, "platform_experiment_id=pe-1&offset=20&limit=50")
	if len(ids) != 8 || rec.Header().Get("X-Total-Count") != "28" {
		t.Errorf("last page: %d rows, X-Total-Count %q, want 8 of 28", len(ids), rec.Header().Get("X-Total-Count"))
	}
}

// untaken drops the hypotheses an agent works on before the page is cut, so the page and the
// total both leave them out.
func TestUntakenLeavesOutWhatAnAgentWorksOnFromThePageAndTheTotal(t *testing.T) {
	graph := &listGraph{ids: hypothesisIDs(28), taken: []string{"hyp-01", "hyp-02", "hyp-03"}}
	rec, ids := listHypotheses(t, graph, "platform_experiment_id=pe-1&untaken=true&limit=10")
	if want := hypothesisIDs(13)[3:]; !slices.Equal(ids, want) || rec.Header().Get("X-Total-Count") != "25" || graph.gotTaken != "pe-1" {
		t.Errorf("untaken: ids %v, X-Total-Count %q, taken asked in %q, want %v of 25 within pe-1", ids, rec.Header().Get("X-Total-Count"), graph.gotTaken, want)
	}
	rec, ids = listHypotheses(t, graph, "platform_experiment_id=pe-1&limit=3")
	if len(ids) != 3 || ids[0] != "hyp-01" || rec.Header().Get("X-Total-Count") != "28" {
		t.Errorf("without untaken: ids %v, X-Total-Count %q, want the taken ones kept, 28", ids, rec.Header().Get("X-Total-Count"))
	}
}

// scopedGraph pages hypotheses of named platform experiments the way the graph store does: only
// those within the caller's scope, and of those only the ones whose text holds the search.
type scopedGraph struct {
	Graph
	pe   map[string]string
	text map[string]string
}

func (g *scopedGraph) match(f graphdb.ListFilter) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(g.pe)) {
		if (!f.Restricted || slices.Contains(f.Within, g.pe[id])) &&
			(f.PlatformExperimentID == "" || f.PlatformExperimentID == g.pe[id]) &&
			strings.Contains(strings.ToLower(g.text[id]), strings.ToLower(f.Search)) {
			out = append(out, id)
		}
	}
	return out
}

func (g *scopedGraph) ListHypotheses(_ context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error) {
	matching := g.match(f)
	matching = matching[min(f.Offset, len(matching)):]
	matching = matching[:min(f.Limit, len(matching))]
	items := make([]*domain.HypothesisListItem, len(matching))
	for i, id := range matching {
		items[i] = &domain.HypothesisListItem{Hypothesis: &domain.Hypothesis{ID: id, PlatformExperimentID: g.pe[id], Text: g.text[id]}}
	}
	return items, nil
}

func (g *scopedGraph) HypothesisStats(_ context.Context, f graphdb.ListFilter) (*domain.HypothesisStats, error) {
	return &domain.HypothesisStats{Total: len(g.match(f))}, nil
}

// The hypotheses page of a project reads every experiment of it at once, a page at a time, and
// finds nothing of a project in another org, whose id it may have learned.
func TestAProjectsHypothesesArePagedSearchedAndKeptToTheCallersOrg(t *testing.T) {
	graph := &scopedGraph{
		pe:   map[string]string{"hyp-a1": "pe-a1", "hyp-a2": "pe-a1", "hyp-a3": "pe-a2", "hyp-b1": "pe-b1"},
		text: map[string]string{"hyp-a1": "Longer warmup", "hyp-a2": "Dropout", "hyp-a3": "Warmup with cosine decay", "hyp-b1": "Warmup elsewhere"},
	}
	placement := authtest.Placement{PlatformExperiments: map[string]domain.Tenancy{
		"pe-a1": {PlatformExperimentID: "pe-a1", ProjectID: "prj-a", OrgID: "org-a"},
		"pe-a2": {PlatformExperimentID: "pe-a2", ProjectID: "prj-a", OrgID: "org-a"},
		"pe-b1": {PlatformExperimentID: "pe-b1", ProjectID: "prj-b", OrgID: "org-b"},
	}}
	r := authtest.Router(authtest.As{Kind: auth.KindUser, UserID: "u-a", OrgIDs: []string{"org-a"}})
	svc := &Service{store: &listStore{}, graph: graph, logger: zap.NewNop()}
	RegisterHuma(apidocs.New(r, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop()), NewHandler(svc, zap.NewNop()))
	list := func(query string) ([]string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, authtest.Authorize(httptest.NewRequest(http.MethodGet, "/hypotheses?"+query, nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /hypotheses?%s: status %d: %s", query, rec.Code, rec.Body)
		}
		var items []struct{ ID string }
		if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(items))
		for i, item := range items {
			ids[i] = item.ID
		}
		return ids, rec.Header().Get("X-Total-Count")
	}

	if ids, total := list("project_id=prj-a&limit=2"); !slices.Equal(ids, []string{"hyp-a1", "hyp-a2"}) || total != "3" {
		t.Errorf("first page of prj-a: got %v of %s, want [hyp-a1 hyp-a2] of 3", ids, total)
	}
	if ids, total := list("project_id=prj-a&limit=2&offset=2"); !slices.Equal(ids, []string{"hyp-a3"}) || total != "3" {
		t.Errorf("second page of prj-a: got %v of %s, want [hyp-a3] of 3", ids, total)
	}
	if ids, total := list("project_id=prj-a&q=WARMUP"); !slices.Equal(ids, []string{"hyp-a1", "hyp-a3"}) || total != "2" {
		t.Errorf("search of prj-a: got %v of %s, want [hyp-a1 hyp-a3] of 2", ids, total)
	}
	if ids, total := list("project_id=prj-b"); len(ids) != 0 || total != "0" {
		t.Errorf("project of another org: got %v of %s, want nothing", ids, total)
	}
	if ids, total := list("platform_experiment_id=pe-b1"); len(ids) != 0 || total != "0" {
		t.Errorf("experiment of another org: got %v of %s, want nothing", ids, total)
	}
}
