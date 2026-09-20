package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

// memRepos is RepoStore in memory over the world's projects.
type memRepos struct {
	*memStore
	repos map[string]*domain.ProjectRepo
}

func (s *memRepos) AttachProjectRepo(_ context.Context, repo *domain.ProjectRepo) error {
	if _, ok := s.projects[repo.ProjectID]; !ok {
		return db.ErrProjectNotFound
	}
	repo.AttachedAt = s.now()
	stored := *repo
	s.repos[repo.ProjectID] = &stored
	return nil
}

func (s *memRepos) GetProjectRepo(_ context.Context, projectID string) (*domain.ProjectRepo, error) {
	repo, ok := s.repos[projectID]
	if !ok {
		return nil, nil
	}
	out := *repo
	return &out, nil
}

func (s *memRepos) DetachProjectRepo(_ context.Context, projectID string) error {
	if _, ok := s.repos[projectID]; !ok {
		return db.ErrProjectRepoNotFound
	}
	delete(s.repos, projectID)
	return nil
}

type minted struct {
	installation, repository int64
	access                   github.Access
}

type lookup struct {
	repo, code, redirect string
}

const (
	testRedirect    = "http://localhost:3000/settings/repository"
	modelCredential = "/git-credential?protocol=https&host=github.com&path=Acme%2FModel.git"
	otherCredential = "/git-credential?protocol=https&host=github.com&path=acme%2Fother.git"
)

// fakeApp is the GitHub App as the handler sees it. It knows two repositories on installation
// 100 and accepts one code, and fails with whatever error a test arranges.
type fakeApp struct {
	known map[string]github.Repository
	// names is what GitHub calls each repository now, by id.
	names         map[int64]string
	lookups       []lookup
	mints         []minted
	files         []minted
	repositoryErr error
	tokenErr      error
	fileErr       error
	file          string
}

func newFakeApp() *fakeApp {
	return &fakeApp{known: map[string]github.Repository{
		"acme/model": {ID: 1, FullName: "Acme/Model", InstallationID: 100},
		"acme/other": {ID: 2, FullName: "acme/other", InstallationID: 100},
		"solo/tool":  {ID: 9, FullName: "solo/tool", InstallationID: 300},
	}, names: map[int64]string{1: "Acme/Model", 2: "acme/other"}}
}

func (a *fakeApp) App() github.App {
	return github.App{Slug: "hypothesisloop", ClientID: "Iv23test", InstallURL: "https://github.test/install", AuthorizeURL: "https://github.test/authorize"}
}

func (a *fakeApp) Repository(_ context.Context, fullName, userCode, redirectURI string) (*github.Repository, error) {
	a.lookups = append(a.lookups, lookup{fullName, userCode, redirectURI})
	if a.repositoryErr != nil {
		return nil, a.repositoryErr
	}
	repo, ok := a.known[strings.ToLower(fullName)]
	if !ok {
		return nil, github.ErrRepositoryNotInstalled
	}
	return &repo, nil
}

func (a *fakeApp) Token(_ context.Context, installationID, repositoryID int64, access github.Access) (*github.Token, error) {
	if a.tokenErr != nil {
		return nil, a.tokenErr
	}
	a.mints = append(a.mints, minted{installationID, repositoryID, access})
	return &github.Token{
		Value: fmt.Sprintf("ghs_minted_%d", len(a.mints)), ExpiresAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), Repository: a.names[repositoryID],
	}, nil
}

func (a *fakeApp) File(_ context.Context, token *github.Token, repositoryID int64, ref, path string) ([]byte, error) {
	a.files = append(a.files, minted{0, repositoryID, github.Access(token.Value + "@" + ref + ":" + path)})
	if a.fileErr != nil {
		return nil, a.fileErr
	}
	return []byte(a.file), nil
}

type repoWorld struct {
	*world
	repos *memRepos
	app   *fakeApp
	svc   *Repos
}

