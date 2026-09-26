package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

// memInstallations is InstallationStore in memory over the world's orgs and projects.
type memInstallations struct {
	*memStore
	connected map[string][]*domain.OrgGitHubInstallation
}

func (s *memInstallations) ConnectGitHubInstallation(_ context.Context, in *domain.OrgGitHubInstallation) error {
	if _, ok := s.orgs[in.OrgID]; !ok {
		return db.ErrOrgNotFound
	}
	for _, have := range s.connected[in.OrgID] {
		if have.InstallationID == in.InstallationID {
			have.AccountLogin = in.AccountLogin
			*in = *have
			return nil
		}
	}
	in.ConnectedAt = s.now()
	stored := *in
	s.connected[in.OrgID] = append(s.connected[in.OrgID], &stored)
	return nil
}

func (s *memInstallations) ListGitHubInstallations(_ context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error) {
	out := []*domain.OrgGitHubInstallation{}
	for _, have := range s.connected[orgID] {
		one := *have
		out = append(out, &one)
	}
	return out, nil
}

func (s *memInstallations) GitHubInstallationOfAccount(_ context.Context, orgID, login string) (*domain.OrgGitHubInstallation, error) {
	for _, have := range s.connected[orgID] {
		if strings.EqualFold(have.AccountLogin, login) {
			one := *have
			return &one, nil
		}
	}
	return nil, nil
}

func (s *memInstallations) DisconnectGitHubInstallation(_ context.Context, orgID string, id int64) error {
	for i, have := range s.connected[orgID] {
		if have.InstallationID == id {
			s.connected[orgID] = slices.Delete(s.connected[orgID], i, i+1)
			return nil
		}
	}
	return db.ErrGitHubInstallationNotFound
}

// experiments says which repository each platform experiment names, and in which org.
type experiments map[string][2]string

func (e experiments) PlatformExperimentRepository(_ context.Context, id string) (string, string, error) {
	found, ok := e[id]
	if !ok {
		return "", "", db.ErrPlatformExperimentNotFound
	}
	return found[0], found[1], nil
}

type minted struct {
	installation int64
	repository   string
	access       github.Access
}

type asked struct{ code, redirect string }

const testRedirect = "http://localhost:3000/settings/github"

// fakeApp is the GitHub App as the service sees it. Installation 100 is on the account Acme and
// covers Acme/Model and acme/other; installation 200 is on rival and covers rival/secret. Each
// code belongs to a person who reaches some of them, and controls the account of each one they
// reach unless guests says they are only a collaborator on a repository there.
type fakeApp struct {
	reaches map[string][]github.Installation
	guests  map[string]map[int64]bool
	covers  map[int64][]github.Repository
	asked   []asked
	listed  []int64
	mints   []minted
	files   []string
	userErr error
	listErr map[int64]error
	// tokenErr fails every mint, fileErr every file read.
	tokenErr error
	fileErr  error
	file     string
	// created holds each repository created, as "installation name", and createErr fails it.
	created   []string
	createErr error
}

func (a *fakeApp) CreateRepository(_ context.Context, installationID int64, name, _ string) (*github.Repository, error) {
	if a.createErr != nil {
		return nil, a.createErr
	}
	a.created = append(a.created, fmt.Sprintf("%d %s", installationID, name))
	return &github.Repository{FullName: "Acme/" + name, Private: true}, nil
}

var (
	acme  = github.Installation{ID: 100, AccountLogin: "Acme"}
	rival = github.Installation{ID: 200, AccountLogin: "rival"}
)

func newFakeApp() *fakeApp {
	return &fakeApp{
		reaches: map[string][]github.Installation{"code-acme": {acme}, "code-both": {acme, rival}, "code-none": {}, "code-guest": {acme}, "code-acme-admin-rival-guest": {acme, rival}},
		guests:  map[string]map[int64]bool{"code-guest": {100: true}, "code-acme-admin-rival-guest": {200: true}},
		covers: map[int64][]github.Repository{
			100: {{FullName: "acme/other"}, {FullName: "Acme/Model", Private: true}},
			200: {{FullName: "rival/secret", Private: true}},
		},
		listErr: map[int64]error{},
	}
}

func (a *fakeApp) App() github.App {
	return github.App{Slug: "hypothesisloop", ClientID: "Iv23test", InstallURL: "https://github.test/install", AuthorizeURL: "https://github.test/authorize"}
}

func (a *fakeApp) UserInstallations(_ context.Context, code, redirect string) ([]github.Installation, error) {
	a.asked = append(a.asked, asked{code, redirect})
	if a.userErr != nil {
		return nil, a.userErr
	}
	reached, ok := a.reaches[code]
	if !ok {
		return nil, github.ErrCodeRejected
	}
	out := []github.Installation{}
	for _, one := range reached {
		one.Controlled = !a.guests[code][one.ID]
		out = append(out, one)
	}
	return out, nil
}

func (a *fakeApp) Repositories(_ context.Context, installationID int64) ([]github.Repository, error) {
	a.listed = append(a.listed, installationID)
	if err := a.listErr[installationID]; err != nil {
		return nil, err
	}
	covered, ok := a.covers[installationID]
	if !ok {
		return nil, github.ErrInstallationGone
	}
	return covered, nil
}

