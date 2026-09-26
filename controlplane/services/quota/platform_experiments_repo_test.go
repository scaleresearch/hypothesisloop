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
	if base.Name != "tune the model" || base.ReportIntervalSeconds != 30 || len(base.Metrics) != 1 || base.Metrics[0].Key != "val_accuracy" ||
		len(base.Stages) != 1 || base.Stages[0].LengthPct != 100 {
		t.Fatalf("base = %+v", base)
	}
	// What a run costs and how long it lasts is never the file's to say.
	if base.BudgetAcceleratorHours != 0 || base.MaxAgents != 0 || !base.EndsAt.IsZero() {
		t.Fatalf("base = %+v, want no budget, no agent count and no end taken from the file", base)
	}
	for name, file := range map[string]string{
		"a project of its own choosing": experimentBase + "project_id: prj_other\n",
		"from_repo":                     experimentBase + "from_repo: true\n",
		"a key the body does not have":  experimentBase + "budget_acelerator_hours: 4\n",
		"a list":                        "- name: a\n",
		"not yaml":                      "name: [unclosed\n",
		"a value of the wrong type":     "report_interval_seconds: often\n",
		"empty":                         "",
		"only a comment":                "# nothing yet\n",
		"a key that is a number":        experimentBase + "7: seven\n",
		"a nested key that is a number": "name: x\nmetrics:\n  - key: a\n    direction: maximize\n    1: one\n",
		"a nested key that is a list":   "name: x\nstages:\n  - ? [a, b]\n    : c\n",
	} {
		if _, err := decodeExperimentBase([]byte(file)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
}

// fakeRepositories is GitHub as the org of each project reaches it: files holds, by project, the
// experiment file of the one repository its org's installation covers, acme/model, which GitHub
// spells Acme/Model.
type fakeRepositories struct {
	files   map[string]string
	refusal error
	checked []string
	asked   []string
}

const testRepository = "Acme/Model"

func notReachable() error {
	return &reasonError{status: http.StatusUnprocessableEntity, Reason: "repository_not_reachable", Message: "the GitHub installation the org connected does not cover that repository"}
}

func (f *fakeRepositories) Reachable(_ context.Context, projectID, repository string) (string, error) {
	f.checked = append(f.checked, projectID+" "+repository)
	if _, ok := f.files[projectID]; !ok || !strings.EqualFold(repository, testRepository) {
		return "", notReachable()
	}
	return testRepository, nil
}

func (f *fakeRepositories) ExperimentBase(_ context.Context, projectID, repository string) ([]byte, error) {
	f.asked = append(f.asked, projectID+" "+repository)
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

func (s *createdStore) UpdatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, _ domain.PlatformExperimentStatus) error {
	for i, stored := range s.created {
		if stored.ID == pe.ID {
			s.created[i] = pe
		}
	}
	return nil
}

func repoAPI(t *testing.T, caller authtest.As, repos *fakeRepositories) (*createdStore, func(method, path string, body any) *httptest.ResponseRecorder) {
	t.Helper()
	store := &createdStore{}
	svc := NewPlatformExperimentsService(store, domain.QuotaConfig{DefaultStages: []domain.Stage{{LengthPct: 100}}}, zap.NewNop(), "", time.Minute)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{},
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", caller, auth.NewAccess(placement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, repos, zap.NewNop()))
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

// run is what a request with from_repo must always state itself.
func run(more map[string]any) map[string]any {
	body := map[string]any{"project_id": "prj-a", "from_repo": true, "repository": "acme/model", "budget_accelerator_hours": 5, "max_agents": 3, "ends_at": "2027-01-01T00:00:00Z"}
	for key, value := range more {
		body[key] = value
	}
	return body
}

func TestCreateFromRepoTakesWhatTheRequestLeavesOutFromTheFile(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	rec := call("POST", "/platform-experiments", run(map[string]any{"description": ""}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got = %d %s, want 201", rec.Code, rec.Body)
	}
	if len(store.created) != 1 {
		t.Fatalf("created = %d, want one", len(store.created))
	}
	pe := store.created[0]
	if pe.ProjectID != "prj-a" || pe.Name != "tune the model" || pe.ReportIntervalSeconds != 30 || len(pe.Metrics) != 1 {
		t.Fatalf("created = %+v, want the file's name, cadence and metrics", pe)
	}
	// The file says 40 hours, 6 agents and an end in 2026. None of it is read.
	if pe.BudgetAcceleratorHours != 5 || pe.MaxAgents != 3 || !pe.EndsAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("created = %+v, want the request's budget, agent count and end", pe)
	}
	// A field the body carries wins over the file even when it carries the zero value.
	if pe.Description != "" {
		t.Fatalf("description = %q, want the empty one the request stated", pe.Description)
	}
	// The file is read from the repository as GitHub names it, which is also what is stored.
	if len(repos.asked) != 1 || repos.asked[0] != "prj-a "+testRepository {
		t.Fatalf("the file was read for %v, want the request's project and repository once", repos.asked)
	}
	if pe.Repository != testRepository || pe.RepositoryURL != "https://github.com/Acme/Model.git" {
		t.Fatalf("created with repository %q at %q, want GitHub's own spelling and its clone address", pe.Repository, pe.RepositoryURL)
	}
}

// Any agent's token can push to the branch the file is read from, so the file cannot raise what
// a run costs: without them in the body the request is refused before the repository is read.
func TestCreateFromRepoNeverTakesTheCostOfARunFromTheFile(t *testing.T) {
	for _, field := range []string{"budget_accelerator_hours", "max_agents", "ends_at"} {
		repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
		store, call := repoAPI(t, ownerOfA, repos)
		body := run(nil)
		delete(body, field)
		rec := call("POST", "/platform-experiments", body)
		if rec.Code != http.StatusUnprocessableEntity || len(store.created) != 0 || len(repos.asked) != 0 {
			t.Errorf("without %s: got = %d %s, read %v, want 422 before the repository is read", field, rec.Code, rec.Body, repos.asked)
		}
	}
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	if rec := call("POST", "/platform-experiments", run(map[string]any{"budget_accelerator_hours": 0})); rec.Code != http.StatusUnprocessableEntity || len(store.created) != 0 {
		t.Errorf("a zero budget in the request: got = %d %s, want 422 and the file's 40 hours left unread", rec.Code, rec.Body)
	}
}

// Without from_repo nothing can supply a field the body leaves out, so each one a platform
// experiment needs is required again. A zero starts_at is a value: it means start now.
func TestCreateWithoutFromRepoRequiresWhatTheFileCouldHaveStated(t *testing.T) {
	whole := func() map[string]any {
		return map[string]any{
			"project_id": "prj-a", "name": "by hand", "description": "", "budget_accelerator_hours": 5, "max_agents": 3,
			"report_interval_seconds": 10, "starts_at": "0001-01-01T00:00:00Z", "ends_at": "2027-01-01T00:00:00Z",
			"metrics": []map[string]any{{"key": "loss", "direction": "minimize"}},
		}
	}
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	if rec := call("POST", "/platform-experiments", whole()); rec.Code != http.StatusCreated {
		t.Fatalf("a whole body with an empty description and a zero starts_at: got = %d %s, want 201", rec.Code, rec.Body)
	}
	for _, field := range []string{"name", "description", "metrics", "report_interval_seconds", "starts_at", "budget_accelerator_hours", "max_agents", "ends_at"} {
		body := whole()
		delete(body, field)
		rec := call("POST", "/platform-experiments", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("without %s: got = %d %s, want 422 naming it", field, rec.Code, rec.Body)
		}
	}
	if len(store.created) != 1 || len(repos.asked) != 0 {
		t.Fatalf("created %d, repository read %v, want only the whole body created and no read", len(store.created), repos.asked)
	}
}

// The project is checked before GitHub is asked anything, so a repository cannot be probed
// through another org's installation.
func TestCreateFromRepoChecksTheProjectFirst(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-b": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	for _, project := range []string{"prj-b", "prj-missing"} {
		rec := call("POST", "/platform-experiments", run(map[string]any{"project_id": project}))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got = %d %s, want 404", project, rec.Code, rec.Body)
		}
	}
	if len(repos.asked) != 0 || len(repos.checked) != 0 || len(store.created) != 0 {
		t.Fatalf("GitHub was asked for another org: checked %v, read %v", repos.checked, repos.asked)
	}
}

