package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
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
	r := chi.NewRouter()
	svc := &Service{store: &listStore{graph: graph}, graph: graph, logger: zap.NewNop()}
	RegisterHuma(apidocs.New(r, "test", "1.0.0", "", authtest.Staff, auth.NewAccess(authtest.Placement{}), zap.NewNop()), NewHandler(svc, zap.NewNop()))
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
