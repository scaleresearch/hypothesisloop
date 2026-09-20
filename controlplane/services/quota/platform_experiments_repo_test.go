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

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const experimentBase = `
name: tune the model
description: beat the baseline in the README
budget_accelerator_hours: 40
max_agents: 6
report_interval_seconds: 30
ends_at: 2026-12-01T00:00:00Z
metrics:
  - key: val_accuracy
    direction: maximize
stages:
  - length_pct: 100
    evict_pct: 0
`

func TestTheExperimentFileIsShapedLikeTheCreateBody(t *testing.T) {
	base, err := decodeExperimentBase([]byte(experimentBase))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if base.Name != "tune the model" || base.BudgetAcceleratorHours != 40 || base.MaxAgents != 6 || base.ReportIntervalSeconds != 30 ||
		!base.EndsAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || len(base.Metrics) != 1 || base.Metrics[0].Key != "val_accuracy" ||
		len(base.Stages) != 1 || base.Stages[0].LengthPct != 100 {
		t.Fatalf("base = %+v", base)
	}
	for name, file := range map[string]string{
		"a project of its own choosing": experimentBase + "project_id: prj_other\n",
		"from_repo":                     experimentBase + "from_repo: true\n",
		"a key the body does not have":  experimentBase + "budget_acelerator_hours: 4\n",
		"a list":                        "- name: a\n",
		"not yaml":                      "name: [unclosed\n",
		"a value of the wrong type":     "max_agents: many\n",
	} {
		if _, err := decodeExperimentBase([]byte(file)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
}

func TestWhatTheRequestSetsWinsOverTheFile(t *testing.T) {
	base, err := decodeExperimentBase([]byte(experimentBase))
	if err != nil {
		t.Fatal(err)
	}
	ends := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	got := overlay(base, CreatePlatformExperimentRequest{ProjectID: "prj-1", FromRepo: true, BudgetAcceleratorHours: 5, EndsAt: ends})
	if got.ProjectID != "prj-1" || !got.FromRepo || got.BudgetAcceleratorHours != 5 || !got.EndsAt.Equal(ends) {
		t.Fatalf("the request's own fields were lost: %+v", got)
	}
	if got.Name != "tune the model" || got.MaxAgents != 6 || len(got.Metrics) != 1 || len(got.Stages) != 1 {
		t.Fatalf("the file's fields were lost: %+v", got)
	}
}

type fakeProjectRepos struct {
	files   map[string]string
	refusal error
	asked   []string
}

func (f *fakeProjectRepos) CloneURL(_ context.Context, projectID string) (string, error) {
	if _, ok := f.files[projectID]; !ok {
		return "", nil
	}
	return "https://github.com/acme/" + projectID + ".git", nil
}

func (f *fakeProjectRepos) ExperimentBase(_ context.Context, projectID string) ([]byte, error) {
	f.asked = append(f.asked, projectID)
	if f.refusal != nil {
		return nil, f.refusal
	}
	return []byte(f.files[projectID]), nil
}

type createdStore struct {
	PlatformExperimentsStore
	created []*domain.PlatformExperiment
}

func (s *createdStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment) error {
	s.created = append(s.created, pe)
	return nil
}

func (s *createdStore) GetPlatformExperiment(_ context.Context, id string) (*domain.PlatformExperiment, error) {
	for _, pe := range s.created {
		if pe.ID == id {
			out := *pe
			return &out, nil
		}
	}
	return nil, nil
}

func (s *createdStore) ListSignups(context.Context, string) ([]string, error) { return nil, nil }

func repoAPI(t *testing.T, caller authtest.As, repos *fakeProjectRepos) (*createdStore, func(method, path string, body any) *httptest.ResponseRecorder) {
	t.Helper()
	store := &createdStore{}
	svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", caller, auth.NewAccess(placement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, zap.NewNop()).WithProjectRepos(repos))
	return store, func(method, path string, body any) *httptest.ResponseRecorder {
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
		return rec
	}
}

var ownerOfA = authtest.As{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}

func TestCreateFromRepoTakesWhatTheRequestLeavesOutFromTheFile(t *testing.T) {
	repos := &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	rec := call("POST", "/platform-experiments", map[string]any{"project_id": "prj-a", "from_repo": true, "budget_accelerator_hours": 5})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got = %d %s, want 201 from a body that names only the project, from_repo and a budget", rec.Code, rec.Body)
	}
	if len(store.created) != 1 {
		t.Fatalf("created = %d, want one", len(store.created))
	}
	pe := store.created[0]
	if pe.ProjectID != "prj-a" || pe.Name != "tune the model" || pe.BudgetAcceleratorHours != 5 || pe.MaxAgents != 6 ||
		pe.ReportIntervalSeconds != 30 || len(pe.Metrics) != 1 || !pe.EndsAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("created = %+v, want the file with the request's budget over it", pe)
	}
	if len(repos.asked) != 1 || repos.asked[0] != "prj-a" {
		t.Fatalf("the file was read for %v, want the request's project once", repos.asked)
	}
}