func (a *fakeApp) Token(_ context.Context, installationID int64, fullName string, access github.Access) (*github.Token, error) {
	if a.tokenErr != nil {
		return nil, a.tokenErr
	}
	covered, ok := a.covers[installationID]
	if !ok {
		return nil, github.ErrInstallationGone
	}
	for _, repo := range covered {
		if strings.EqualFold(repo.FullName, fullName) {
			a.mints = append(a.mints, minted{installationID, repo.FullName, access})
			return &github.Token{
				Value: fmt.Sprintf("ghs_minted_%d", len(a.mints)), ExpiresAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), Repository: repo.FullName,
			}, nil
		}
	}
	return nil, github.ErrRepositoryNotInstalled
}

func (a *fakeApp) File(_ context.Context, token *github.Token, ref, path string) ([]byte, error) {
	a.files = append(a.files, token.Value+"@"+token.Repository+":"+ref+":"+path)
	if a.fileErr != nil {
		return nil, a.fileErr
	}
	return []byte(a.file), nil
}

type githubWorld struct {
	*world
	installations *memInstallations
	experiments   experiments
	app           *fakeApp
	svc           *Repositories
}

// newGitHubWorld is the accounts world with the GitHub routes on it. Platform experiment pe-a,
// which the world's agent, coordinator and job belong to, names Acme/Model. withApp false is a
// deployment that has no GitHub App.
func newGitHubWorld(t *testing.T, withApp bool) *githubWorld {
	t.Helper()
	w := &githubWorld{world: newWorld(t), app: newFakeApp()}
	w.installations = &memInstallations{memStore: w.store, connected: map[string][]*domain.OrgGitHubInstallation{}}
	w.experiments = experiments{"pe-a": {w.orgA, "Acme/Model"}, "pe-bare": {w.orgA, ""}, "pe-b": {w.orgB, "Acme/Model"}}
	var app GitHubApp
	if withApp {
		app = w.app
	}
	access := auth.NewAccess(placement{memStore: w.store})
	w.svc = NewRepositories(w.installations, w.experiments, app, access, zap.NewNop())
	w.callers["agent-bare"] = auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{w.orgA}, ProjectID: w.projectA, PlatformExperimentID: "pe-bare", AgentID: "agent-2"}
	w.callers["agent-other-org"] = auth.Principal{Kind: auth.KindAgent, OrgIDs: []string{w.orgB}, ProjectID: w.projectB, PlatformExperimentID: "pe-b", AgentID: "agent-b"}
	for _, kind := range []string{"agent", "coordinator", "job"} {
		over := w.callers[kind]
		over.RunOver = true
		w.callers[kind+"-run-over"] = over
	}
	router := chi.NewRouter()
	doc := apidocs.New(router, "test", "1.0.0", "", tokens{w.world}, access, zap.NewNop())
	RegisterRepositoriesHuma(doc, w.svc)
	w.router = router
	return w
}

// connect records an installation as the org's, as a connect that went through leaves it.
func (w *githubWorld) connect(orgID string, installation github.Installation) {
	w.t.Helper()
	err := w.installations.ConnectGitHubInstallation(context.Background(), &domain.OrgGitHubInstallation{
		OrgID: orgID, InstallationID: installation.ID, AccountLogin: installation.AccountLogin, ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "u-owner"},
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

func connectBody(code string, installation int64) map[string]any {
	body := map[string]any{"code": code, "redirect_uri": testRedirect}
	if installation != 0 {
		body["installation_id"] = installation
	}
	return body
}

// refusalOf is the reason and message of a refusal, which must be all its envelope holds
// besides what the reason says it carries.
func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) (reason, message string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal %d is not JSON: %s", rec.Code, rec.Body)
	}
	reason, _ = body["reason"].(string)
	message, _ = body["message"].(string)
	return reason, message
}

func (w *githubWorld) refused(caller, method, path string, body any, status int, reason string) string {
	w.t.Helper()
	rec := w.call(caller, method, path, body)
	got, message := refusalOf(w.t, rec)
	if rec.Code != status || got != reason || message == "" {
		w.t.Fatalf("%s %s %s: got = %d %s, want = %d %s with a message", caller, method, path, rec.Code, rec.Body, status, reason)
	}
	return message
}

func credentialBody(path string) map[string]any {
	return map[string]any{"protocol": "https", "host": "github.com", "path": path}
}

var modelCredential = credentialBody("Acme/Model.git")