// newRepoWorld is the accounts world with the repository routes on it. withApp false is a
// deployment that has no GitHub App.
func newRepoWorld(t *testing.T, withApp bool) *repoWorld {
	t.Helper()
	w := &repoWorld{world: newWorld(t), app: newFakeApp()}
	w.repos = &memRepos{memStore: w.store, repos: map[string]*domain.ProjectRepo{}}
	var app GitHub
	if withApp {
		app = w.app
	}
	access := auth.NewAccess(placement{memStore: w.store})
	w.svc = NewRepos(w.repos, app, access, zap.NewNop())
	inA2 := auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{w.orgA}, ProjectID: w.projectA2, PlatformExperimentID: "pe-a2", AgentID: "agent-2"}
	w.callers["agent-second-project"] = inA2
	w.callers["agent-other-org"] = auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{w.orgB}, ProjectID: w.projectB, PlatformExperimentID: "pe-b", AgentID: "agent-b"}
	for _, kind := range []string{"agent", "coordinator", "job"} {
		over := w.callers[kind]
		over.RunOver = true
		w.callers[kind+"-run-over"] = over
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", tokens{w.world}, access, zap.NewNop())
	RegisterReposHuma(doc, w.svc)
	w.router = router
	return w
}

func (w *repoWorld) seed(projectID string, repositoryID int64, repo string) {
	w.t.Helper()
	if err := w.repos.AttachProjectRepo(context.Background(), &domain.ProjectRepo{
		ProjectID: projectID, InstallationID: 100, RepositoryID: repositoryID, Repo: repo,
	}); err != nil {
		w.t.Fatal(err)
	}
}

func attachBody(repo, code string) map[string]any {
	return map[string]any{"repo": repo, "code": code, "redirect_uri": testRedirect}
}