// A file that cannot be read as the body and a merged body the service refuses are the same
// mistake to the caller: one status, and the answer names the file.
func TestCreateFromRepoRefusals(t *testing.T) {
	names := func(body string) bool { return strings.Contains(body, domain.ExperimentFile) }
	for name, tc := range map[string]struct {
		file      string
		refusal   error
		more      map[string]any
		want      int
		namesFile bool
	}{
		"the repository has no such file":       {"", &reasonError{status: 422, Reason: "no_experiment_file", Message: "the repository has none"}, nil, http.StatusUnprocessableEntity, false},
		"no app on this deployment":             {"", huma.Error503ServiceUnavailable("this deployment has no GitHub App configured"), nil, http.StatusServiceUnavailable, false},
		"an empty file":                         {"", nil, nil, http.StatusUnprocessableEntity, true},
		"a file that is not the body":           {"nmae: typo\n", nil, nil, http.StatusUnprocessableEntity, true},
		"a file that picks its project":         {experimentBase + "project_id: prj-b\n", nil, nil, http.StatusUnprocessableEntity, true},
		"a file with a numeric key":             {experimentBase + "7: seven\n", nil, nil, http.StatusUnprocessableEntity, true},
		"a file with no name":                   {strings.Replace(experimentBase, "name: tune the model", "", 1), nil, nil, http.StatusUnprocessableEntity, true},
		"a file with no ranking metric":         {strings.Replace(experimentBase, "direction: maximize", "direction: maximize\n    role: attribute", 1), nil, nil, http.StatusUnprocessableEntity, true},
		"a file with a bad ladder":              {strings.Replace(experimentBase, "length_pct: 100", "length_pct: 50", 1), nil, nil, http.StatusUnprocessableEntity, true},
		"a request that spoils the merged body": {experimentBase, nil, map[string]any{"default_quota_tier": "gold"}, http.StatusUnprocessableEntity, true},
	} {
		t.Run(name, func(t *testing.T) {
			repos := &fakeRepositories{files: map[string]string{"prj-a": tc.file}, refusal: tc.refusal}
			store, call := repoAPI(t, ownerOfA, repos)
			rec := call("POST", "/platform-experiments", run(tc.more))
			if rec.Code != tc.want || len(store.created) != 0 || names(rec.Body.String()) != tc.namesFile {
				t.Fatalf("got = %d %s, created %d, want %d, nothing created, the file named: %v", rec.Code, rec.Body, len(store.created), tc.want, tc.namesFile)
			}
		})
	}
}