func TestWhoMayReachAnOrgsGitHub(t *testing.T) {
	const ok, created, gone, forbidden, notFound, noToken = http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized
	derived := map[string]int{"agent": forbidden, "coordinator": forbidden, "job": forbidden, "runtime": forbidden, "launcher": forbidden, "": noToken}
	for _, tc := range []struct {
		name, method, path string
		body               func() any
		want               map[string]int
	}{
		{"the app", "GET", "/github/app", nil,
			map[string]int{"owner": ok, "member": ok, "outsider": ok, "org-key": ok, "project-key": ok, "foreign-key": ok, "staff": ok, "staff-user": ok}},
		{"connect", "POST", "/orgs/<a>/github-installations", func() any { return connectBody("code-acme", 100) },
			map[string]int{"owner": created, "member": forbidden, "outsider": notFound, "org-key": created, "project-key": forbidden, "foreign-key": notFound, "staff": created, "staff-user": created}},
		{"list installations", "GET", "/orgs/<a>/github-installations", nil,
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": ok, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"list repositories", "GET", "/orgs/<a>/github-repositories", nil,
			map[string]int{"owner": ok, "member": ok, "outsider": notFound, "org-key": ok, "project-key": ok, "foreign-key": notFound, "staff": ok, "staff-user": ok}},
		{"disconnect", "DELETE", "/orgs/<a>/github-installations/100", nil,
			map[string]int{"owner": gone, "member": forbidden, "outsider": notFound, "org-key": gone, "project-key": forbidden, "foreign-key": notFound, "staff": gone, "staff-user": gone}},
	} {
		for caller, want := range derived {
			if _, stated := tc.want[caller]; !stated {
				tc.want[caller] = want
			}
		}
		for caller, want := range tc.want {
			w := newGitHubWorld(t, true)
			w.connect(w.orgA, acme)
			var body any
			if tc.body != nil {
				body = tc.body()
			}
			rec := w.call(caller, tc.method, strings.ReplaceAll(tc.path, "<a>", w.orgA), body)
			if rec.Code != want {
				t.Errorf("%s as %q: got = %d %s, want = %d", tc.name, caller, rec.Code, rec.Body, want)
			}
			// Whoever is refused leaves the org's installations as they were and learns nothing.
			if want >= 400 && (len(w.installations.connected[w.orgA]) != 1 || len(w.app.asked) != 0 || len(w.app.listed) != 0) {
				t.Errorf("%s as %q: a refused call reached the store or GitHub", tc.name, caller)
			}
		}
	}
}

func TestConnectRecordsAnInstallationThePersonReachesOnGitHub(t *testing.T) {
	w := newGitHubWorld(t, true)
	path := "/orgs/" + w.orgA + "/github-installations"
	var connected domain.OrgGitHubInstallation
	w.ok("owner", "POST", path, connectBody("code-both", 200), http.StatusCreated, &connected)
	want := domain.OrgGitHubInstallation{OrgID: w.orgA, InstallationID: 200, AccountLogin: "rival", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "u-owner", Name: "Owner"}, ConnectedAt: connected.ConnectedAt}
	if connected != want || connected.ConnectedAt.IsZero() {
		t.Fatalf("connected = %+v, want %+v", connected, want)
	}
	if len(w.app.asked) != 1 || w.app.asked[0] != (asked{"code-both", testRedirect}) {
		t.Fatalf("GitHub was asked %+v, want the code with its redirect once", w.app.asked)
	}

	// With no installation named, the only one the person reaches is the one meant.
	w.ok("owner", "POST", path, connectBody("code-acme", 0), http.StatusCreated, &connected)
	if connected.InstallationID != 100 || connected.AccountLogin != "Acme" {
		t.Fatalf("connected with none named = %+v, want the one installation the person reaches", connected)
	}
	// Connecting what is connected answers with it as it was first connected.
	first := connected
	w.store.clock = w.store.clock.Add(time.Hour)
	w.ok("org-key", "POST", path, connectBody("code-acme", 100), http.StatusCreated, &connected)
	if connected != first || len(w.installations.connected[w.orgA]) != 2 {
		t.Fatalf("connected again = %+v with %d rows, want %+v and two", connected, len(w.installations.connected[w.orgA]), first)
	}
	// An API key that connects is named as who did.
	w.installations.connected[w.orgA] = nil
	w.ok("org-key", "POST", path, connectBody("code-acme", 100), http.StatusCreated, &connected)
	if connected.ConnectedActor != (domain.Actor{Kind: domain.ActorKey, ID: "k-org"}) {
		t.Fatalf("connected by a key = %+v, want the key named", connected)
	}
	if len(w.installations.connected[w.orgB]) != 0 {
		t.Fatal("connecting to one org connected another")
	}
}