func TestWhoMayReachAProjectsRepository(t *testing.T) {
	const (
		ok        = http.StatusOK
		noContent = http.StatusNoContent
		noToken   = http.StatusUnauthorized
		forbidden = http.StatusForbidden
		notFound  = http.StatusNotFound
	)
	callers := []string{"owner", "member", "outsider", "org-key", "project-key", "foreign-key", "staff", "staff-user",
		"agent", "coordinator", "job", "runtime", "launcher", ""}
	for _, op := range []struct {
		name    string
		request func(w *repoWorld) request
		want    map[string]int
	}{
		{"read the app", func(w *repoWorld) request { return request{"GET", "/github/app", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": ok, "org-key": ok, "project-key": ok, "foreign-key": ok, "staff": ok, "staff-user": ok}},
		{"attach", func(w *repoWorld) request {
			return request{"POST", "/projects/" + w.projectA + "/repo", attachBody("acme/model", "code")}
		}, map[string]int{"owner": ok, "member": forbidden, "outsider": notFound, "org-key": ok, "project-key": forbidden, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"read", func(w *repoWorld) request { return request{"GET", "/projects/" + w.projectA + "/repo", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": ok, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"read a sibling project's", func(w *repoWorld) request { return request{"GET", "/projects/" + w.projectA2 + "/repo", nil} },
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": notFound, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"detach", func(w *repoWorld) request { return request{"DELETE", "/projects/" + w.projectA + "/repo", nil} },
			map[string]int{"owner": noContent, "member": forbidden, "outsider": notFound, "org-key": noContent, "project-key": forbidden, "foreign-key": notFound, "staff": noContent, "staff-user": noContent}},
		{"detach a sibling project's", func(w *repoWorld) request { return request{"DELETE", "/projects/" + w.projectA2 + "/repo", nil} },
			map[string]int{"owner": noContent, "member": forbidden, "outsider": notFound, "org-key": noContent, "project-key": notFound, "foreign-key": notFound, "staff": noContent, "staff-user": noContent}},
	} {
		for _, caller := range callers {
			want, listed := op.want[caller]
			if !listed {
				want = forbidden
				if caller == "" {
					want = noToken
				}
			}
			t.Run(op.name+" as "+caller, func(t *testing.T) {
				w := newRepoWorld(t, true)
				w.seed(w.projectA, 1, "Acme/Model")
				w.seed(w.projectA2, 2, "acme/other")
				before := len(w.repos.repos)
				req := op.request(w)
				rec := w.call(caller, req.method, req.path, req.body)
				if rec.Code != want {
					t.Fatalf("got = %d %s, want = %d", rec.Code, rec.Body, want)
				}
				if want >= 300 {
					if len(w.repos.repos) != before || len(w.app.lookups) != 0 {
						t.Fatalf("a refused call changed the repositories or reached GitHub: %d rows, lookups %+v", len(w.repos.repos), w.app.lookups)
					}
					if strings.Contains(rec.Body.String(), "Acme") || strings.Contains(rec.Body.String(), "acme") {
						t.Fatalf("a refusal names the repository: %s", rec.Body)
					}
				}
			})
		}
	}
}

// A project of another org and one that does not exist must read alike, on every route.
func TestAnotherOrgsProjectReadsAsAbsent(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectB, 1, "Acme/Model")
	for _, req := range []request{
		{"GET", "/projects/%s/repo", nil}, {"DELETE", "/projects/%s/repo", nil}, {"POST", "/projects/%s/repo", attachBody("acme/model", "code")},
	} {
		foreign := w.call("owner", req.method, fmt.Sprintf(req.path, w.projectB), req.body)
		absent := w.call("owner", req.method, fmt.Sprintf(req.path, "prj_missing"), req.body)
		if foreign.Code != http.StatusNotFound || foreign.Code != absent.Code || foreign.Body.String() != absent.Body.String() {
			t.Errorf("%s: another org's = %d %s, absent = %d %s, want the same 404", req.method, foreign.Code, foreign.Body, absent.Code, absent.Body)
		}
	}
	if _, still := w.repos.repos[w.projectB]; !still || len(w.app.lookups) != 0 {
		t.Fatal("org A's owner changed org B's repository or reached GitHub about it")
	}
}

func TestAttachRecordsWhatGitHubStates(t *testing.T) {
	w := newRepoWorld(t, true)
	var view RepoView
	w.ok("owner", "POST", "/projects/"+w.projectA+"/repo", attachBody("acme/model", "the-code"), http.StatusOK, &view)
	want := lookup{"acme/model", "the-code", testRedirect}
	if len(w.app.lookups) != 1 || w.app.lookups[0] != want {
		t.Fatalf("GitHub was asked %+v, want %+v", w.app.lookups, want)
	}
	if view.ProjectID != w.projectA || view.InstallationID != 100 || view.RepositoryID != 1 || view.Repo != "Acme/Model" ||
		view.CloneURL != "https://github.com/Acme/Model.git" || view.AttachedAt.IsZero() {
		t.Fatalf("view = %+v", view)
	}
	var read RepoView
	w.ok("member", "GET", "/projects/"+w.projectA+"/repo", nil, http.StatusOK, &read)
	if read != view {
		t.Fatalf("read back = %+v, want = %+v", read, view)
	}

	// The installation is the one GitHub names for the repository, whichever that is.
	var solo RepoView
	w.ok("owner", "POST", "/projects/"+w.projectA+"/repo", attachBody("solo/tool", "solo-code"), http.StatusOK, &solo)
	if solo.InstallationID != 300 || solo.RepositoryID != 9 {
		t.Fatalf("view = %+v, want the installation GitHub stated for that repository", solo)
	}

	// A second attach replaces the first, and a second project may sit on the same installation.
	w.ok("owner", "POST", "/projects/"+w.projectA+"/repo", attachBody("acme/other", "next-code"), http.StatusOK, &view)
	w.ok("owner", "POST", "/projects/"+w.projectA2+"/repo", attachBody("acme/model", "third-code"), http.StatusOK, nil)
	if view.RepositoryID != 2 || w.repos.repos[w.projectA].RepositoryID != 2 || w.repos.repos[w.projectA2].RepositoryID != 1 {
		t.Fatalf("after re-attaching: %+v and %+v", w.repos.repos[w.projectA], w.repos.repos[w.projectA2])
	}

	w.ok("owner", "DELETE", "/projects/"+w.projectA+"/repo", nil, http.StatusNoContent, nil)
	for _, method := range []string{"GET", "DELETE"} {
		gone := w.call("owner", method, "/projects/"+w.projectA+"/repo", nil)
		if gone.Code != http.StatusNotFound || reasonOf(t, gone) != ReasonNoRepository || !strings.Contains(gone.Body.String(), "no repository is attached") {
			t.Fatalf("%s after detach: got = %d %s, want 404 with reason no_repository", method, gone.Code, gone.Body)
		}
	}
	// A project that is not there carries no reason, so a client tells the two 404s apart.
	for _, project := range []string{w.projectB, "prj_missing"} {
		absent := w.call("owner", "GET", "/projects/"+project+"/repo", nil)
		if absent.Code != http.StatusNotFound || reasonOf(t, absent) != "" {
			t.Fatalf("project %s: got = %d %s, want 404 with no reason", project, absent.Code, absent.Body)
		}
	}
	if _, kept := w.repos.repos[w.projectA2]; !kept {
		t.Fatal("detaching one project's repository took the other's")
	}
}

func TestAttachRefusesARequestItCannotCheck(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"no repository":         attachBody("", "code"),
		"a url for a name":      attachBody("https://github.com/acme/model", "code"),
		"a path out of the api": attachBody("acme/../../app/installations", "code"),
		"no code":               attachBody("acme/model", ""),
		"no redirect":           map[string]any{"repo": "acme/model", "code": "code", "redirect_uri": ""},
	} {
		w := newRepoWorld(t, true)
		rec := w.call("owner", "POST", "/projects/"+w.projectA+"/repo", body)
		if rec.Code != http.StatusBadRequest || len(w.app.lookups) != 0 || len(w.repos.repos) != 0 {
			t.Errorf("%s: got = %d %s with lookups %+v, want 400 before GitHub is asked, so the code is not spent", name, rec.Code, rec.Body, w.app.lookups)
		}
	}
	// The installation is not the client's to name: a body that still does is refused whole.
	w := newRepoWorld(t, true)
	named := attachBody("acme/model", "code")
	named["installation_id"] = 100
	if rec := w.call("owner", "POST", "/projects/"+w.projectA+"/repo", named); rec.Code != http.StatusUnprocessableEntity || len(w.app.lookups) != 0 {
		t.Errorf("a body naming an installation: got = %d %s, want 422 before GitHub is asked", rec.Code, rec.Body)
	}
}

// reasonOf is the machine-readable reason of a refusal, empty when it carries none.
func reasonOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %s", rec.Body)
	}
	return body.Reason
}

const leak = "ghs_a_token_in_an_error"

func TestWhatGitHubRefusesOnAttach(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		repo   string
		want   int
		reason string
	}{
		"the person cannot push":                   {github.ErrUserCannotPush, "acme/model", http.StatusForbidden, ""},
		"single sign-on blocks the person's token": {github.ErrUserBlocked, "acme/model", http.StatusForbidden, ""},
		"the installation withholds the access":    {github.ErrPermissionNotGranted, "acme/model", http.StatusConflict, ""},
		"the app is not on that repository":        {github.ErrRepositoryNotInstalled, "acme/model", http.StatusUnprocessableEntity, ReasonAppNotInstalled},
		"a repository github does not know":        {nil, "acme/nowhere", http.StatusUnprocessableEntity, ReasonAppNotInstalled},
		"the code was used":                        {github.ErrCodeRejected, "acme/model", http.StatusBadRequest, ""},
		"the installation was suspended":           {github.ErrInstallationGone, "acme/model", http.StatusConflict, ""},
		"github rate limits":                       {fmt.Errorf("%w (%s)", github.ErrRateLimited, leak), "acme/model", http.StatusServiceUnavailable, ""},
		"the app key expired":                      {fmt.Errorf("%w (%s)", github.ErrAppCredentials, leak), "acme/model", http.StatusBadGateway, ""},
		"something else":                           {errors.New("dial tcp: " + leak), "acme/model", http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := newRepoWorld(t, true)
			w.app.repositoryErr = tc.err
			rec := w.call("owner", "POST", "/projects/"+w.projectA+"/repo", attachBody(tc.repo, "code"))
			if rec.Code != tc.want || reasonOf(t, rec) != tc.reason {
				t.Fatalf("got = %d %s, want = %d with reason %q", rec.Code, rec.Body, tc.want, tc.reason)
			}
			if tc.reason == ReasonAppNotInstalled && !strings.Contains(rec.Body.String(), "install") {
				t.Fatalf("the answer does not say to install the App: %s", rec.Body)
			}
			if len(w.repos.repos) != 0 {
				t.Fatalf("a refused attach was stored: %+v", w.repos.repos)
			}
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatalf("the answer repeats the failure's text: %s", rec.Body)
			}
		})
	}
}