// PUT has its own body. A partial one is refused whole: taken as it stands it would clear the
// description, and from_repo is no field of it.
func TestAPartialUpdateIsRefusedAndClearsNothing(t *testing.T) {
	whole := func() map[string]any {
		return map[string]any{
			"name": "renamed", "description": "a new brief", "budget_accelerator_hours": 5, "max_agents": 3,
			"report_interval_seconds": 10, "starts_at": "2026-06-01T00:00:00Z", "ends_at": "2027-01-01T00:00:00Z",
			"metrics": []map[string]any{{"key": "loss", "direction": "minimize"}},
		}
	}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		repos := &fakeRepositories{}
		store, call := repoAPI(t, authtest.Staff, repos)
		stored := func() *domain.PlatformExperiment { return store.created[0] }
		store.created = []*domain.PlatformExperiment{{
			ID: "pe-1", ProjectID: "prj-a", Name: "kept", Description: "the brief agents work from", Status: status,
			BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 10,
			StartsAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), EndsAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}}
		for name, body := range map[string]map[string]any{
			"a name alone":               {"name": "x"},
			"a description alone":        {"description": "y"},
			"everything but description": func() map[string]any { b := whole(); delete(b, "description"); return b }(),
			"from_repo beside the rest":  func() map[string]any { b := whole(); b["from_repo"] = true; return b }(),
		} {
			rec := call("PUT", "/platform-experiments/pe-1", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s, %s: got = %d %s, want 422", status, name, rec.Code, rec.Body)
			}
			if stored().Name != "kept" || stored().Description != "the brief agents work from" {
				t.Fatalf("%s, %s: the stored experiment became %q / %q", status, name, stored().Name, stored().Description)
			}
		}
		if rec := call("PUT", "/platform-experiments/pe-1", whole()); rec.Code != http.StatusOK || stored().Name != "renamed" || stored().Description != "a new brief" {
			t.Errorf("%s: a whole update: got = %d %s, stored %q / %q", status, rec.Code, rec.Body, stored().Name, stored().Description)
		}
		if len(repos.asked) != 0 {
			t.Fatalf("%s: an update read the repository: %v", status, repos.asked)
		}
	}
}