func TestConnectAsksWhichInstallationWhenThePersonReachesSeveral(t *testing.T) {
	w := newGitHubWorld(t, true)
	rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/github-installations", connectBody("code-both", 0))
	var body struct {
		Reason, Message string
		Installations   []github.Installation
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusConflict {
		t.Fatalf("got = %d %s, want 409", rec.Code, rec.Body)
	}
	if body.Reason != ReasonInstallationChoiceRequired || body.Message == "" || !slices.Equal(body.Installations, []github.Installation{acme, rival}) {
		t.Fatalf("answer = %s, want installation_choice_required with both installations", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"installation_id":100`) || !strings.Contains(rec.Body.String(), `"account_login":"Acme"`) {
		t.Fatalf("the installations are not listed by installation_id and account_login: %s", rec.Body)
	}
	if len(w.installations.connected[w.orgA]) != 0 {
		t.Fatal("an installation was connected though none was chosen")
	}
	// Only what the person controls is a choice: an admin of Acme who is a collaborator on one
	// repository of rival is asked nothing, and cannot connect rival by naming it.
	w.refused("owner", "POST", "/orgs/"+w.orgA+"/github-installations", connectBody("code-acme-admin-rival-guest", 200), http.StatusUnprocessableEntity, ReasonInstallationNotControlled)
	var connected domain.OrgGitHubInstallation
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/github-installations", connectBody("code-acme-admin-rival-guest", 0), http.StatusCreated, &connected)
	if connected.InstallationID != 100 || len(w.installations.connected[w.orgA]) != 1 {
		t.Fatalf("connected = %+v, want Acme's installation alone", connected)
	}
}

func TestConnectRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body   map[string]any
		err    error
		status int
		reason string
		asks   int
	}{
		"a person who reaches no installation":    {connectBody("code-none", 0), nil, 422, ReasonAppNotInstalled, 1},
		"a person who reaches none, naming one":   {connectBody("code-none", 100), nil, 422, ReasonAppNotInstalled, 1},
		"an installation the person cannot reach": {connectBody("code-acme", 200), nil, 422, ReasonInstallationNotReachable, 1},
		"a collaborator on one repository":        {connectBody("code-guest", 0), nil, 422, ReasonInstallationNotControlled, 1},
		"a collaborator naming the installation":  {connectBody("code-guest", 100), nil, 422, ReasonInstallationNotControlled, 1},
		"an installation that does not exist":     {connectBody("code-both", 999), nil, 422, ReasonInstallationNotReachable, 1},
		"a code github does not take":             {connectBody("code-made-up", 100), nil, 422, ReasonCodeRejected, 1},
		"no code":                                 {connectBody("", 100), nil, 422, ReasonInvalidRequest, 0},
		"no redirect":                             {map[string]any{"code": "code-acme", "redirect_uri": ""}, nil, 422, ReasonInvalidRequest, 0},
		"a negative installation":                 {connectBody("code-acme", -1), nil, 422, ReasonInvalidRequest, 0},
		"github rate limits":                      {connectBody("code-acme", 100), fmt.Errorf("%w (%s)", github.ErrRateLimited, leak), 503, ReasonUnavailable, 1},
		"the app key expired":                     {connectBody("code-acme", 100), fmt.Errorf("%w (%s)", github.ErrAppCredentials, leak), 502, ReasonUnavailable, 1},
		"something else":                          {connectBody("code-acme", 100), errors.New("dial tcp: " + leak), 500, ReasonInternal, 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := newGitHubWorld(t, true)
			w.app.userErr = tc.err
			rec := w.call("owner", "POST", "/orgs/"+w.orgA+"/github-installations", tc.body)
			reason, message := refusalOf(t, rec)
			if rec.Code != tc.status || reason != tc.reason || message == "" {
				t.Fatalf("got = %d %s, want = %d %s", rec.Code, rec.Body, tc.status, tc.reason)
			}
			if len(w.app.asked) != tc.asks || len(w.installations.connected[w.orgA]) != 0 {
				t.Fatalf("GitHub was asked %d times and %d installations are connected, want %d and none", len(w.app.asked), len(w.installations.connected[w.orgA]), tc.asks)
			}
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatalf("the answer repeats the failure's text: %s", rec.Body)
			}
		})
	}
}

const leak = "ghs_a_token_in_an_error"

func TestInstallationsAreListedPagedAndDisconnected(t *testing.T) {
	w := newGitHubWorld(t, true)
	path := "/orgs/" + w.orgA + "/github-installations"
	rec := w.call("member", "GET", path, nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" || rec.Header().Get("X-Total-Count") != "0" {
		t.Fatalf("an org that connected none: got = %d %s (total %q), want an empty array", rec.Code, rec.Body, rec.Header().Get("X-Total-Count"))
	}
	w.connect(w.orgA, acme)
	w.store.clock = w.store.clock.Add(time.Minute)
	w.connect(w.orgA, rival)
	w.connect(w.orgB, acme)

	var listed []domain.OrgGitHubInstallation
	w.ok("member", "GET", path, nil, http.StatusOK, &listed)
	if len(listed) != 2 || listed[0].InstallationID != 100 || listed[1].InstallationID != 200 || listed[0].OrgID != w.orgA {
		t.Fatalf("listed = %+v, want the org's two, oldest first", listed)
	}
	rec = w.call("member", "GET", path+"?limit=1&offset=1", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].InstallationID != 200 || rec.Header().Get("X-Total-Count") != "2" {
		t.Fatalf("the second page of one: got = %s (total %q)", rec.Body, rec.Header().Get("X-Total-Count"))
	}
	if rec := w.call("member", "GET", path+"?limit=201", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a limit over the maximum: got = %d %s, want 422", rec.Code, rec.Body)
	}

	w.ok("owner", "DELETE", path+"/100", nil, http.StatusNoContent, nil)
	w.refused("owner", "DELETE", path+"/100", nil, http.StatusNotFound, ReasonNotFound)
	w.refused("owner", "DELETE", path+"/999", nil, http.StatusNotFound, ReasonNotFound)
	if len(w.installations.connected[w.orgA]) != 1 || len(w.installations.connected[w.orgB]) != 1 {
		t.Fatalf("after disconnecting one: %d of org a and %d of org b, want one each", len(w.installations.connected[w.orgA]), len(w.installations.connected[w.orgB]))
	}
	if len(w.app.asked)+len(w.app.listed)+len(w.app.mints) != 0 {
		t.Fatal("listing or disconnecting asked GitHub")
	}
}

func TestRepositoriesAreReadFromGitHubOnEveryCallAcrossTheOrgsInstallations(t *testing.T) {
	w := newGitHubWorld(t, true)
	path := "/orgs/" + w.orgA + "/github-repositories"
	rec := w.call("member", "GET", path, nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" || len(w.app.listed) != 0 {
		t.Fatalf("an org that connected none: got = %d %s, want an empty array and GitHub left alone", rec.Code, rec.Body)
	}
	w.connect(w.orgA, acme)
	w.connect(w.orgA, rival)
	var found []Repository
	rec = w.call("member", "GET", path, nil)
	want := []Repository{{"Acme/Model", 100, true}, {"acme/other", 100, false}, {"rival/secret", 200, true}}
	if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil || !slices.Equal(found, want) || rec.Header().Get("X-Total-Count") != "3" {
		t.Fatalf("repositories = %s (total %q), want %+v sorted by name", rec.Body, rec.Header().Get("X-Total-Count"), want)
	}
	if !strings.Contains(rec.Body.String(), `"repository":"Acme/Model"`) || !strings.Contains(rec.Body.String(), `"installation_id":100`) || !strings.Contains(rec.Body.String(), `"private":true`) {
		t.Fatalf("a repository is not {repository, installation_id, private}: %s", rec.Body)
	}
	w.ok("member", "GET", path, nil, http.StatusOK, nil)
	if !slices.Equal(w.app.listed, []int64{100, 200, 100, 200}) {
		t.Fatalf("GitHub was asked for %v, want both installations on each of the two calls", w.app.listed)
	}

	for query, tc := range map[string]struct {
		want  []string
		total string
	}{
		"?q=MODEL":           {[]string{"Acme/Model"}, "1"},
		"?q=acme/":           {[]string{"Acme/Model", "acme/other"}, "2"},
		"?q=nothing":         {[]string{}, "0"},
		"?limit=1&offset=1":  {[]string{"acme/other"}, "3"},
		"?q=e&limit=1":       {[]string{"Acme/Model"}, "3"},
		"?offset=7":          {[]string{}, "3"},
		"?q=%20secret%20":    {[]string{"rival/secret"}, "1"},
		"?q=rival&offset=0":  {[]string{"rival/secret"}, "1"},
		"?limit=200&q=other": {[]string{"acme/other"}, "1"},
	} {
		rec := w.call("member", "GET", path+query, nil)
		var page []Repository
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		names := []string{}
		for _, repo := range page {
			names = append(names, repo.Repository)
		}
		if rec.Code != http.StatusOK || !slices.Equal(names, tc.want) || rec.Header().Get("X-Total-Count") != tc.total {
			t.Errorf("%s: got = %d %v (total %q), want %v of %s", query, rec.Code, names, rec.Header().Get("X-Total-Count"), tc.want, tc.total)
		}
	}
	if rec := w.call("member", "GET", path+"?sort=name", nil); rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("a query parameter the route does not have: got = %d %s, want it refused", rec.Code, rec.Body)
	}

	// An installation removed on GitHub covers nothing, and the others are still listed.
	delete(w.app.covers, 100)
	w.ok("member", "GET", path, nil, http.StatusOK, &found)
	if len(found) != 1 || found[0].Repository != "rival/secret" {
		t.Fatalf("with one installation gone: %+v, want the other's repositories", found)
	}
	// Any other failure is a failure of the read: a short list would read as the whole of it.
	w.app.listErr[200] = fmt.Errorf("%w (%s)", github.ErrRateLimited, leak)
	if message := w.refused("member", "GET", path, nil, http.StatusServiceUnavailable, ReasonUnavailable); strings.Contains(message, leak) {
		t.Fatalf("the answer repeats the failure's text: %s", message)
	}
	w.app.listErr[200] = errors.New("unexpected " + leak)
	w.refused("member", "GET", path, nil, http.StatusInternalServerError, ReasonInternal)
}

func TestADeploymentWithoutAnAppAnswersSoAfterTheAccessCheck(t *testing.T) {
	w := newGitHubWorld(t, false)
	w.connect(w.orgA, acme)
	org := "/orgs/" + w.orgA
	for _, tc := range []struct {
		name, caller, method, path string
		body                       any
		status                     int
		reason                     string
	}{
		{"the app", "owner", "GET", "/github/app", nil, 503, ReasonGitHubNotConfigured},
		{"connect", "owner", "POST", org + "/github-installations", connectBody("code-acme", 100), 503, ReasonGitHubNotConfigured},
		{"repositories", "member", "GET", org + "/github-repositories", nil, 503, ReasonGitHubNotConfigured},
		{"a credential", "agent", "POST", "/git-credentials", modelCredential, 503, ReasonGitHubNotConfigured},
		{"a credential of an experiment with no repository", "agent-bare", "POST", "/git-credentials", modelCredential, 404, ReasonNoRepository},
	} {
		message := w.refused(tc.caller, tc.method, tc.path, tc.body, tc.status, tc.reason)
		if tc.status == 503 && !strings.Contains(message, "no GitHub App configured") {
			t.Errorf("%s: message = %q, want it to say the deployment has no App", tc.name, message)
		}
	}
	for name, tc := range map[string]struct {
		caller, method, path string
		body                 any
		want                 int
	}{
		"connect to another org":      {"owner", "POST", "/orgs/" + w.orgB + "/github-installations", connectBody("code-acme", 100), 404},
		"connect as a member":         {"member", "POST", org + "/github-installations", connectBody("code-acme", 100), 403},
		"a credential for a person":   {"owner", "POST", "/git-credentials", modelCredential, 403},
		"listing what is connected":   {"member", "GET", org + "/github-installations", nil, 200},
		"disconnecting what is there": {"owner", "DELETE", org + "/github-installations/100", nil, 204},
	} {
		if rec := w.call(tc.caller, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, rec.Code, rec.Body, tc.want)
		}
	}
	if _, err := w.svc.Reachable(context.Background(), w.projectA, "acme/model"); statusOf(err) != 503 {
		t.Errorf("Reachable with no App: %v, want 503", err)
	}
}

func statusOf(err error) int {
	var refusal *refused
	if errors.As(err, &refusal) {
		return refusal.status
	}
	return 0
}

func reasonOf(err error) string {
	var refusal *refused
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

func TestAGitCredentialGoesToWhatThePlatformLaunchedForItsExperimentsRepository(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	for _, tc := range []struct {
		caller string
		access github.Access
	}{{"agent", github.Write}, {"coordinator", github.Read}, {"job", github.Read}} {
		before := len(w.app.mints)
		rec := w.call(tc.caller, "POST", "/git-credentials", modelCredential)
		if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: got = %d %s (Cache-Control %q), want 201 that is not stored", tc.caller, rec.Code, rec.Body, rec.Header().Get("Cache-Control"))
		}
		var credential GitCredential
		w.ok(tc.caller, "POST", "/git-credentials", modelCredential, http.StatusCreated, &credential)
		want := minted{100, "Acme/Model", tc.access}
		if len(w.app.mints) != before+2 || w.app.mints[before] != want || w.app.mints[before+1] != want {
			t.Fatalf("%s: minted %+v, want %+v on each of the two calls", tc.caller, w.app.mints[before:], want)
		}
		if credential.Protocol != "https" || credential.Host != "github.com" || credential.Path != "Acme/Model.git" ||
			credential.Username != "x-access-token" || credential.Password != fmt.Sprintf("ghs_minted_%d", before+2) || credential.ExpiresAt.IsZero() {
			t.Fatalf("%s: credential = %+v", tc.caller, credential)
		}
	}
}

// Nothing is minted until the address git asked about is known to be the experiment's repository.
func TestAGitCredentialIsMintedOnlyForTheAddressOfTheExperimentsRepository(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	ask := func(protocol, host, path string) map[string]any {
		return map[string]any{"protocol": protocol, "host": host, "path": path}
	}
	for _, path := range []string{"Acme/Model.git", "Acme/Model", "acme/model.git", "/Acme/Model.git/", "Acme/Model.git/info/lfs"} {
		var credential GitCredential
		w.ok("agent", "POST", "/git-credentials", ask("https", "github.com", path), http.StatusCreated, &credential)
		if credential.Path != strings.Trim(path, "/") || credential.Password == "" {
			t.Errorf("path %q: credential path = %q", path, credential.Path)
		}
	}
	w.ok("agent", "POST", "/git-credentials", ask("https", "GitHub.com", "Acme/Model.git"), http.StatusCreated, nil)
	served := len(w.app.mints)
	for name, body := range map[string]map[string]any{
		"another repository of the installation": ask("https", "github.com", "acme/other.git"),
		"a repository under it":                  ask("https", "github.com", "Acme/Model.git/extra"),
		"a repository that starts so":            ask("https", "github.com", "Acme/Model.github"),
		"the lfs path of another":                ask("https", "github.com", "acme/other.git/info/lfs"),
		"another host":                           ask("https", "evil.example", "Acme/Model.git"),
		"a host that starts so":                  ask("https", "github.com.evil.example", "Acme/Model.git"),
		"plain http":                             ask("http", "github.com", "Acme/Model.git"),
		"no protocol":                            ask("", "github.com", "Acme/Model.git"),
		"no path":                                ask("https", "github.com", ""),
	} {
		message := w.refused("agent", "POST", "/git-credentials", body, http.StatusNotFound, ReasonNotFound)
		if strings.Contains(message, "ghs_") {
			t.Errorf("%s: the refusal holds a token: %s", name, message)
		}
	}
	for name, body := range map[string]map[string]any{
		"no body fields": {}, "no path field": {"protocol": "https", "host": "github.com"},
		"a field it does not know": {"protocol": "https", "host": "github.com", "path": "Acme/Model.git", "username": "x"},
	} {
		if rec := w.call("agent", "POST", "/git-credentials", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got = %d %s, want 422", name, rec.Code, rec.Body)
		}
	}
	if rec := w.call("agent", "GET", "/git-credential?protocol=https&host=github.com&path=Acme%2FModel.git", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the route this one replaced: got = %d, want it gone", rec.Code)
	}
	if len(w.app.mints) != served {
		t.Fatalf("%d tokens were minted for addresses that are not the repository's", len(w.app.mints)-served)
	}
}

func TestNoGitCredentialForAnyoneElse(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	for _, caller := range []string{"owner", "member", "outsider", "org-key", "project-key", "staff", "staff-user", "runtime", "launcher"} {
		if rec := w.call(caller, "POST", "/git-credentials", modelCredential); rec.Code != http.StatusForbidden {
			t.Errorf("%s: got = %d %s, want 403", caller, rec.Code, rec.Body)
		}
	}
	if rec := w.call("", "POST", "/git-credentials", modelCredential); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got = %d, want 401", rec.Code)
	}
	// An agent of another org's experiment that names the same repository gets nothing through
	// this org's installation: its own org connected none.
	w.refused("agent-other-org", "POST", "/git-credentials", modelCredential, http.StatusConflict, ReasonInstallationNotConnected)
	w.refused("agent-bare", "POST", "/git-credentials", modelCredential, http.StatusNotFound, ReasonNoRepository)
	if len(w.app.mints) != 0 {
		t.Fatalf("a token was minted for a refused caller: %+v", w.app.mints)
	}
}

// The token of a run that is over still works for its wind-down. A credential would outlive it
// by an hour, so none is minted: for an agent, a coordinator and a job alike.
func TestNoGitCredentialOnceTheRunIsOver(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	for _, caller := range []string{"agent-run-over", "coordinator-run-over", "job-run-over"} {
		w.refused(caller, "POST", "/git-credentials", modelCredential, http.StatusForbidden, ReasonRunOver)
	}
	if len(w.app.mints) != 0 {
		t.Fatalf("a token was minted for a run that is over: %+v", w.app.mints)
	}
	for _, caller := range []string{"agent", "coordinator", "job"} {
		w.ok(caller, "POST", "/git-credentials", modelCredential, http.StatusCreated, nil)
	}
}

// What changes after the experiment was created shows on the next credential, as a state the
// caller cannot fix and with the reason that says who can.
func TestACredentialIsRefusedOnceTheRepositoryCanNoLongerBeReached(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*githubWorld)
		status  int
		reason  string
	}{
		"the org disconnected the installation":            {func(w *githubWorld) { w.installations.connected[w.orgA] = nil }, 409, ReasonInstallationNotConnected},
		"the org connected only another account's":         {func(w *githubWorld) { w.installations.connected[w.orgA] = nil; w.connect(w.orgA, rival) }, 409, ReasonInstallationNotConnected},
		"the repository was taken out of the installation": {func(w *githubWorld) { w.app.covers[100] = w.app.covers[100][:1] }, 409, ReasonRepositoryNotReachable},
		"the app was removed on github":                    {func(w *githubWorld) { delete(w.app.covers, 100) }, 409, ReasonInstallationGone},
		"the installation withholds the access":            {func(w *githubWorld) { w.app.tokenErr = github.ErrPermissionNotGranted }, 409, ReasonPermissionNotGranted},
		"github rate limits":                               {func(w *githubWorld) { w.app.tokenErr = github.ErrRateLimited }, 503, ReasonUnavailable},
		"the app key expired":                              {func(w *githubWorld) { w.app.tokenErr = fmt.Errorf("%w (%s)", github.ErrAppCredentials, leak) }, 502, ReasonUnavailable},
		"something else":                                   {func(w *githubWorld) { w.app.tokenErr = errors.New("unexpected " + leak) }, 500, ReasonInternal},
	} {
		t.Run(name, func(t *testing.T) {
			w := newGitHubWorld(t, true)
			w.connect(w.orgA, acme)
			w.ok("agent", "POST", "/git-credentials", modelCredential, http.StatusCreated, nil)
			tc.arrange(w)
			for _, caller := range []string{"agent", "coordinator", "job"} {
				message := w.refused(caller, "POST", "/git-credentials", modelCredential, tc.status, tc.reason)
				if strings.Contains(message, leak) || strings.Contains(message, "ghs_") {
					t.Fatalf("%s: the refusal holds a credential or the failure's text: %s", caller, message)
				}
			}
			if len(w.app.mints) != 1 {
				t.Fatalf("tokens minted = %d, want only the one from before", len(w.app.mints))
			}
		})
	}
	// Connected again, the same agent is served again with no other step.
	w := newGitHubWorld(t, true)
	w.refused("agent", "POST", "/git-credentials", modelCredential, http.StatusConflict, ReasonInstallationNotConnected)
	w.connect(w.orgA, acme)
	w.ok("agent", "POST", "/git-credentials", modelCredential, http.StatusCreated, nil)
}

func TestARepositoryIsReachableThroughTheInstallationsOfTheProjectsOrgAlone(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	w.connect(w.orgB, rival)
	ctx := context.Background()
	named, err := w.svc.Reachable(ctx, w.projectA2, "ACME/model")
	if err != nil || named != "Acme/Model" || len(w.app.mints) != 1 || w.app.mints[0] != (minted{100, "Acme/Model", github.Read}) {
		t.Fatalf("Reachable = %q, %v, minted %+v, want the name as GitHub spells it, checked with one read token", named, err, w.app.mints)
	}
	for name, tc := range map[string]struct {
		project, repository string
		status              int
		reason              string
	}{
		"a repository of an account the org did not connect": {w.projectA, "rival/secret", 422, ReasonInstallationNotConnected},
		"another org's installation, for this org's project": {w.projectB, "Acme/Model", 422, ReasonInstallationNotConnected},
		"a repository the installation does not cover":       {w.projectA, "acme/unlisted", 422, ReasonRepositoryNotReachable},
		"a name that is no repository":                       {w.projectA, "https://github.com/acme/model", 422, ReasonInvalidRepository},
		"a path out of the api":                              {w.projectA, "acme/../../app", 422, ReasonInvalidRepository},
		"a project that does not exist":                      {"prj-missing", "acme/model", 404, ReasonNotFound},
	} {
		named, err := w.svc.Reachable(ctx, tc.project, tc.repository)
		if named != "" || statusOf(err) != tc.status || reasonOf(err) != tc.reason {
			t.Errorf("%s: Reachable = %q, %v, want %d %s", name, named, err, tc.status, tc.reason)
		}
	}
	if len(w.app.mints) != 1 {
		t.Fatalf("a token was minted for a repository that is not reachable: %+v", w.app.mints)
	}
	delete(w.app.covers, 100)
	if _, err := w.svc.Reachable(ctx, w.projectA, "acme/model"); statusOf(err) != 422 || reasonOf(err) != ReasonInstallationGone {
		t.Errorf("with the App removed on GitHub: %v, want 422 installation_gone", err)
	}
}

func TestARepositoryIsCreatedOnTheAccountTheProjectsOrgConnected(t *testing.T) {
	ctx := context.Background()
	bare := newGitHubWorld(t, false)
	if named, err := bare.svc.Create(ctx, bare.projectA, "", "sweep-1a2b3c4d", ""); named != "" || err != nil {
		t.Fatalf("on a deployment with no GitHub App: Create = %q, %v, want none made and no refusal", named, err)
	}
	w := newGitHubWorld(t, true)
	if named, err := w.svc.Create(ctx, w.projectA, "", "sweep-1a2b3c4d", ""); named != "" || statusOf(err) != 422 || reasonOf(err) != ReasonGitHubNotConnected {
		t.Fatalf("for an org that connected no GitHub account: Create = %q, %v, want 422 github_not_connected", named, err)
	}
	w.connect(w.orgA, acme)
	w.connect(w.orgB, rival)
	named, err := w.svc.Create(ctx, w.projectA, "", "sweep-1a2b3c4d", "")
	if err != nil || named != "Acme/sweep-1a2b3c4d" || len(w.app.created) != 1 || w.app.created[0] != "100 sweep-1a2b3c4d" {
		t.Fatalf("Create = %q, %v (created %v), want it made through the org's own installation", named, err, w.app.created)
	}
	// With two accounts connected the request says which, and nothing is made on a guess.
	w.connect(w.orgA, rival)
	if named, err := w.svc.Create(ctx, w.projectA, "", "ambiguous", ""); named != "" || statusOf(err) != 422 || reasonOf(err) != ReasonRepositoryAccountRequired || !strings.Contains(err.Error(), "Acme, rival") {
		t.Fatalf("with two accounts and none named: Create = %q, %v, want 422 repository_account_required listing them", named, err)
	}
	if named, err := w.svc.Create(ctx, w.projectA, "RIVAL", "on-rival", ""); err != nil || len(w.app.created) != 2 || w.app.created[1] != "200 on-rival" {
		t.Fatalf("naming the account: Create = %q, %v (created %v), want it made through that account's installation", named, err, w.app.created)
	}
	if named, err := w.svc.Create(ctx, w.projectA, "elsewhere", "nowhere", ""); named != "" || statusOf(err) != 422 || reasonOf(err) != ReasonInstallationNotConnected || len(w.app.created) != 2 {
		t.Fatalf("naming an account the org did not connect: Create = %q, %v, want 422 installation_not_connected", named, err)
	}
	for name, tc := range map[string]struct {
		project   string
		createErr error
		status    int
		reason    string
	}{
		"the installation may not create": {w.projectA, github.ErrCannotCreateRepositories, 422, ReasonRepositoryNotCreated},
		"the name is taken":               {w.projectA, github.ErrRepositoryExists, 409, ReasonRepositoryNotCreated},
		"the App was removed on GitHub":   {w.projectA, github.ErrInstallationGone, 422, ReasonInstallationGone},
		"a project that does not exist":   {"prj-missing", nil, 404, ReasonNotFound},
	} {
		w.app.createErr = tc.createErr
		if named, err := w.svc.Create(ctx, tc.project, "acme", "another", ""); named != "" || statusOf(err) != tc.status || reasonOf(err) != tc.reason {
			t.Errorf("%s: Create = %q, %v, want %d %s", name, named, err, tc.status, tc.reason)
		}
	}
}

func TestTheExperimentFileIsReadFromTheRepositoryNamed(t *testing.T) {
	w := newGitHubWorld(t, true)
	w.connect(w.orgA, acme)
	w.app.file = "name: from the repository\n"
	ctx := context.Background()
	data, err := w.svc.ExperimentBase(ctx, w.projectA, "acme/model")
	want := "ghs_minted_1@Acme/Model::" + domain.ExperimentFile
	if err != nil || string(data) != w.app.file || len(w.app.files) != 1 || w.app.files[0] != want {
		t.Fatalf("base = %q, %v, read as %v, want the file at the head of the default branch with a token for that repository", data, err, w.app.files)
	}
	if w.app.mints[0].access != github.Read {
		t.Fatalf("the file was read with %s access, want read", w.app.mints[0].access)
	}
	for name, tc := range map[string]struct {
		err    error
		status int
		reason string
	}{
		"no such file":        {github.ErrFileNotFound, 422, ReasonNoExperimentFile},
		"a directory":         {github.ErrNotAFile, 422, ReasonNoExperimentFile},
		"too large":           {github.ErrFileTooLarge, 422, ReasonNoExperimentFile},
		"github rate limits":  {github.ErrRateLimited, 503, ReasonUnavailable},
		"something unplanned": {errors.New("unexpected " + leak), 500, ReasonInternal},
	} {
		w.app.fileErr = tc.err
		_, err := w.svc.ExperimentBase(ctx, w.projectA, "acme/model")
		if statusOf(err) != tc.status || reasonOf(err) != tc.reason || strings.Contains(err.Error(), leak) {
			t.Errorf("%s: %v, want %d %s without the failure's text", name, err, tc.status, tc.reason)
		}
	}
	w.app.fileErr = nil
	read := len(w.app.files)
	if _, err := w.svc.ExperimentBase(ctx, w.projectA, "rival/secret"); reasonOf(err) != ReasonInstallationNotConnected || len(w.app.files) != read {
		t.Errorf("a repository the org does not reach: %v, read %d files, want installation_not_connected and none read", err, len(w.app.files)-read)
	}
}