func TestADeploymentWithoutAnAppAnswersSoAfterTheAccessCheck(t *testing.T) {
	w := newRepoWorld(t, false)
	w.seed(w.projectA, 1, "Acme/Model")
	const unavailable = http.StatusServiceUnavailable
	for _, tc := range []struct {
		name, caller, method, path string
		body                       any
		want                       int
	}{
		{"the app", "owner", "GET", "/github/app", nil, unavailable},
		{"attach", "owner", "POST", "/projects/" + w.projectA + "/repo", attachBody("acme/model", "code"), unavailable},
		{"attach to another org's project", "owner", "POST", "/projects/" + w.projectB + "/repo", attachBody("acme/model", "code"), http.StatusNotFound},
		{"attach as a member", "member", "POST", "/projects/" + w.projectA + "/repo", attachBody("acme/model", "code"), http.StatusForbidden},
		{"a credential", "agent", "GET", modelCredential, nil, unavailable},
		{"a credential for a person", "owner", "GET", modelCredential, nil, http.StatusForbidden},
		{"a credential with nothing to it", "agent-second-project", "GET", otherCredential, nil, http.StatusNotFound},
		{"reading what is attached", "owner", "GET", "/projects/" + w.projectA + "/repo", nil, http.StatusOK},
		{"detaching it", "owner", "DELETE", "/projects/" + w.projectA + "/repo", nil, http.StatusNoContent},
	} {
		rec := w.call(tc.caller, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", tc.name, rec.Code, rec.Body, tc.want)
		}
		if tc.want == unavailable && (reasonOf(t, rec) != ReasonGitHubNotConfigured || !strings.Contains(rec.Body.String(), "no GitHub App configured")) {
			t.Errorf("%s: got = %s, want its own sentence with reason github_not_configured", tc.name, rec.Body)
		}
	}
}

