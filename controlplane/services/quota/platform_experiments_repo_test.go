package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

const experimentBase = `
name: tune the model
description: beat the baseline in the README
budget_accelerator_hours: 40
max_agents: 6
report_interval_seconds: 30
duration_hours: 400
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
	if base.BudgetAcceleratorHours != 0 || base.MaxAgents != 0 || base.DurationHours != 0 {
		t.Fatalf("base = %+v, want no budget, no agent count and no length taken from the file", base)
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
	// created holds each repository made for an experiment that names none, as "project name",
	// and committed the files the last one started from.
	created    []string
	committed  map[string]string
	notCreated error
	// notOn is what refuses an account before anything is created.
	notOn error
}

func (f *fakeRepositories) Create(_ context.Context, projectID, account, name, _ string, files map[string]string) (string, error) {
	if f.notCreated != nil {
		return "", f.notCreated
	}
	f.created = append(f.created, projectID+" "+name)
	f.committed = files
	if account == "" {
		account = "acme"
	}
	return account + "/" + name, nil
}

// CreatesOn answers the account named, or acme, the only one the org connected, and notOn when
// that is set.
func (f *fakeRepositories) CreatesOn(_ context.Context, _, account string) (string, error) {
	if f.notOn != nil {
		return "", f.notOn
	}
	if account == "" {
		return "acme", nil
	}
	return account, nil
}

const testRepository = "Acme/Model"

func notReachable() error {
	return api.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "the GitHub installation the org connected does not cover that repository")
}

func (f *fakeRepositories) Reachable(_ context.Context, projectID, repository string) (string, error) {
	f.checked = append(f.checked, projectID+" "+repository)
	if _, ok := f.files[projectID]; !ok || !strings.EqualFold(repository, testRepository) {
		return "", notReachable()
	}
	return testRepository, nil
}

func (f *fakeRepositories) URL(repository string) string {
	return "https://github.com/" + repository + ".git"
}

func (f *fakeRepositories) Credential(context.Context, string, string, bool) (*githost.Credential, error) {
	panic("the handler asked for a git credential")
}

func (f *fakeRepositories) Serves(string, string, string, string) bool { return false }

func (f *fakeRepositories) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, githost.ErrFileNotFound
}

func (f *fakeRepositories) File(_ context.Context, projectID, repository, _ string) ([]byte, error) {
	f.asked = append(f.asked, projectID+" "+repository)
	if f.refusal != nil {
		return nil, f.refusal
	}
	return []byte(f.files[projectID]), nil
}

type createdStore struct {
	fakeStore
	created []*domain.PlatformExperiment
	// prior are the prior runs of every platform experiment it holds.
	prior []string
	// messages are the messages sent to its agents.
	messages []domain.AgentMessage
	// researchers are the agents of role competitor that joined.
	researchers []string
	// baselines are the agents of role baseline that joined, and left the COMPLETED jobs on the
	// objective of those of them that finished.
	baselines []domain.AgentSignup
	left      []*domain.Experiment
	// objective are the jobs on the objective, oldest first.
	objective []*domain.Experiment
	// completed is every COMPLETED job, oldest first.
	completed []*domain.Experiment
	// started counts the starts that reached the store.
	started int
	// answered is when a launcher last asked for its machines, by name. One not in it asked now.
	answered map[string]*time.Time
}

func (s *createdStore) awaiting(pe *domain.PlatformExperiment) bool {
	return pe.Status != domain.PlatformExpClosed && pe.AwaitsRepository()
}

func (s *createdStore) PriorRuns(context.Context, string) ([]string, error) {
	return s.prior, nil
}

func (s *createdStore) RunsToRank(_ context.Context, ids []string) ([]db.RunToRank, error) {
	var out []db.RunToRank
	for _, id := range ids {
		for _, pe := range s.created {
			if pe.ID == id {
				out = append(out, db.RunToRank{PE: pe})
			}
		}
	}
	return out, nil
}

func (s *createdStore) RepositoriesToCreate(context.Context) ([]db.RepositoryToCreate, error) {
	var out []db.RepositoryToCreate
	for _, pe := range s.created {
		if s.awaiting(pe) {
			out = append(out, db.RepositoryToCreate{ID: pe.ID, ProjectID: pe.ProjectID, Name: pe.Name, Account: pe.RepositoryAccount})
		}
	}
	return out, nil
}

func (s *createdStore) GiveRepository(_ context.Context, id, repository, url string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Repository, pe.RepositoryURL = repository, url
			return nil
		}
	}
	return db.ErrRepositoryNotAwaited
}

func (s *createdStore) FailRepository(_ context.Context, id, answer string) error {
	for _, pe := range s.created {
		if pe.ID == id && s.awaiting(pe) {
			pe.Status, pe.CloseReason, pe.RepositoryError = domain.PlatformExpClosed, domain.CloseRepositoryNotCreated, answer
		}
	}
	return nil
}

func (s *createdStore) LauncherAnsweredAt(_ context.Context, names []string) (map[string]*time.Time, error) {
	now := time.Now()
	out := map[string]*time.Time{}
	for _, name := range names {
		out[name] = &now
		if at, ok := s.answered[name]; ok {
			out[name] = at
		}
	}
	return out, nil
}

func (s *createdStore) ListExperimentsTesting(context.Context, []string) ([]*domain.Experiment, error) {
	return s.objective, nil
}

func (s *createdStore) FinishedBaselineJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.left, nil
}

func (s *createdStore) CompletedJobs(context.Context, string) ([]*domain.Experiment, error) {
	return s.completed, nil
}

// StartPlatformExperimentTx starts the open platform experiment id with a researcher at work.
func (s *createdStore) StartPlatformExperimentTx(_ context.Context, id string, _ time.Duration, quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	participants := make([]db.StartParticipant, len(s.researchers))
	for i, agentID := range s.researchers {
		participants[i] = db.StartParticipant{AgentID: agentID, Role: domain.SignupRoleCompetitor, AgentExists: true}
	}
	quotas, _, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
	}
	for _, pe := range s.created {
		if pe.ID == id {
			pe.Status, pe.StartedAt = domain.PlatformExpRunning, time.Now().UTC()
			pe.SetClosesAt(pe.StartedAt.Add(time.Hour))
		}
	}
	s.started++
	return true, quotas, nil
}

func (s *createdStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleCompetitor {
		return nil, nil
	}
	return s.researchers, nil
}

func (s *createdStore) CreatePlatformExperiment(_ context.Context, pe *domain.PlatformExperiment, _ []*domain.Agent, signups []domain.AgentSignup) error {
	s.created = append(s.created, pe)
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			s.researchers = append(s.researchers, signup.AgentID)
			continue
		}
		signup.Name = "Baseline 1"
		s.baselines = append(s.baselines, signup)
	}
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

func (s *createdStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	signups := make([]domain.AgentSignup, len(s.researchers))
	for i, agentID := range s.researchers {
		signups[i] = domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleCompetitor}
	}
	return append(signups, s.baselines...), nil
}

// UpdatePlatformExperiment holds max_agents to the researchers that joined, every one at work.
func (s *createdStore) UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, status domain.PlatformExperimentStatus, researchers domain.ResearcherPool) error {
	return s.update(pe, researchers, len(s.researchers))
}

func (s *createdStore) update(pe *domain.PlatformExperiment, researchers domain.ResearcherPool, atWork int) error {
	if pe.MaxAgents < atWork {
		return &db.PoolBelowAtWorkError{Count: pe.MaxAgents, AtWork: atWork}
	}
	if researchers != nil {
		pe.Researchers = researchers
	}
	for i, stored := range s.created {
		if stored.ID == pe.ID {
			s.created[i] = pe
		}
	}
	return nil
}

func repoAPI(t *testing.T, caller authtest.As, repos *fakeRepositories) (*createdStore, func(method, path string, body any) *httptest.ResponseRecorder, *PlatformExperimentsService) {
	t.Helper()
	store := &createdStore{}
	svc := reaching(NewPlatformExperimentsService(store, testSettings, zap.NewNop(), "", time.Minute), domain.CPUType).WithRepositories(repos)
	placement := authtest.Placement{
		Projects:            map[string]domain.Project{"prj-a": {ID: "prj-a", OrgID: "org-a"}, "prj-b": {ID: "prj-b", OrgID: "org-b"}},
		PlatformExperiments: map[string]domain.Tenancy{},
	}
	router := authtest.Router(caller)
	doc := apidocs.New(router, "test", "1.0.0", "", auth.NewAccess(placement), zap.NewNop())
	RegisterHuma(doc, NewHandler(nil, zap.NewNop()), NewPlatformExperimentsHandler(svc, repos, testPlanning, zap.NewNop()).WithGitProxy("https://api.test/git"))
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
	}, svc
}

var ownerOfA = authtest.As{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}

// run is what a request with from_repo must always state itself.
func run(more map[string]any) map[string]any {
	body := map[string]any{"project_id": "prj-a", "from_repo": true, "repository": "acme/model", "budget_accelerator_hours": 5, "max_agents": 3, "duration_hours": 48}
	for key, value := range more {
		body[key] = value
	}
	return body
}

func TestCreateFromRepoTakesWhatTheRequestLeavesOutFromTheFile(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call, _ := repoAPI(t, ownerOfA, repos)
	rec := call("POST", "/platform-experiments", run(map[string]any{"description": "the request's brief"}))
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
	// The file says 40 hours, 6 agents and a run of 400 hours. None of it is read.
	if pe.BudgetAcceleratorHours != 5 || pe.MaxAgents != 3 || pe.DurationHours != 48 {
		t.Fatalf("created = %+v, want the request's budget, agent count and length", pe)
	}
	// A field the body carries wins over the file.
	if pe.Description != "the request's brief" {
		t.Fatalf("description = %q, want the one the request stated", pe.Description)
	}
	// The file is read from the repository as GitHub names it, which is also what is stored.
	if len(repos.asked) != 1 || repos.asked[0] != "prj-a "+testRepository {
		t.Fatalf("the file was read for %v, want the request's project and repository once", repos.asked)
	}
	if pe.Repository != testRepository || pe.RepositoryURL != "https://github.com/Acme/Model.git" {
		t.Fatalf("created with repository %q at %q, want GitHub's own spelling and its clone address", pe.Repository, pe.RepositoryURL)
	}
	if want := "https://api.test/git/" + pe.ID + ".git"; pe.GitURL != want {
		t.Fatalf("answered with git_url %q, want the repository through the proxy, %q", pe.GitURL, want)
	}
}

// Any agent's token can push to the branch the file is read from, so the file cannot raise what
// a run costs: without them in the body the request is refused before the repository is read.
func TestCreateFromRepoNeverTakesTheCostOfARunFromTheFile(t *testing.T) {
	for _, field := range []string{"budget_accelerator_hours", "duration_hours"} {
		repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
		store, call, _ := repoAPI(t, ownerOfA, repos)
		body := run(nil)
		delete(body, field)
		rec := call("POST", "/platform-experiments", body)
		if rec.Code != http.StatusUnprocessableEntity || len(store.created) != 0 || len(repos.asked) != 0 {
			t.Errorf("without %s: got = %d %s, read %v, want 422 before the repository is read", field, rec.Code, rec.Body, repos.asked)
		}
	}
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call, _ := repoAPI(t, ownerOfA, repos)
	if rec := call("POST", "/platform-experiments", run(map[string]any{"budget_accelerator_hours": 0})); rec.Code != http.StatusUnprocessableEntity || len(store.created) != 0 {
		t.Errorf("a zero budget in the request: got = %d %s, want 422 and the file's 40 hours left unread", rec.Code, rec.Body)
	}
}

// Without from_repo nothing can supply a field the body leaves out, so what only the caller can
// say is required. The rest is the platform's setting: the report interval is, there is no start
// time, and the length is a duration or an end.
func TestCreateWithoutFromRepoRequiresWhatOnlyTheCallerCanSay(t *testing.T) {
	whole := func() map[string]any {
		return map[string]any{
			"project_id": "prj-a", "name": "by hand", "description": "the brief", "budget_accelerator_hours": 5, "max_agents": 3, "duration_hours": 2,
			"metrics": []map[string]any{{"key": "loss", "direction": "minimize"}},
		}
	}
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call, svc := repoAPI(t, ownerOfA, repos)
	before := time.Now()
	if rec := call("POST", "/platform-experiments", whole()); rec.Code != http.StatusCreated {
		t.Fatalf("a body with a duration and no report interval or start: got = %d %s, want 201", rec.Code, rec.Body)
	}
	pe := store.created[0]
	// Its two hours count from the start of its run, so until then it has no end: its baseline has
	// the platform's limit from the create.
	if pe.ReportIntervalSeconds != 30 || pe.StartsAt.Before(before) || pe.StartsAt.After(time.Now()) || pe.DurationHours != 2 ||
		!pe.EndsAt.IsZero() || !pe.BaselineDueAt.Equal(pe.StartsAt.Add(testSettings.BaselineLimit)) {
		t.Fatalf("created = %+v, want the platform's report interval, a start at the create, no end yet and the baseline due at the platform's limit", pe)
	}
	for _, field := range []string{"name", "description", "metrics", "budget_accelerator_hours", "max_agents", "duration_hours"} {
		body := whole()
		delete(body, field)
		rec := call("POST", "/platform-experiments", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("without %s: got = %d %s, want 422 naming it", field, rec.Code, rec.Body)
		}
	}
	both := whole()
	both["ends_at"] = "2030-01-01T00:00:00Z"
	if rec := call("POST", "/platform-experiments", both); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ends_at") {
		t.Errorf("an end stated beside the duration: got = %d %s, want 422 naming ends_at, which is no field of a create", rec.Code, rec.Body)
	}
	if len(store.created) != 1 || len(repos.asked) != 0 || len(repos.created) != 0 {
		t.Fatalf("created %d, repository read %v, made %v, want only the whole body created, no read and its repository not made yet", len(store.created), repos.asked, repos.created)
	}

	// One that names no repository is answered at once and gets its own on the org's account from
	// the repository pass, and none is made for a body that is refused.
	named := whole()
	named["name"] = "LR warm-up: a sweep!"
	rec := call("POST", "/platform-experiments", named)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusCreated || out["repository"] != nil || out["status_reason"].(map[string]any)["reason"] != "repository_creating" {
		t.Fatalf("with no repository: got = %d %s, want 201 with no repository yet and repository_creating", rec.Code, rec.Body)
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "acme/lr-warm-up-a-sweep-" + strings.TrimPrefix(out["id"].(string), "pe-")
	if pe := store.created[1]; pe.Repository != want || pe.RepositoryURL != "https://github.com/"+want+".git" || len(repos.created) != 2 || !strings.HasPrefix(repos.created[1], "prj-a ") {
		t.Fatalf("after the pass: repository %q at %q (created %v), want %s made for the project's org and stored", pe.Repository, pe.RepositoryURL, repos.created, want)
	}
	invalid := whole()
	invalid["metrics"] = []map[string]any{}
	if rec := call("POST", "/platform-experiments", invalid); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_metrics") {
		t.Errorf("a body with no ranking metric: got = %d %s, want 422 invalid_metrics", rec.Code, rec.Body)
	}
	repos.notOn = api.Refuse(http.StatusUnprocessableEntity, "github_not_connected", "the org has connected no GitHub account")
	if rec := call("POST", "/platform-experiments", whole()); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "github_not_connected") {
		t.Errorf("when the org has connected no GitHub account: got = %d %s, want 422 github_not_connected", rec.Code, rec.Body)
	}
	if len(store.created) != 2 {
		t.Fatalf("created %d, want nothing more stored", len(store.created))
	}
	// The account a new repository goes to is the request's to say, and never beside a repository.
	repos.notOn = nil
	onAccount := whole()
	onAccount["repository_account"] = "second-account"
	if rec := call("POST", "/platform-experiments", onAccount); rec.Code != http.StatusCreated || store.created[2].RepositoryAccount != "second-account" {
		t.Errorf("naming the account: got = %d %s, want the repository to be made there", rec.Code, rec.Body)
	}
	if err := svc.ProvideRepositories(context.Background()); err != nil || !strings.HasPrefix(store.created[2].Repository, "second-account/") {
		t.Errorf("after the pass: repository %q, %v, want it made on second-account", store.created[2].Repository, err)
	}
	onAccount["repository"] = "acme/model"
	if rec := call("POST", "/platform-experiments", onAccount); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_request") || len(store.created) != 3 {
		t.Errorf("an account beside a repository: got = %d %s, want 422 invalid_request", rec.Code, rec.Body)
	}
}

// The project is checked before GitHub is asked anything, so a repository cannot be probed
// through another org's installation.
func TestCreateFromRepoChecksTheProjectFirst(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-b": experimentBase}}
	store, call, _ := repoAPI(t, ownerOfA, repos)
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
		"the repository has no such file":       {"", api.Refuse(422, "no_experiment_file", "the repository has none"), nil, http.StatusUnprocessableEntity, false},
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
			store, call, _ := repoAPI(t, ownerOfA, repos)
			rec := call("POST", "/platform-experiments", run(tc.more))
			if rec.Code != tc.want || len(store.created) != 0 || names(rec.Body.String()) != tc.namesFile {
				t.Fatalf("got = %d %s, created %d, want %d, nothing created, the file named: %v", rec.Code, rec.Body, len(store.created), tc.want, tc.namesFile)
			}
		})
	}
}

// The update was a PUT that had to carry every field at its live value, so changing the brief
// meant reading the experiment, rebuilding its whole body and sending it back. It is a PATCH of
// the fields that change.
func TestAnUpdateTakesOnlyTheFieldsThatChange(t *testing.T) {
	starts, ends := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	live := func(status domain.PlatformExperimentStatus) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "prj-a", Name: "kept", Description: "the brief agents work from", Status: status,
			Repository: testRepository, AgentImage: "image:1", BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 10,
			StartsAt: starts, DurationHours: 24, BaselineDueAt: ends, Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}
	}
	repos := &fakeRepositories{}
	store, call, _ := repoAPI(t, authtest.Staff, repos)
	patch := func(status domain.PlatformExperimentStatus, body map[string]any) (int, map[string]any, *domain.PlatformExperiment) {
		t.Helper()
		store.created = []*domain.PlatformExperiment{live(status)}
		rec := call("PATCH", "/platform-experiments/pe-1", body)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, store.created[0]
	}

	// One field changes and every other keeps its value, running or not.
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		code, out, stored := patch(status, map[string]any{"description": "a new brief"})
		want := live(status)
		want.Description = "a new brief"
		if code != http.StatusOK || !reflect.DeepEqual(stored, want) {
			t.Fatalf("%s, the description alone: got = %d %v, stored %+v", status, code, out, stored)
		}
	}
	// A name and a brief are stored without the whitespace around them, as a create stores them.
	if code, out, stored := patch(domain.PlatformExpRunning, map[string]any{"name": " renamed\n", "description": "a new brief\n"}); code != http.StatusOK || stored.Name != "renamed" || stored.Description != "a new brief" {
		t.Errorf("a name and a brief written as blocks: got = %d %v, stored %q and %q", code, out, stored.Name, stored.Description)
	}
	// While it is open, what a run costs and how it is judged can still change.
	code, out, stored := patch(domain.PlatformExpOpen, map[string]any{
		"budget_accelerator_hours": 8, "max_agents": 6, "max_concurrent_accelerators": 4, "report_interval_seconds": 60, "duration_hours": 36,
		"metrics": []map[string]any{{"key": "val_accuracy", "direction": "maximize"}},
	})
	if code != http.StatusOK || stored.BudgetAcceleratorHours != 8 || stored.MaxAgents != 6 || *stored.MaxConcurrentAccelerators != 4 || stored.ReportIntervalSeconds != 60 ||
		stored.Metrics[0].Key != "val_accuracy" || !stored.StartsAt.Equal(starts) || stored.DurationHours != 36 || !stored.BaselineDueAt.Equal(ends) || !stored.EndsAt.IsZero() || stored.Description != "the brief agents work from" {
		t.Errorf("an open experiment's fields: got = %d %v, stored %+v", code, out, stored)
	}

	for name, c := range map[string]struct {
		status       domain.PlatformExperimentStatus
		body         map[string]any
		code         int
		reason, says string
	}{
		"nothing":                       {domain.PlatformExpOpen, map[string]any{}, 422, "nothing_to_change", ""},
		"a budget once it runs":         {domain.PlatformExpRunning, map[string]any{"description": "d", "budget_accelerator_hours": 9, "starts_at": "2030-01-02T00:00:00Z", "max_agents": 9, "duration_hours": 9}, 409, "experiment_running", "carries: budget_accelerator_hours, starts_at."},
		"a closed one":                  {domain.PlatformExpClosed, map[string]any{"description": "d"}, 409, "experiment_not_editable", ""},
		"a draft":                       {domain.PlatformExpDraft, map[string]any{"description": "d"}, 409, "experiment_not_editable", ""},
		"no name":                       {domain.PlatformExpOpen, map[string]any{"name": " "}, 422, "invalid_experiment", "name"},
		"a name of two lines":           {domain.PlatformExpRunning, map[string]any{"name": "one\ntwo"}, 422, "invalid_experiment", "name is one line"},
		"no brief":                      {domain.PlatformExpRunning, map[string]any{"description": " "}, 422, "invalid_experiment", "description"},
		"a run of years":                {domain.PlatformExpOpen, map[string]any{"duration_hours": 99999}, 422, "run_too_long", "30 days"},
		"an end, which is no field":     {domain.PlatformExpOpen, map[string]any{"ends_at": "2099-01-01T00:00:00Z"}, 422, "", ""},
		"no ranked metric":              {domain.PlatformExpOpen, map[string]any{"metrics": []map[string]any{}}, 422, "invalid_metrics", "metrics"},
		"a concurrency cap of zero":     {domain.PlatformExpOpen, map[string]any{"max_concurrent_accelerators": 0}, 422, "invalid_experiment", "max_concurrent_accelerators"},
		"the repository, fixed":         {domain.PlatformExpOpen, map[string]any{"description": "d", "repository": "other/one"}, 422, "", ""},
		"the image, fixed":              {domain.PlatformExpOpen, map[string]any{"description": "d", "agent_image": "other:1"}, 422, "", ""},
		"from_repo, which is no update": {domain.PlatformExpOpen, map[string]any{"description": "d", "from_repo": true}, 422, "", ""},
	} {
		code, out, stored := patch(c.status, c.body)
		message, _ := out["message"].(string)
		if code != c.code || (c.reason != "" && out["reason"] != c.reason) || !strings.Contains(message, c.says) {
			t.Errorf("%s: got = %d %v, want %d %s naming %q", name, code, out, c.code, c.reason, c.says)
		}
		if !reflect.DeepEqual(stored, live(c.status)) {
			t.Errorf("%s: a refused update changed the experiment to %+v", name, stored)
		}
	}
	if len(repos.asked) != 0 {
		t.Fatalf("an update read the repository: %v", repos.asked)
	}
	// The whole-body PUT is gone.
	if rec := call("PUT", "/platform-experiments/pe-1", map[string]any{"description": "d"}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: got = %d, want 405", rec.Code)
	}
}

// The owner who wants the run longer or shorter had to close it and start again. Its length
// changes while it runs, still counted from its start, and never to an end that has passed.
func TestARunningExperimentTakesANewDuration(t *testing.T) {
	started := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	repos := &fakeRepositories{}
	store, call, _ := repoAPI(t, authtest.Staff, repos)
	patch := func(awaitingAgents bool, hours float64) (int, map[string]any, *domain.PlatformExperiment) {
		t.Helper()
		pe := &domain.PlatformExperiment{
			ID: "pe-1", ProjectID: "prj-a", Name: "kept", Description: "the brief agents work from", Status: domain.PlatformExpRunning,
			Repository: testRepository, BudgetAcceleratorHours: 5, MaxAgents: 3, ReportIntervalSeconds: 10,
			StartsAt: started, DurationHours: 4, Metrics: []domain.MetricDefinition{{Key: "loss", Direction: "minimize"}},
		}
		if awaitingAgents {
			pe.SetClosesAt(time.Now().UTC().Add(10 * time.Minute))
		} else {
			pe.StartedAt = started
			pe.SetClosesAt(started.Add(4 * time.Hour))
		}
		store.created = []*domain.PlatformExperiment{pe}
		rec := call("PATCH", "/platform-experiments/pe-1", map[string]any{"duration_hours": hours})
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, store.created[0]
	}
	if code, out, stored := patch(false, 6); code != http.StatusOK || stored.DurationHours != 6 || !stored.ClosesAt().Equal(started.Add(6*time.Hour)) {
		t.Errorf("six hours: got = %d %v, closes at %v, want the run's end moved to %v", code, out, stored.ClosesAt(), started.Add(6*time.Hour))
	}
	if code, out, stored := patch(false, 1); code != http.StatusUnprocessableEntity || out["reason"] != "invalid_length" || stored.DurationHours != 4 {
		t.Errorf("an end that passed: got = %d %v, want 422 invalid_length and the run as it was", code, out)
	}
	// With no researcher at work yet it closes at the end of its run when that comes first.
	if code, out, stored := patch(true, 2.1); code != http.StatusOK || !stored.AgentsDueAt.Equal(started.Add(126*time.Minute)) {
		t.Errorf("a run that waits for its researchers, shortened: got = %d %v, agents due %v, want the new end", code, out, stored.AgentsDueAt)
	}
}

// A person picking a repository in the form is shown what its experiment file states before
// anything is created: the same reading a create with from_repo makes, without what the file
// may not state.
func TestTheExperimentFileOfARepositoryIsReadWithoutCreatingAnything(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call, _ := repoAPI(t, authtest.As(auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a"}}), repos)

	rec := call("GET", "/projects/prj-a/experiment-file?repository=acme/model", nil)
	var file map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &file); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if file["repository"] != testRepository || file["name"] == "" || file["description"] == "" || len(file["metrics"].([]any)) == 0 {
		t.Fatalf("file = %v, want the repository as GitHub names it and what the file states", file)
	}
	if _, stated := file["budget_accelerator_hours"]; stated || len(store.created) != 0 {
		t.Fatalf("file = %v, created %d: what a run costs is never the file's, and a read creates nothing", file, len(store.created))
	}
	if rec := call("GET", "/projects/prj-b/experiment-file?repository=acme/model", nil); rec.Code != http.StatusNotFound || len(repos.asked) != 1 {
		t.Fatalf("another org's project: %d, asked GitHub %d times", rec.Code, len(repos.asked))
	}
	repos.files["prj-a"] = "nmae: typo\n"
	if rec := call("GET", "/projects/prj-a/experiment-file?repository=acme/model", nil); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid_experiment_file") {
		t.Fatalf("a file with an unknown key: %d %s", rec.Code, rec.Body)
	}
}

func (s *createdStore) RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error {
	raise.At = time.Now().UTC()
	pe.BaselineRaises = append(pe.BaselineRaises, raise)
	return s.UpdatePlatformExperiment(ctx, pe, domain.PlatformExpRunning, nil)
}

func (s *createdStore) ListAgentMessages(context.Context, string, string) ([]domain.AgentMessage, error) {
	return s.messages, nil
}

// The check a form makes before the create runs the create's own repository checks and creates
// nothing: what it refuses, the create refuses with the same reason.
func TestTheRepositoryCheckAnswersAsTheCreateWouldAndCreatesNothing(t *testing.T) {
	repos := &fakeRepositories{files: map[string]string{"prj-a": experimentBase}}
	store, call, _ := repoAPI(t, ownerOfA, repos)
	check := func(query string) (int, map[string]any) {
		t.Helper()
		rec := call("GET", "/projects/prj-a/repository-check"+query, nil)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
		}
		return rec.Code, out
	}
	if code, out := check("?repository=acme/model"); code != http.StatusOK || out["repository"] != testRepository || out["repository_url"] != "https://github.com/Acme/Model.git" || out["repository_account"] != nil {
		t.Fatalf("a repository named: %d %v, want it as GitHub names it", code, out)
	}
	if code, out := check(""); code != http.StatusOK || out["repository_account"] != "acme" || out["repository"] != nil {
		t.Fatalf("a new repository: %d %v, want the account it would be created on", code, out)
	}
	if code, out := check("?repository_account=second-account"); code != http.StatusOK || out["repository_account"] != "second-account" {
		t.Fatalf("a new repository on an account named: %d %v", code, out)
	}
	for name, tc := range map[string]struct {
		query, reason string
		arrange       func()
	}{
		"a repository the org does not reach": {"?repository=rival/secret", "repository_not_reachable", nil},
		"an account beside a repository":      {"?repository=acme/model&repository_account=acme", "invalid_request", nil},
		"an installation GitHub removed": {"", "installation_gone", func() {
			repos.notOn = api.Refuse(http.StatusUnprocessableEntity, "installation_gone", "the GitHub App is no longer installed on acme")
		}},
	} {
		if tc.arrange != nil {
			tc.arrange()
		}
		if code, out := check(tc.query); code != http.StatusUnprocessableEntity || out["reason"] != tc.reason {
			t.Errorf("%s: %d %v, want 422 %s", name, code, out, tc.reason)
		}
		body := map[string]any{"project_id": "prj-a", "name": "n", "description": "d", "budget_accelerator_hours": 5, "max_agents": 3, "duration_hours": 48,
			"metrics": []map[string]any{{"key": "loss", "role": "ranking", "direction": "minimize"}}}
		if tc.query != "" {
			for _, pair := range strings.Split(strings.TrimPrefix(tc.query, "?"), "&") {
				k, v, _ := strings.Cut(pair, "=")
				body[k] = v
			}
		}
		if rec := call("POST", "/platform-experiments", body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.reason) {
			t.Errorf("%s: the create answered %d %s, want 422 %s as the check did", name, rec.Code, rec.Body, tc.reason)
		}
	}
	if len(store.created) != 0 || len(repos.created) != 0 {
		t.Fatalf("created %d experiments and repositories %v, want nothing", len(store.created), repos.created)
	}
	if rec := call("GET", "/projects/prj-b/repository-check", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another org's project: %d, want 404", rec.Code)
	}
}