// The repository and the image are the experiment's own, stated at create and by nothing else.
func TestCreateNamesTheRepositoryAndTheAgentImage(t *testing.T) {
	whole := func(more map[string]any) map[string]any {
		body := map[string]any{
			"project_id": "prj-a", "name": "by hand", "description": "d", "budget_accelerator_hours": 5, "max_agents": 3,
			"report_interval_seconds": 10, "starts_at": "0001-01-01T00:00:00Z", "ends_at": "2027-01-01T00:00:00Z",
			"metrics": []map[string]any{{"key": "loss", "direction": "minimize"}},
		}
		for key, value := range more {
			body[key] = value
		}
		return body
	}
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call := repoAPI(t, ownerOfA, repos)
	rec := call("POST", "/platform-experiments", whole(map[string]any{"repository": "acme/model", "agent_image": "registry.example/team/agent:v2"}))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusCreated || err != nil {
		t.Fatalf("got = %d %s, want 201", rec.Code, rec.Body)
	}
	if out["repository"] != testRepository || out["repository_url"] != "https://github.com/Acme/Model.git" || out["agent_image"] != "registry.example/team/agent:v2" {
		t.Fatalf("answer = %v, want the repository as GitHub names it, its clone address and the image", out)
	}
	if _, old := out["code_repo_url"]; old {
		t.Fatalf("the answer still carries code_repo_url: %v", out)
	}
	if pe := store.created[0]; pe.Repository != testRepository || pe.AgentImage != "registry.example/team/agent:v2" {
		t.Fatalf("stored = %+v", pe)
	}
	if len(repos.asked) != 0 {
		t.Fatalf("a create without from_repo read the experiment file: %v", repos.asked)
	}

	// Neither is needed: an experiment its owner runs by hand names no repository and no image.
	rec = call("POST", "/platform-experiments", whole(nil))
	bare := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &bare)
	if _, named := bare["repository_url"]; rec.Code != http.StatusCreated || named || store.created[1].Repository != "" || store.created[1].AgentImage != "" {
		t.Fatalf("without either: got = %d %s", rec.Code, rec.Body)
	}

	for name, tc := range map[string]struct {
		more   map[string]any
		reason string
	}{
		"a repository the org does not reach": {map[string]any{"repository": "rival/secret"}, "repository_not_reachable"},
		"an image with a space":               {map[string]any{"agent_image": "agent image"}, "invalid_agent_image"},
		"an image with a newline":             {map[string]any{"agent_image": "agent\nENV=x"}, "invalid_agent_image"},
		"an image too long to be one":         {map[string]any{"agent_image": strings.Repeat("a", domain.MaxAgentImageLength+1)}, "invalid_agent_image"},
		"from_repo with no repository":        {map[string]any{"from_repo": true}, "no_repository"},
	} {
		rec := call("POST", "/platform-experiments", whole(tc.more))
		var refused map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &refused)
		if rec.Code != http.StatusUnprocessableEntity || refused["reason"] != tc.reason || len(store.created) != 2 {
			t.Errorf("%s: got = %d %s, want 422 %s and nothing created", name, rec.Code, rec.Body, tc.reason)
		}
	}
}

// Any agent's token can push to the branch the file is read from: the file cannot move the
// experiment to another repository or put another image on its machines.
func TestTheExperimentFileNamesNeitherRepositoryNorImage(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase + "agent_image: registry.example/of-the-file\n"}}
	store, call := repoAPI(t, ownerOfA, repos)
	if rec := call("POST", "/platform-experiments", run(nil)); rec.Code != http.StatusCreated || store.created[0].AgentImage != "" {
		t.Fatalf("an image in the file: got = %d %s, created %+v, want it left unread", rec.Code, rec.Body, store.created)
	}
	repos.files["prj-a"] = experimentBase + "repository: rival/secret\n"
	if rec := call("POST", "/platform-experiments", run(nil)); rec.Code != http.StatusUnprocessableEntity || len(store.created) != 1 {
		t.Fatalf("a repository in the file: got = %d %s, want 422", rec.Code, rec.Body)
	}
}

// Both are fixed at create: an update has no field for either.
func TestAnUpdateCannotMoveTheRepositoryOrTheImage(t *testing.T) {
	store, call := repoAPI(t, authtest.Staff, &fakeRepositories{})
	store.created = []*domain.PlatformExperiment{{
		ID: "pe-1", ProjectID: "prj-a", Name: "kept", Status: domain.PlatformExpOpen, Repository: testRepository, AgentImage: "image:1",
		BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 10, EndsAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
	}}
	whole := map[string]any{
		"name": "renamed", "description": "d", "budget_accelerator_hours": 5, "max_agents": 3,
		"report_interval_seconds": 10, "starts_at": "2026-06-01T00:00:00Z", "ends_at": "2027-01-01T00:00:00Z",
		"metrics": []map[string]any{{"key": "loss", "direction": "minimize"}},
	}
	for _, field := range []string{"repository", "agent_image"} {
		body := map[string]any{field: "other/one"}
		for key, value := range whole {
			body[key] = value
		}
		if rec := call("PUT", "/platform-experiments/pe-1", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("an update with %s: got = %d %s, want 422", field, rec.Code, rec.Body)
		}
	}
	if rec := call("PUT", "/platform-experiments/pe-1", whole); rec.Code != http.StatusOK || store.created[0].Repository != testRepository || store.created[0].AgentImage != "image:1" {
		t.Fatalf("a whole update: got = %d %s, stored %+v, want the repository and image kept", rec.Code, rec.Body, store.created[0])
	}
}