func TestAGitCredentialGoesToWhatThePlatformLaunchedAndIsLimitedToItsProject(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	w.seed(w.projectA2, 2, "acme/other")
	for _, tc := range []struct {
		caller, call string
		want         minted
		path         string
	}{
		{"agent", modelCredential, minted{100, 1, github.Write}, "Acme/Model.git"},
		{"coordinator", modelCredential, minted{100, 1, github.Read}, "Acme/Model.git"},
		{"job", modelCredential, minted{100, 1, github.Read}, "Acme/Model.git"},
		// The second project shares the installation and gets its own repository alone.
		{"agent-second-project", otherCredential, minted{100, 2, github.Write}, "acme/other.git"},
	} {
		before := len(w.app.mints)
		rec := w.call(tc.caller, "GET", tc.call, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got = %d %s", tc.caller, rec.Code, rec.Body)
		}
		var credential GitCredential
		w.ok(tc.caller, "GET", tc.call, nil, http.StatusOK, &credential)
		if len(w.app.mints) != before+2 || w.app.mints[before] != tc.want || w.app.mints[before+1] != tc.want {
			t.Fatalf("%s: minted %+v, want %+v on each of the two calls", tc.caller, w.app.mints[before:], tc.want)
		}
		if credential.Protocol != "https" || credential.Host != "github.com" || credential.Path != tc.path ||
			credential.Username != "x-access-token" || credential.Password != fmt.Sprintf("ghs_minted_%d", before+2) || credential.ExpiresAt.IsZero() {
			t.Fatalf("%s: credential = %+v", tc.caller, credential)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.caller, got)
		}
	}
}

