package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// planningAPI is the planning routes and the ones a draft passes through, over store.
func planningAPI(t *testing.T, store PlatformExperimentsStore, caller auth.Principal) (*PlatformExperimentsService, func(method, path string, body any) (int, map[string]any)) {
	t.Helper()
	svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", authtest.As(caller), auth.NewAccess(placement), zap.NewNop())
	handler := NewPlatformExperimentsHandler(svc, &fakeRepositories{}, zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), handler)
	RegisterPlanningHuma(doc, handler)
	return svc, func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&payload).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := authtest.Authorize(httptest.NewRequest(method, path, &payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
}

var planner = auth.Principal{Kind: auth.KindUser, UserID: "u-1", Name: "Ada", OrgIDs: []string{"org-a"}}

// A person with an idea had to state metrics, a budget, a ladder and dates before anything
// existed. A draft is the experiment made from the idea alone.
func TestADraftIsCreatedFromOneSentenceWithThePlanningAllowanceAndNothingElse(t *testing.T) {
	store := &createdStore{}
	_, call := planningAPI(t, store, planner)
	goal := "Find out whether a cosine schedule beats a constant learning rate for a small transformer on character-level text, measured by validation loss"
	before := time.Now()
	code, out := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-a", "goal": "  " + goal + "\n"})
	if code != http.StatusCreated || len(store.created) != 1 {
		t.Fatalf("got = %d %v with %d created, want 201 and one", code, out, len(store.created))
	}
	pe := store.created[0]
	if pe.Status != domain.PlatformExpDraft || pe.Goal != goal || out["status"] != "draft" || out["goal"] != goal {
		t.Fatalf("created %+v", pe)
	}
	if pe.BudgetAcceleratorHours != DefaultPlanning.AllowanceAcceleratorHours || len(pe.Metrics) != 0 || pe.MaxAgents != 1 || pe.CoordinatorHosted {
		t.Errorf("a draft holds %v AccH, %d metrics, %d places, coordinator %v: want the allowance and nothing of a run", pe.BudgetAcceleratorHours, len(pe.Metrics), pe.MaxAgents, pe.CoordinatorHosted)
	}
	if limit := pe.EndsAt.Sub(before); limit < DefaultPlanning.Limit-time.Minute || limit > DefaultPlanning.Limit+time.Minute {
		t.Errorf("a draft ends %v after it was created, want the planning limit", limit)
	}
	if !strings.HasSuffix(pe.Name, "…") || len([]rune(pe.Name)) > draftNameLength+1 || !strings.HasPrefix(goal, strings.TrimSuffix(pe.Name, "…")) {
		t.Errorf("name = %q, want the goal cut at a word", pe.Name)
	}
	if pe.CreatedBy == nil || *pe.CreatedBy != (domain.Actor{Kind: domain.ActorUser, ID: "u-1", Name: "Ada"}) {
		t.Errorf("created_by = %+v, want the person who asked", pe.CreatedBy)
	}

	for name, body := range map[string]map[string]any{
		"no goal":                {"project_id": "prj-a", "goal": "   "},
		"a goal that is a paper": {"project_id": "prj-a", "goal": strings.Repeat("x", domain.MaxGoalBytes+1)},
	} {
		if code, out := call("POST", "/platform-experiments/drafts", body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got = %d %v, want 422", name, code, out)
		}
	}
	if code, _ := call("POST", "/platform-experiments/drafts", map[string]any{"project_id": "prj-b", "goal": goal}); code != http.StatusNotFound {
		t.Errorf("another org's project: got = %d, want 404", code)
	}
	if len(store.created) != 1 {
		t.Fatalf("a refused draft was created: %d", len(store.created))
	}
}

func TestADraftIsAbandonedByClosingItAndByItsPlanningLimit(t *testing.T) {
	now := time.Now().UTC()
	draft := func(id string, ends time.Time) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpDraft, Goal: "g", EndsAt: ends, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		draft("pe-planning", now.Add(time.Hour)), draft("pe-late", now.Add(-time.Minute)), draft("pe-dropped", now.Add(time.Hour)),
	}}}
	svc, _ := planningAPI(t, store, planner)
	// Closing a draft derives no standings: it never ran and has no metric to rank on.
	if err := svc.Close(context.Background(), "pe-dropped", nil); err != nil {
		t.Fatalf("closing a draft: %v", err)
	}
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := map[string]domain.PlatformExperimentStatus{}
	for _, pe := range store.created {
		status[pe.ID] = pe.Status
	}
	if status["pe-planning"] != domain.PlatformExpDraft || status["pe-late"] != domain.PlatformExpClosed || status["pe-dropped"] != domain.PlatformExpClosed {
		t.Fatalf("statuses = %v, want the draft past its limit and the dropped one closed", status)
	}
}

// statusStore is createdStore with the reads and the write a close and a sweep make.
type statusStore struct{ createdStore }

func (s *statusStore) ListPlatformExperimentsByStatus(_ context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	var out []*domain.PlatformExperiment
	for _, pe := range s.created {
		if pe.Status == status {
			out = append(out, pe)
		}
	}
	return out, nil
}

func (s *statusStore) UpdatePlatformExperimentStatus(_ context.Context, id string, status domain.PlatformExperimentStatus) error {
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status = status
		}
	}
	return nil
}