func TestCreateWithoutFromRepoReadsNoRepository(t *testing.T) {
	repos := &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	rec := call("POST", "/platform-experiments", map[string]any{"project_id": "prj-a"})
	if rec.Code != http.StatusBadRequest || len(repos.asked) != 0 || len(store.created) != 0 {
		t.Fatalf("got = %d %s, repository read %v, want 400 for the missing name and no read", rec.Code, rec.Body, repos.asked)
	}
}

// The project is checked before its repository is read, so from_repo cannot be used to learn
// whether another org's project has one.
func TestCreateFromRepoChecksTheProjectFirst(t *testing.T) {
	repos := &fakeProjectRepos{files: map[string]string{"prj-b": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	for _, project := range []string{"prj-b", "prj-missing"} {
		rec := call("POST", "/platform-experiments", map[string]any{"project_id": project, "from_repo": true})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got = %d %s, want 404", project, rec.Code, rec.Body)
		}
	}
	if len(repos.asked) != 0 || len(store.created) != 0 {
		t.Fatalf("another org's repository was read: %v", repos.asked)
	}
}

func TestCreateFromRepoRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		file    string
		refusal error
		want    int
	}{
		"no repository attached":        {"", huma.Error422UnprocessableEntity("from_repo needs a repository attached to the project, and this one has none"), http.StatusUnprocessableEntity},
		"the installation was removed":  {"", huma.Error409Conflict("the GitHub App installation behind the repository was removed or suspended"), http.StatusConflict},
		"no app on this deployment":     {"", huma.Error503ServiceUnavailable("this deployment has no GitHub App configured"), http.StatusServiceUnavailable},
		"a file that is not the body":   {"nmae: typo\n", nil, http.StatusUnprocessableEntity},
		"a file that picks its project": {experimentBase + "project_id: prj-b\n", nil, http.StatusUnprocessableEntity},
		"a file with no ranking metric": {strings.Replace(experimentBase, "direction: maximize", "direction: maximize\n    role: attribute", 1), nil, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			repos := &fakeProjectRepos{files: map[string]string{"prj-a": tc.file}, refusal: tc.refusal}
			store, call := repoAPI(t, ownerOfA, repos)
			rec := call("POST", "/platform-experiments", map[string]any{"project_id": "prj-a", "from_repo": true})
			if rec.Code != tc.want || len(store.created) != 0 {
				t.Fatalf("got = %d %s, created %d, want %d and nothing created", rec.Code, rec.Body, len(store.created), tc.want)
			}
		})
	}
}

func TestFromRepoIsRefusedOnAnUpdate(t *testing.T) {
	repos := &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}}
	_, call := repoAPI(t, authtest.Staff, repos)
	rec := call("PUT", "/platform-experiments/pe-1", map[string]any{"from_repo": true, "name": "x"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "from_repo") || len(repos.asked) != 0 {
		t.Fatalf("got = %d %s, want 400 naming from_repo and no read", rec.Code, rec.Body)
	}
}

func TestAPlatformExperimentIsReadWithItsProjectsCloneAddress(t *testing.T) {
	repos := &fakeProjectRepos{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, authtest.Staff, repos)
	store.created = []*domain.PlatformExperiment{{ID: "pe-with", ProjectID: "prj-a"}, {ID: "pe-without", ProjectID: "prj-b"}}
	for id, want := range map[string]string{"pe-with": "https://github.com/acme/prj-a.git", "pe-without": ""} {
		rec := call("GET", "/platform-experiments/"+id, nil)
		var pe map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &pe); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("%s: got = %d %s", id, rec.Code, rec.Body)
		}
		got, present := pe["code_repo_url"]
		if (want == "" && present) || (want != "" && got != want) {
			t.Errorf("%s: code_repo_url = %v (present %v), want %q", id, got, present, want)
		}
	}
}