// Nothing is minted until the address git asked about is known to be the project's repository.
func TestAGitCredentialIsMintedOnlyForTheAddressOfTheProjectsRepository(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	w.seed(w.projectA2, 2, "acme/other")
	ask := func(protocol, host, path string) string {
		return "/git-credential?" + url.Values{"protocol": {protocol}, "host": {host}, "path": {path}}.Encode()
	}
	for _, path := range []string{"Acme/Model.git", "Acme/Model", "acme/model.git", "/Acme/Model.git/", "Acme/Model.git/info/lfs"} {
		var credential GitCredential
		w.ok("agent", "GET", ask("https", "github.com", path), nil, http.StatusOK, &credential)
		if credential.Path != strings.Trim(path, "/") || credential.Password == "" {
			t.Errorf("path %q: credential path = %q", path, credential.Path)
		}
	}
	w.ok("agent", "GET", ask("https", "GitHub.com", "Acme/Model.git"), nil, http.StatusOK, nil)
	served := len(w.app.mints)
	for name, call := range map[string]string{
		"a sibling project's repository": ask("https", "github.com", "acme/other.git"),
		"a repository under it":          ask("https", "github.com", "Acme/Model.git/extra"),
		"a repository that starts so":    ask("https", "github.com", "Acme/Model.github"),
		"the lfs path of another":        ask("https", "github.com", "acme/other.git/info/lfs"),
		"another host":                   ask("https", "evil.example", "Acme/Model.git"),
		"a host that starts so":          ask("https", "github.com.evil.example", "Acme/Model.git"),
		"plain http":                     ask("http", "github.com", "Acme/Model.git"),
		"no protocol":                    ask("", "github.com", "Acme/Model.git"),
		"no path":                        ask("https", "github.com", ""),
	} {
		rec := w.call("agent", "GET", call, nil)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got = %d %s, want a refusal", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "ghs_") {
			t.Errorf("%s: the refusal holds a token: %s", name, rec.Body)
		}
	}
	for name, call := range map[string]string{
		"no parameters": "/git-credential", "no path parameter": "/git-credential?protocol=https&host=github.com",
		"a parameter it does not know": modelCredential + "&username=x",
	} {
		if rec := w.call("agent", "GET", call, nil); rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s: got = %d %s, want the request refused", name, rec.Code, rec.Body)
		}
	}
	if len(w.app.mints) != served {
		t.Fatalf("%d tokens were minted for addresses that are not the repository's", len(w.app.mints)-served)
	}
}

func TestNoGitCredentialForAnyoneElse(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	for caller, want := range map[string]int{
		"owner": http.StatusForbidden, "member": http.StatusForbidden, "outsider": http.StatusForbidden, "org-key": http.StatusForbidden,
		"project-key": http.StatusForbidden, "staff": http.StatusForbidden, "staff-user": http.StatusForbidden,
		"runtime": http.StatusForbidden, "launcher": http.StatusForbidden, "": http.StatusUnauthorized,
		// An agent of another org has a project of its own, with nothing attached.
		"agent-other-org": http.StatusNotFound,
	} {
		if rec := w.call(caller, "GET", modelCredential, nil); rec.Code != want {
			t.Errorf("%s: got = %d %s, want = %d", caller, rec.Code, rec.Body, want)
		}
	}
	if len(w.app.mints) != 0 {
		t.Fatalf("a token was minted for a refused caller: %+v", w.app.mints)
	}
}

// The token of a run that is over still works for its wind-down. A credential would outlive it
// by an hour, so none is minted: for an agent, a coordinator and a job alike.
func TestNoGitCredentialOnceTheRunIsOver(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	for _, caller := range []string{"agent-run-over", "coordinator-run-over", "job-run-over"} {
		rec := w.call(caller, "GET", modelCredential, nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "is over") || len(w.app.mints) != 0 {
			t.Errorf("%s: got = %d %s with %d tokens minted, want 403 and none", caller, rec.Code, rec.Body, len(w.app.mints))
		}
	}
	for _, caller := range []string{"agent", "coordinator", "job"} {
		w.ok(caller, "GET", modelCredential, nil, http.StatusOK, nil)
	}
}

func TestARepositoryDetachedMidRunStopsCredentialsAtOnce(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	w.ok("agent", "GET", modelCredential, nil, http.StatusOK, nil)
	w.ok("owner", "DELETE", "/projects/"+w.projectA+"/repo", nil, http.StatusNoContent, nil)
	for _, caller := range []string{"agent", "job", "coordinator"} {
		rec := w.call(caller, "GET", modelCredential, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no repository is attached") {
			t.Errorf("%s after the detach: got = %d %s, want 404", caller, rec.Code, rec.Body)
		}
	}
	if len(w.app.mints) != 1 {
		t.Fatalf("tokens minted = %d, want only the one from before the detach", len(w.app.mints))
	}
	// Attached again, to another repository, the same agent gets that one and no longer the first.
	w.ok("owner", "POST", "/projects/"+w.projectA+"/repo", attachBody("acme/other", "code"), http.StatusOK, nil)
	attachMints := len(w.app.mints)
	var credential GitCredential
	w.ok("agent", "GET", otherCredential, nil, http.StatusOK, &credential)
	if credential.Path != "acme/other.git" || w.app.mints[attachMints].repository != 2 {
		t.Fatalf("after re-attaching: credential = %+v, minted %+v", credential, w.app.mints[attachMints])
	}
	if rec := w.call("agent", "GET", modelCredential, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the repository attached before: got = %d %s, want 404", rec.Code, rec.Body)
	}
}

// The repository is held by its id, so a rename leaves tokens working while every address the
// agents hold is stale. That is refused, and names where the repository went.
func TestARenamedRepositoryIsRefusedUntilItIsAttachedAgain(t *testing.T) {
	w := newRepoWorld(t, true)
	w.seed(w.projectA, 1, "Acme/Model")
	w.app.names[1] = "newowner/model"
	for _, caller := range []string{"agent", "job", "coordinator"} {
		rec := w.call(caller, "GET", modelCredential, nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "newowner/model") || strings.Contains(rec.Body.String(), "ghs_") {
			t.Errorf("%s: got = %d %s, want 409 naming the new address and no token", caller, rec.Code, rec.Body)
		}
	}
	if _, err := w.svc.ExperimentBase(context.Background(), w.projectA); err == nil || len(w.app.files) != 0 {
		t.Fatalf("the experiment file of a renamed repository was read: %v", err)
	}
	// A change of case alone is a rename too: the stored name is the address handed out.
	w.app.names[1] = "acme/model"
	if rec := w.call("agent", "GET", modelCredential, nil); rec.Code != http.StatusConflict {
		t.Fatalf("a case-only rename: got = %d %s, want 409", rec.Code, rec.Body)
	}
}

func TestWhatGitHubRefusesOnACredential(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"the installation was revoked":                  {github.ErrInstallationGone, http.StatusConflict},
		"the repository was taken out of the selection": {github.ErrRepositoryNotInstalled, http.StatusConflict},
		"the installation withholds the access":         {github.ErrPermissionNotGranted, http.StatusConflict},
		"github rate limits":                            {github.ErrRateLimited, http.StatusServiceUnavailable},
		"the app key expired":                           {fmt.Errorf("%w (%s)", github.ErrAppCredentials, leak), http.StatusBadGateway},
		"something else":                                {errors.New("unexpected " + leak), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			w := newRepoWorld(t, true)
			w.seed(w.projectA, 1, "Acme/Model")
			w.app.tokenErr = tc.err
			rec := w.call("agent", "GET", modelCredential, nil)
			if rec.Code != tc.want {
				t.Fatalf("got = %d %s, want = %d", rec.Code, rec.Body, tc.want)
			}
			if strings.Contains(rec.Body.String(), leak) || strings.Contains(rec.Body.String(), "password") {
				t.Fatalf("the refusal holds a credential or the failure's text: %s", rec.Body)
			}
			// The row stays: an installation restored on GitHub works again with no step here.
			if _, kept := w.repos.repos[w.projectA]; !kept {
				t.Fatal("a failed mint detached the repository")
			}
			w.app.tokenErr = nil
			w.ok("agent", "GET", modelCredential, nil, http.StatusOK, nil)
		})
	}
}

func TestExperimentBaseAndCloneURL(t *testing.T) {
	ctx := context.Background()
	w := newRepoWorld(t, true)
	if url, err := w.svc.CloneURL(ctx, w.projectA); err != nil || url != "" {
		t.Fatalf("clone url with nothing attached = %q, %v, want empty", url, err)
	}
	status := func(err error) int {
		var refusal huma.StatusError
		if !errors.As(err, &refusal) {
			t.Fatalf("error %v is no API refusal", err)
		}
		return refusal.GetStatus()
	}
	if _, err := w.svc.ExperimentBase(ctx, w.projectA); status(err) != http.StatusUnprocessableEntity || len(w.app.files) != 0 {
		t.Fatalf("base with nothing attached: got = %v", err)
	}
	w.seed(w.projectA, 1, "Acme/Model")
	if url, err := w.svc.CloneURL(ctx, w.projectA); err != nil || url != "https://github.com/Acme/Model.git" {
		t.Fatalf("clone url = %q, %v", url, err)
	}
	w.app.file = "name: from the repo\n"
	data, err := w.svc.ExperimentBase(ctx, w.projectA)
	want := minted{0, 1, github.Access("ghs_minted_1@:" + domain.ExperimentFile)}
	if err != nil || string(data) != w.app.file || len(w.app.files) != 1 || w.app.files[0] != want {
		t.Fatalf("base = %q, %v, read as %+v, want the file at the default branch head", data, err, w.app.files)
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"no such file":         {github.ErrFileNotFound, http.StatusUnprocessableEntity},
		"a directory":          {github.ErrNotAFile, http.StatusUnprocessableEntity},
		"installation gone":    {github.ErrInstallationGone, http.StatusConflict},
		"repository taken out": {github.ErrRepositoryNotInstalled, http.StatusConflict},
		"rate limited":         {github.ErrRateLimited, http.StatusServiceUnavailable},
		"too large":            {github.ErrFileTooLarge, http.StatusUnprocessableEntity},
		"anything else":        {errors.New(leak), http.StatusInternalServerError},
	} {
		w.app.fileErr = tc.err
		_, err := w.svc.ExperimentBase(ctx, w.projectA)
		if status(err) != tc.want || strings.Contains(err.Error(), leak) {
			t.Errorf("%s: got = %v, want status %d", name, err, tc.want)
		}
	}
	none := newRepoWorld(t, false)
	none.seed(none.projectA, 1, "Acme/Model")
	if _, err := none.svc.ExperimentBase(ctx, none.projectA); status(err) != http.StatusServiceUnavailable {
		t.Fatalf("base on a deployment without an App: got = %v, want 503", err)
	}
}
