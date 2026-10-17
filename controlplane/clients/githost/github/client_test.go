package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

const testAppID = 4242

// testChanged is when GitHub last changed every installation of the fake.
var testChanged = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fakeRepo struct {
	id            int64
	fullName      string
	defaultBranch string
	files         map[string]string
	directories   map[string]bool
	// copying is how many more reads of its default branch answer that it does not exist yet, as
	// GitHub answers while it copies a template's files.
	copying int
}

type fakeInstallation struct {
	account string
	// owner is the user whose own account the installation is on, empty for an organisation's.
	owner string
	// creates says the installation granted the App the creation of repositories, and withheld
	// names a permission of the App's it has not granted.
	creates   bool
	withheld  string
	suspended bool
	// all says the installation covers every repository of its account, those made later too.
	all   bool
	repos map[int64]bool
}

type mintedToken struct {
	installation int64
	repos        []int64
	permissions  map[string]string
}

// fakeGitHub answers the calls the client makes the way GitHub does: it checks the App JWT
// against the App's public key and limits an installation token to the repositories it was
// minted for.
type fakeGitHub struct {
	t             *testing.T
	server        *httptest.Server
	key           *rsa.PublicKey
	now           func() time.Time
	installations map[int64]*fakeInstallation
	repos         map[string]*fakeRepo
	tokens        map[string]mintedToken
	sequence      int
	// rateLimit, when set, answers every API call with that status as a rate limit.
	rateLimit int
	// widen makes a minted token claim a second repository.
	widen bool
	// ungranted makes the installation refuse the permissions asked for.
	ungranted bool
	// kinds holds what a path is when it is not a file: dir, symlink or submodule.
	kinds map[string]string
	calls []string
	// generated is the body of the last generation from the template, and copying how many reads
	// of a generated repository's branch answer before its files are there.
	generated map[string]any
	copying   int
	// createRefusal, when set, is how the creation of a repository is answered, and uncovered
	// leaves a repository created out of its installation.
	createRefusal *createRefusal
	uncovered     bool
	// codes is the person's token each authorization code is exchanged for, once, and reaches the
	// installations each such token lists.
	codes   map[string]string
	reaches map[string][]int64
}

const (
	testClientID     = "Iv23testclient"
	testClientSecret = "a-client-secret"
)

const testTemplate = "scaleresearch/hypothesisloop-experiment-template"

var templateFiles = map[string]string{"README.md": "what this is\n", ".hypothesisloop/job.yaml": "job:\n  image: one\n"}

type createRefusal struct {
	status int
	header http.Header
	body   map[string]any
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *Client) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{
		t: t, key: &key.PublicKey, now: time.Now,
		installations: map[int64]*fakeInstallation{
			100: {account: "Acme", creates: true, repos: map[int64]bool{1: true, 2: true}},
			200: {account: "rival", repos: map[int64]bool{3: true}},
			300: {account: "carol", owner: "carol", creates: true, repos: map[int64]bool{}},
		},
		repos: map[string]*fakeRepo{
			"acme/model":    {id: 1, fullName: "Acme/Model", defaultBranch: "main", files: map[string]string{".hypothesisloop/experiment.yaml": "name: from the repo\n"}, directories: map[string]bool{".hypothesisloop": true}},
			"acme/other":    {id: 2, fullName: "acme/other", defaultBranch: "trunk"},
			"rival/secret":  {id: 3, fullName: "rival/secret", defaultBranch: "main"},
			"acme/unlisted": {id: 4, fullName: "acme/unlisted", defaultBranch: "main"},
		},
		tokens:  map[string]mintedToken{},
		kinds:   map[string]string{"link": "symlink", "vendored": "submodule"},
		codes:   map[string]string{"code-alice": "gho_alice", "code-mallory": "gho_mallory"},
		reaches: map[string][]int64{"gho_alice": {100, 300}, "gho_mallory": {200}},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	client, err := New(config.GitHubConfig{AppID: testAppID, AppSlug: "hypothesisloop", ClientID: testClientID},
		string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL, client.webURL = f.server.URL, f.server.URL
	client.mintRetries = []time.Duration{time.Millisecond, time.Millisecond}
	return f, client
}

func (f *fakeGitHub) refuse(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": message}) //nolint:errcheck
}

func (f *fakeGitHub) answer(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body) //nolint:errcheck
}

// appAuthenticated checks an App JWT as GitHub does: signature, issuer, and a lifetime of at
// most ten minutes that has begun and not ended.
// userID is the id of a user's own account, and an organisation's is its installation's id, so
// that an organisation and a user can hold one number as GitHub lets them.
func userID(user string) int64 {
	return map[string]int64{"alice": 100, "reader": 200, "mallory": 300, "carol": 400}[user]
}

func (f *fakeGitHub) accountOf(installationID int64) map[string]any {
	installation := f.installations[installationID]
	if installation.owner != "" {
		return map[string]any{"id": userID(installation.owner), "login": installation.account, "type": "User"}
	}
	return map[string]any{"id": installationID, "login": installation.account, "type": "Organization"}
}

func (f *fakeGitHub) appAuthenticated(bearer string) bool {
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.key, crypto.SHA256, digest[:], signature) != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	now := f.now().Unix()
	return claims.Iss == strconv.Itoa(testAppID) && claims.Iat <= now && claims.Exp > now && claims.Exp-claims.Iat <= 600
}

func (f *fakeGitHub) repoByID(id int64) *fakeRepo {
	for _, repo := range f.repos {
		if repo.id == id {
			return repo
		}
	}
	return nil
}

// visible reports whether bearer may read the repository, and whether it may push.
func (f *fakeGitHub) visible(bearer string, repo *fakeRepo) (read, push bool) {
	if token, ok := f.tokens[bearer]; ok {
		for _, id := range token.repos {
			if id == repo.id {
				return true, token.permissions["contents"] == "write"
			}
		}
	}
	return false, false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if f.rateLimit != 0 {
		if f.rateLimit == http.StatusForbidden {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
		f.refuse(w, f.rateLimit, "API rate limit exceeded for installation")
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/login/oauth/access_token" && r.Method == http.MethodPost:
		f.exchange(w, r)
	case r.URL.Path == "/user/installations" && r.Method == http.MethodGet:
		reaches, ok := f.reaches[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		all := []map[string]any{}
		for _, id := range reaches {
			all = append(all, map[string]any{"id": id, "account": f.accountOf(id)})
		}
		f.answer(w, http.StatusOK, map[string]any{"total_count": len(all), "installations": paged(r, all)})
	case len(segments) == 4 && segments[0] == "app" && segments[1] == "installations" && segments[3] == "access_tokens" && r.Method == http.MethodPost:
		f.mint(w, r, bearer, segments[2])
	case len(segments) == 3 && segments[0] == "app" && segments[1] == "installations" && r.Method == http.MethodGet:
		if !f.appAuthenticated(bearer) {
			f.refuse(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		id, _ := strconv.ParseInt(segments[2], 10, 64)
		installation, ok := f.installations[id]
		if !ok {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		body := map[string]any{"id": id, "suspended_at": nil, "updated_at": testChanged.Format(time.RFC3339), "account": f.accountOf(id), "repository_selection": "selected",
			"permissions": maps.Clone(Permissions)}
		delete(body["permissions"].(map[string]string), installation.withheld)
		if f.ungranted {
			body["permissions"] = map[string]string{"metadata": "read", "contents": "read"}
		}
		if installation.all {
			body["repository_selection"] = "all"
		}
		if installation.suspended {
			body["suspended_at"] = "2026-01-01T00:00:00Z"
		}
		f.answer(w, http.StatusOK, body)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/"+testTemplate+"/generate":
		var body struct {
			Owner       string `json:"owner"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		token, ok := f.tokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		f.generated = map[string]any{"owner": body.Owner, "name": body.Name, "description": body.Description, "private": body.Private}
		installation := f.installations[token.installation]
		switch {
		case !strings.EqualFold(installation.account, body.Owner):
			f.refuse(w, http.StatusNotFound, "Not Found")
		case f.createRefusal != nil:
			maps.Copy(w.Header(), f.createRefusal.header)
			f.answer(w, f.createRefusal.status, f.createRefusal.body)
		case !installation.creates || len(token.permissions) != 0:
			w.Header().Set("X-Accepted-GitHub-Permissions", "administration=write; repository_creation=write")
			f.refuse(w, http.StatusForbidden, "Resource not accessible by integration")
		case f.repos[strings.ToLower(installation.account+"/"+body.Name)] != nil:
			f.answer(w, http.StatusUnprocessableEntity, map[string]any{
				"message": "Repository creation failed.",
				"errors":  []map[string]string{{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}},
			})
		case !body.Private:
			f.t.Errorf("a repository was asked for public")
		default:
			f.sequence++
			repo := &fakeRepo{id: int64(1000 + f.sequence), fullName: installation.account + "/" + body.Name, defaultBranch: "main", files: maps.Clone(templateFiles), copying: f.copying}
			f.repos[strings.ToLower(repo.fullName)] = repo
			if !f.uncovered {
				installation.repos[repo.id] = true
			}
			f.answer(w, http.StatusCreated, map[string]any{"id": repo.id, "full_name": repo.fullName, "private": true, "default_branch": repo.defaultBranch})
		}
	case len(segments) >= 5 && segments[0] == "repos" && segments[3] == "branches" && r.Method == http.MethodGet:
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if read, _ := f.visible(bearer, repo); !read {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if strings.Join(segments[4:], "/") != repo.defaultBranch {
			f.refuse(w, http.StatusNotFound, "Branch not found")
			return
		}
		if repo.copying > 0 {
			repo.copying--
			f.refuse(w, http.StatusNotFound, "Branch not found")
			return
		}
		f.answer(w, http.StatusOK, map[string]any{"name": repo.defaultBranch})
	case r.URL.Path == "/installation/repositories":
		token, ok := f.tokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		sort.Slice(token.repos, func(i, j int) bool { return token.repos[i] < token.repos[j] })
		all := []map[string]any{}
		for _, id := range token.repos {
			all = append(all, map[string]any{"id": id, "full_name": f.repoByID(id).fullName, "private": id != 2})
		}
		f.answer(w, http.StatusOK, map[string]any{"total_count": len(all), "repositories": paged(r, all)})
	case len(segments) == 3 && segments[0] == "repos":
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		if _, isInstallation := f.tokens[bearer]; !isInstallation {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		read, push := f.visible(bearer, repo)
		if !read {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		f.answer(w, http.StatusOK, map[string]any{
			"id": repo.id, "full_name": repo.fullName, "default_branch": repo.defaultBranch, "private": true,
			"permissions": map[string]bool{"pull": true, "push": push},
		})
	case len(segments) >= 5 && segments[0] == "repos" && segments[3] == "contents":
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if read, _ := f.visible(bearer, repo); !read {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		path := strings.Join(segments[4:], "/")
		if repo.directories[path] {
			f.answer(w, http.StatusOK, []map[string]string{{"type": "file", "name": "experiment.yaml"}})
			return
		}
		if kind, ok := f.kinds[path]; ok {
			f.answer(w, http.StatusOK, map[string]any{"type": kind, "size": 12, "target": "elsewhere"})
			return
		}
		content, ok := repo.files[path]
		if ref := r.URL.Query().Get("ref"); !ok || (ref != "" && ref != repo.defaultBranch) {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		f.answer(w, http.StatusOK, map[string]any{
			"type": "file", "size": len(content), "encoding": "base64",
			"content": wrapped(base64.StdEncoding.EncodeToString([]byte(content))),
		})
	default:
		f.refuse(w, http.StatusNotFound, "Not Found")
	}
}

// exchange answers as GitHub's OAuth endpoint does: 200 for a code it refuses too, with an error.
func (f *fakeGitHub) exchange(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") != "application/json" || r.ParseForm() != nil {
		f.refuse(w, http.StatusBadRequest, "not a form asking for JSON")
		return
	}
	if r.PostForm.Get("client_id") != testClientID || r.PostForm.Get("client_secret") != testClientSecret {
		f.answer(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	token, ok := f.codes[r.PostForm.Get("code")]
	if !ok {
		f.answer(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		return
	}
	delete(f.codes, r.PostForm.Get("code"))
	f.answer(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "bearer"})
}

func (f *fakeGitHub) mint(w http.ResponseWriter, r *http.Request, bearer, installationID string) {
	if !f.appAuthenticated(bearer) {
		f.refuse(w, http.StatusUnauthorized, "'Expiration time' claim ('exp') must be a numeric value representing the future time at which the assertion expires")
		return
	}
	id, _ := strconv.ParseInt(installationID, 10, 64)
	installation, ok := f.installations[id]
	if !ok {
		f.refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	if installation.suspended {
		f.refuse(w, http.StatusForbidden, "This installation has been suspended")
		return
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.refuse(w, http.StatusBadRequest, "bad body")
		return
	}
	// Only a token that reads names and nothing else, or the one a repository is created with,
	// which is asked for whole, may reach a whole installation.
	if len(body.Repositories) == 0 && len(body.Permissions) != 0 && !maps.Equal(body.Permissions, map[string]string{"metadata": "read"}) {
		f.t.Errorf("a token for a whole installation was asked with %v", body.Permissions)
	}
	if f.ungranted {
		f.refuse(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
		return
	}
	repositories := []map[string]any{}
	var ids []int64
	if len(body.Repositories) == 0 {
		for id := range installation.repos {
			ids = append(ids, id)
		}
	}
	// As GitHub does, a repository is named without its owner, who is the installation's account.
	for _, name := range body.Repositories {
		repo := f.repos[strings.ToLower(installation.account+"/"+name)]
		if repo == nil || !installation.repos[repo.id] {
			f.refuse(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
		ids = append(ids, repo.id)
		repositories = append(repositories, map[string]any{"id": repo.id, "full_name": repo.fullName})
	}
	if f.widen {
		repositories = append(repositories, map[string]any{"id": 999})
	}
	f.sequence++
	token := fmt.Sprintf("ghs_installation_%d", f.sequence)
	f.tokens[token] = mintedToken{installation: id, repos: ids, permissions: body.Permissions}
	f.answer(w, http.StatusCreated, map[string]any{
		"token": token, "expires_at": f.now().Add(time.Hour).UTC().Format(time.RFC3339), "repositories": repositories,
	})
}

// paged is one page of all, as GitHub cuts a listing by per_page and page.
func paged(r *http.Request, all []map[string]any) []map[string]any {
	size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if size <= 0 || page <= 0 {
		return all
	}
	start := min((page-1)*size, len(all))
	return all[start:min(start+size, len(all))]
}

func (f *fakeGitHub) lastToken() mintedToken {
	return f.tokens[fmt.Sprintf("ghs_installation_%d", f.sequence)]
}

var ctx = context.Background()

func TestInstallationIsReadAsTheApp(t *testing.T) {
	f, client := newFakeGitHub(t)
	found, err := client.Installation(ctx, 200)
	if err != nil || *found != (Installation{ID: 200, AccountLogin: "rival", AccountType: AccountOrganization, SelectedRepositories: true}) {
		t.Fatalf("Installation = %+v, %v", found, err)
	}
	f.installations[300].all = true
	if found, err := client.Installation(ctx, 300); err != nil || found.AccountType != AccountUser || found.SelectedRepositories {
		t.Fatalf("Installation on a person's own account = %+v, %v, want it typed User and covering every repository", found, err)
	}
	if _, err := client.Installation(ctx, 999); !errors.Is(err, ErrInstallationGone) {
		t.Fatalf("an installation that is not the App's: got = %v, want %v", err, ErrInstallationGone)
	}
	f.installations[200].suspended = true
	if _, err := client.Installation(ctx, 200); !errors.Is(err, ErrInstallationSuspended) || !errors.Is(err, ErrInstallationGone) {
		t.Fatalf("a suspended installation: got = %v, want %v, which is %v", err, ErrInstallationSuspended, ErrInstallationGone)
	}
	client.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	if _, err := client.Installation(ctx, 100); !errors.Is(err, ErrAppCredentials) {
		t.Fatalf("with a key GitHub does not know: got = %v, want %v", err, ErrAppCredentials)
	}
}

func TestARepositoryIsCreatedPrivateOnTheInstallationsOrganisationAndCoveredByIt(t *testing.T) {
	f, client := newFakeGitHub(t)
	// Installation 100 does not cover every repository of its organisation, and covers the one
	// it creates.
	const want = "Acme/lr-sweep-1a2b3c4d"
	created, err := client.CreateFromTemplate(ctx, 100, testTemplate, "lr-sweep-1a2b3c4d", "what it is for")
	if err != nil || *created != (Repository{FullName: want, Private: true, DefaultBranch: "main"}) {
		t.Fatalf("CreateFromTemplate = %+v, %v, want %s", created, err, want)
	}
	if !maps.Equal(f.generated, map[string]any{"owner": "Acme", "name": "lr-sweep-1a2b3c4d", "description": "what it is for", "private": true}) {
		t.Fatalf("the template was generated from with %v, want a private repository on Acme", f.generated)
	}
	if token, err := client.Token(ctx, 100, want, Write); err != nil || token.Repository != want {
		t.Fatalf("a token for the new repository: got = %+v, %v", token, err)
	}
	for name, tc := range map[string]struct {
		installation int64
		repo         string
		arrange      func()
		want         error
	}{
		"a name the organisation has":            {100, "lr-sweep-1a2b3c4d", nil, ErrRepositoryExists},
		"a name with an owner in it":             {100, "a/b", nil, ErrInvalidRepository},
		"an installation that is gone":           {999, "fresh", nil, ErrInstallationGone},
		"an installation that may not create":    {200, "fresh", nil, ErrCannotCreateRepositories},
		"an installation that granted no access": {100, "fresh", func() { f.ungranted = true }, ErrPermissionNotGranted},
	} {
		if tc.arrange != nil {
			tc.arrange()
		}
		if _, err := client.CreateFromTemplate(ctx, tc.installation, testTemplate, tc.repo, ""); !errors.Is(err, tc.want) {
			t.Errorf("%s: got = %v, want %v", name, err, tc.want)
		}
		f.ungranted = false
	}
	if f.repos["acme/fresh"] != nil || f.repos["rival/fresh"] != nil {
		t.Fatal("a refused create left a repository behind")
	}
	f.generated = nil
	if _, err := client.CreateFromTemplate(ctx, 100, "no-owner", "fresh", ""); err == nil || f.generated != nil {
		t.Fatalf("a template that is not owner/name: got = %v, want it refused before GitHub is asked", err)
	}
}

// On a person's own account the installation generates the repository on that account with its
// own token, and its branch is awaited with a read token for the one repository.
func TestARepositoryIsCreatedOnAPersonsOwnAccountWithTheInstallationsToken(t *testing.T) {
	f, client := newFakeGitHub(t)
	created, err := client.CreateFromTemplate(ctx, 300, testTemplate, "sweep", "what it is for")
	if err != nil || *created != (Repository{FullName: "carol/sweep", Private: true, DefaultBranch: "main"}) {
		t.Fatalf("CreateFromTemplate = %+v, %v, want carol/sweep", created, err)
	}
	repo := f.repos["carol/sweep"]
	if !f.installations[300].repos[repo.id] || f.generated["owner"] != "carol" {
		t.Fatalf("the installation covers it: %v, generated on %v, want it covered on carol", f.installations[300].repos[repo.id], f.generated["owner"])
	}
	if read := f.lastToken(); read.permissions["contents"] != "read" || len(read.repos) != 1 || read.repos[0] != repo.id {
		t.Fatalf("the branch was read with %+v, want a read token for the one repository", read)
	}
}

func TestACreateOnAPersonsOwnAccountRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*fakeGitHub)
		check   func(error) bool
		left    bool
	}{
		"an installation that may not create": {func(f *fakeGitHub) { f.installations[300].creates = false }, func(err error) bool {
			var notPermitted *CreationNotPermittedError
			return errors.As(err, &notPermitted) && notPermitted.Accepted == "administration=write; repository_creation=write"
		}, false},
		"a name the account has": {func(f *fakeGitHub) { f.repos["carol/sweep"] = &fakeRepo{id: 77, fullName: "carol/sweep"} }, func(err error) bool { return errors.Is(err, ErrRepositoryExists) }, true},
		"a repository the installation does not cover": {func(f *fakeGitHub) { f.uncovered = true }, func(err error) bool {
			var notCovered *NotCoveredError
			return errors.As(err, &notCovered) && notCovered.Repository == "carol/sweep"
		}, true},
	} {
		f, client := newFakeGitHub(t)
		tc.arrange(f)
		created, err := client.CreateFromTemplate(ctx, 300, testTemplate, "sweep", "")
		if created != nil || !tc.check(err) || (f.repos["carol/sweep"] != nil) != tc.left {
			t.Errorf("%s: CreateFromTemplate = %+v, %v, repository left behind %v", name, created, err, f.repos["carol/sweep"] != nil)
		}
		if strings.Contains(fmt.Sprint(err), "ghs_") {
			t.Errorf("%s: the error holds a token: %v", name, err)
		}
	}
}

// Each refusal of a create reads as what it is: GitHub answers 403 for a missing permission and
// for a rate limit alike, and 422 for a taken name and for every other invalid request.
func TestARefusedCreateIsToldApartByWhatGitHubAnswered(t *testing.T) {
	accepted := http.Header{"X-Accepted-Github-Permissions": {"administration=write; repository_creation=write"}}
	nameErrors := func(message string) map[string]any {
		return map[string]any{"message": "Repository creation failed.", "errors": []map[string]string{{"resource": "Repository", "code": "custom", "field": "name", "message": message}}}
	}
	for name, tc := range map[string]struct {
		refusal createRefusal
		check   func(error) bool
	}{
		"the name is taken": {
			createRefusal{status: 422, body: nameErrors("name already exists on this account")},
			func(err error) bool { return errors.Is(err, ErrRepositoryExists) },
		},
		"a 422 about something else": {
			createRefusal{status: 422, body: map[string]any{"message": "Repository creation failed.", "errors": []map[string]string{{"resource": "Repository", "code": "custom", "field": "visibility", "message": "private repositories are disabled"}}}},
			func(err error) bool {
				var failed *CreationFailedError
				return errors.As(err, &failed) && failed.Status == 422 && failed.Message == "Repository creation failed." && !errors.Is(err, ErrRepositoryExists)
			},
		},
		"a 422 about the name that is not a taken one": {
			createRefusal{status: 422, body: nameErrors("name is too long (maximum is 100 characters)")},
			func(err error) bool {
				var failed *CreationFailedError
				return errors.As(err, &failed) && !errors.Is(err, ErrRepositoryExists)
			},
		},
		"a permission the installation lacks": {
			createRefusal{status: 403, header: accepted, body: map[string]any{"message": "Resource not accessible by integration"}},
			func(err error) bool {
				var notPermitted *CreationNotPermittedError
				return errors.As(err, &notPermitted) && notPermitted.Accepted == "administration=write; repository_creation=write" && errors.Is(err, ErrCannotCreateRepositories)
			},
		},
		"an organisation GitHub hides from the installation": {
			createRefusal{status: 404, body: map[string]any{"message": "Not Found"}},
			func(err error) bool { return errors.Is(err, ErrCannotCreateRepositories) },
		},
		"a rate limit with no calls remaining": {
			createRefusal{status: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}}, body: map[string]any{"message": "API rate limit exceeded for installation"}},
			func(err error) bool { return errors.Is(err, ErrRateLimited) },
		},
		"a rate limit only the message tells of": {
			createRefusal{status: 403, header: accepted, body: map[string]any{"message": "Rate Limit Exceeded"}},
			func(err error) bool {
				return errors.Is(err, ErrRateLimited) && !errors.Is(err, ErrCannotCreateRepositories)
			},
		},
		"a failure of GitHub's": {
			createRefusal{status: 500, body: map[string]any{"message": "Server Error"}},
			func(err error) bool {
				var failed *CreationFailedError
				return errors.As(err, &failed) && failed.Status == 500 && failed.Message == "Server Error"
			},
		},
	} {
		f, client := newFakeGitHub(t)
		f.createRefusal = &tc.refusal
		if created, err := client.CreateFromTemplate(ctx, 100, testTemplate, "fresh", ""); created != nil || !tc.check(err) {
			t.Errorf("%s: CreateFromTemplate = %+v, %v", name, created, err)
		}
	}
}

// GitHub copies the template's files after it answers the create: the create returns once the
// default branch exists, and the repository then holds the template's files.
func TestACreatedRepositoryHoldsTheTemplatesFilesOnceItsBranchExists(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.copying = 1
	created, err := client.CreateFromTemplate(ctx, 100, testTemplate, "sweep", "")
	if err != nil {
		t.Fatal(err)
	}
	if branchReads := len(slices.DeleteFunc(slices.Clone(f.calls), func(call string) bool { return call != "GET /repos/Acme/sweep/branches/main" })); branchReads != 2 {
		t.Fatalf("the branch was read %d times, want once before the copy and once after", branchReads)
	}
	token, err := client.Token(ctx, 100, created.FullName, Read)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range templateFiles {
		if data, err := client.File(ctx, token, "", path); err != nil || string(data) != want {
			t.Errorf("File(%s) = %q, %v, want %q", path, data, err, want)
		}
	}
}

func TestHasBranch(t *testing.T) {
	_, client := newFakeGitHub(t)
	token, err := client.Token(ctx, 100, "acme/other", Read)
	if err != nil {
		t.Fatal(err)
	}
	for branch, want := range map[string]bool{"trunk": true, "main": false, "hl/a-branch": false} {
		if found, err := client.HasBranch(ctx, token, branch); err != nil || found != want {
			t.Errorf("HasBranch(%s) = %v, %v, want %v", branch, found, err, want)
		}
	}
}

func TestRepositoriesListsWhatAnInstallationCoversToTheLastPage(t *testing.T) {
	f, client := newFakeGitHub(t)
	found, err := client.Repositories(ctx, 100)
	want := []Repository{{FullName: "Acme/Model", Private: true}, {FullName: "acme/other"}}
	if err != nil || !slices.Equal(found, want) {
		t.Fatalf("Repositories = %+v, %v, want %+v", found, err, want)
	}
	if minted := f.lastToken(); !maps.Equal(minted.permissions, map[string]string{"metadata": "read"}) {
		t.Fatalf("the listing was read with %+v, want a token that reads names alone", minted.permissions)
	}
	for id := int64(1000); id < 1230; id++ {
		name := fmt.Sprintf("acme/r%d", id)
		f.repos[name] = &fakeRepo{id: id, fullName: name}
		f.installations[100].repos[id] = true
	}
	if found, err := client.Repositories(ctx, 100); err != nil || len(found) != 232 {
		t.Fatalf("Repositories = %d of them, %v, want all 232 over three pages", len(found), err)
	}
	if found, err := client.Repositories(ctx, 999); !errors.Is(err, ErrInstallationGone) || found != nil {
		t.Fatalf("an installation that is gone: %+v, %v, want %v", found, err, ErrInstallationGone)
	}
}

func TestTokenIsLimitedToOneRepositoryAndTheAccessAsked(t *testing.T) {
	f, client := newFakeGitHub(t)
	for _, access := range []Access{Read, Write} {
		token, err := client.Token(ctx, 100, "acme/other", access)
		if err != nil {
			t.Fatalf("Token(%s): %v", access, err)
		}
		minted := f.tokens[token.Value]
		if len(minted.repos) != 1 || minted.repos[0] != 2 || minted.permissions["contents"] != string(access) {
			t.Fatalf("minted for %s = %+v, want repository 2 alone with contents %s", access, minted, access)
		}
		if until := time.Until(token.ExpiresAt); until < 50*time.Minute || until > 70*time.Minute {
			t.Fatalf("expires in %s, want GitHub's hour", until)
		}
	}
	if _, err := client.Token(ctx, 100, "acme/other", "admin"); err == nil {
		t.Fatal("Token minted an access that is neither read nor write")
	}
}

// Two platform experiments share installation 100. The token of one must not read the other's repository.
func TestATokenOfOneRepositoryDoesNotReachItsSiblingOnTheSameInstallation(t *testing.T) {
	f, client := newFakeGitHub(t)
	token, err := client.Token(ctx, 100, "acme/model", Write)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := client.call(ctx, http.MethodGet, "/repos/acme/other", token.Value, nil, &out); !errors.Is(err, errNotFound) {
		t.Fatalf("repository 1's token reading repository 2: got = %v, want not found", err)
	}
	if err := client.call(ctx, http.MethodGet, "/repos/acme/model", token.Value, nil, &out); err != nil {
		t.Fatalf("repository 1's token reading repository 1: %v", err)
	}
	_ = f
}

func TestTokenRefusals(t *testing.T) {
	cases := map[string]struct {
		arrange      func(*fakeGitHub, *Client)
		installation int64
		repo         string
		want         error
	}{
		"the installation was removed":                     {func(f *fakeGitHub, _ *Client) { delete(f.installations, 100) }, 100, "acme/model", ErrInstallationGone},
		"the installation was suspended":                   {func(f *fakeGitHub, _ *Client) { f.installations[100].suspended = true }, 100, "acme/model", ErrInstallationGone},
		"the repository was taken out of the installation": {func(f *fakeGitHub, _ *Client) { delete(f.installations[100].repos, 1) }, 100, "acme/model", ErrRepositoryNotInstalled},
		"a repository of another installation":             {func(*fakeGitHub, *Client) {}, 100, "rival/secret", ErrRepositoryNotInstalled},
		// An installation names its repositories without their owner. Another account's
		// installation holding a repository of the same name must not answer for this one.
		"a namesake under another account's installation": {func(f *fakeGitHub, _ *Client) {
			f.repos["rival/model"] = &fakeRepo{id: 7, fullName: "rival/model"}
			f.installations[200].repos[7] = true
		}, 200, "acme/model", ErrRepositoryNotInstalled},
		"a name that is no repository": {func(*fakeGitHub, *Client) {}, 100, "acme/model/../other", ErrInvalidRepository},
		"the App's key was rotated out": {func(_ *fakeGitHub, c *Client) {
			c.key, _ = rsa.GenerateKey(rand.Reader, 2048)
		}, 100, "acme/model", ErrAppCredentials},
		"a clock so far behind that the JWT has expired": {func(_ *fakeGitHub, c *Client) {
			c.now = func() time.Time { return time.Now().Add(-time.Hour) }
		}, 100, "acme/model", ErrAppCredentials},
		"the primary rate limit":   {func(f *fakeGitHub, _ *Client) { f.rateLimit = http.StatusForbidden }, 100, "acme/model", ErrRateLimited},
		"the secondary rate limit": {func(f *fakeGitHub, _ *Client) { f.rateLimit = http.StatusTooManyRequests }, 100, "acme/model", ErrRateLimited},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			tc.arrange(f, client)
			token, err := client.Token(ctx, tc.installation, tc.repo, Read)
			if !errors.Is(err, tc.want) || token != nil {
				t.Fatalf("Token = %v, %v, want %v", token, err, tc.want)
			}
		})
	}
}

// An installation whose owner has not accepted every permission the App asks for is refused with
// those it lacks named, whatever else it holds.
func TestAnInstallationThatHasNotGrantedTheAppsPermissionsIsRefusedNamingThem(t *testing.T) {
	f, client := newFakeGitHub(t)
	if _, err := client.Installation(ctx, 300); err != nil {
		t.Fatalf("an installation that granted everything: %v", err)
	}
	f.installations[100].withheld = "administration"
	_, err := client.Installation(ctx, 100)
	var missing *PermissionMissingError
	if !errors.As(err, &missing) || !slices.Equal(missing.Missing, []string{"administration:write"}) || !errors.Is(err, ErrPermissionNotGranted) {
		t.Fatalf("an installation without administration: got = %v, want administration:write named", err)
	}
	f.ungranted = true
	if _, err := client.Installation(ctx, 300); !errors.As(err, &missing) || !slices.Equal(missing.Missing, []string{"administration:write", "contents:write"}) {
		t.Fatalf("an installation that holds contents read alone: got = %v, want administration:write and contents:write named", err)
	}
}

// GitHub has answered a scoped installation token with a bare 500: that reads as GitHub failing,
// for the installation the call was made for, as does a read with a token minted before.
func TestGitHubFailingACallNamesTheInstallation(t *testing.T) {
	_, client := newFakeGitHub(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	token, err := client.Token(ctx, 100, "acme/model", Write)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"File":         func() error { _, err := client.File(ctx, token, "", "README.md"); return err },
		"HasBranch":    func() error { _, err := client.HasBranch(ctx, token, "main"); return err },
		"Installation": func() error { _, err := client.Installation(ctx, 100); return err },
		"Repositories": func() error { _, err := client.Repositories(ctx, 100); return err },
		"Token":        func() error { _, err := client.Token(ctx, 100, "acme/model", Write); return err },
		"CreateFromTemplate": func() error {
			_, err := client.CreateFromTemplate(ctx, 100, testTemplate, "fresh", "")
			return err
		},
	}
	for _, server := range []struct {
		url    string
		status int
	}{{broken.URL, http.StatusInternalServerError}, {"http://127.0.0.1:1", 0}} {
		client.apiURL = server.url
		for name, call := range calls {
			var failed *FailedError
			if err := call(); !errors.As(err, &failed) || failed.Installation != 100 || failed.Status != server.status || failed.Message != "" {
				t.Errorf("%s with GitHub answering %d: got = %v, want a FailedError of installation 100", name, server.status, err)
			}
		}
	}
}

// GitHub answered one mint of a token for git 500 among many that worked, and the agent's git
// command failed with it: a mint GitHub failed is asked again, and one it refused is not.
func TestAMintGitHubFailedIsAskedAgain(t *testing.T) {
	f, client := newFakeGitHub(t)
	for _, tc := range []struct {
		status, failures, wantMints int
		ok                          bool
	}{
		{http.StatusInternalServerError, 1, 2, true},
		{http.StatusBadGateway, 2, 3, true},
		{http.StatusInternalServerError, 3, 3, false},
		{http.StatusUnprocessableEntity, 1, 1, false},
	} {
		failures, mints := tc.failures, 0
		flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/access_tokens") {
				mints++
				if failures > 0 {
					failures--
					f.refuse(w, tc.status, "")
					return
				}
			}
			f.serve(w, r)
		}))
		client.apiURL = flaky.URL
		token, err := client.Token(ctx, 100, "acme/model", Write)
		flaky.Close()
		if (err == nil) != tc.ok || (token != nil) != tc.ok || mints != tc.wantMints {
			t.Errorf("GitHub answering %d %d times: Token = %v, %v after %d mints, want ok %v after %d", tc.status, tc.failures, token, err, mints, tc.ok, tc.wantMints)
		}
	}
}

// A clock a little ahead of GitHub's must still produce a JWT GitHub takes as already issued.
func TestTheAppJWTToleratesAClockThatRunsAhead(t *testing.T) {
	_, client := newFakeGitHub(t)
	client.now = func() time.Time { return time.Now().Add(45 * time.Second) }
	if _, err := client.Token(ctx, 100, "acme/model", Read); err != nil {
		t.Fatalf("Token with the clock 45s ahead: %v", err)
	}
}

func TestATokenThatReachesFurtherThanAskedIsNotHandedOn(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.widen = true
	token, err := client.Token(ctx, 100, "acme/model", Read)
	if err == nil || token != nil {
		t.Fatalf("Token = %+v, %v, want a refusal of a token that names two repositories", token, err)
	}
}

func TestFile(t *testing.T) {
	f, client := newFakeGitHub(t)
	readToken, err := client.Token(ctx, 100, "acme/model", Read)
	if err != nil || readToken.Repository != "Acme/Model" {
		t.Fatalf("Token = %+v, %v, want it to name the repository as GitHub does", readToken, err)
	}
	data, err := client.File(ctx, readToken, "main", ".hypothesisloop/experiment.yaml")
	if err != nil || string(data) != "name: from the repo\n" {
		t.Fatalf("File = %q, %v", data, err)
	}
	if minted := f.lastToken(); minted.permissions["contents"] != "read" || len(minted.repos) != 1 || minted.repos[0] != 1 {
		t.Fatalf("the file was read with %+v, want read on repository 1", minted)
	}
	if data, err := client.File(ctx, readToken, "", ".hypothesisloop/experiment.yaml"); err != nil || string(data) != "name: from the repo\n" {
		t.Fatalf("File at the default branch = %q, %v", data, err)
	}
	for name, tc := range map[string]struct {
		ref, path string
		want      error
	}{
		"a path the repository does not hold":   {"main", ".hypothesisloop/job.yaml", ErrFileNotFound},
		"a branch the repository does not have": {"gone", ".hypothesisloop/experiment.yaml", ErrFileNotFound},
		"a directory":                           {"main", ".hypothesisloop", ErrNotAFile},
		"a symlink":                             {"main", "link", ErrNotAFile},
		"a submodule":                           {"main", "vendored", ErrNotAFile},
	} {
		if _, err := client.File(ctx, readToken, tc.ref, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("%s: got = %v, want %v", name, err, tc.want)
		}
	}
	for _, path := range []string{"../other", "a//b", "/etc/passwd", "a/./b", ""} {
		before := len(f.calls)
		if _, err := client.File(ctx, readToken, "main", path); err == nil {
			t.Errorf("File(%q) was read", path)
		}
		for _, call := range f.calls[before:] {
			if strings.Contains(call, "contents") {
				t.Errorf("File(%q) reached GitHub as %s", path, call)
			}
		}
	}
	// A file up to the limit is read whole, though base64 makes its answer a third larger.
	for _, size := range []int{0, 750 << 10, 900 << 10, MaxFileBytes} {
		f.repos["acme/model"].files["sized"] = strings.Repeat("y", size)
		if data, err := client.File(ctx, readToken, "main", "sized"); err != nil || len(data) != size {
			t.Errorf("a file of %d bytes: read %d, %v", size, len(data), err)
		}
	}
	f.repos["acme/model"].files["big"] = strings.Repeat("x", MaxFileBytes+1)
	if _, err := client.File(ctx, readToken, "main", "big"); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("a file over the limit: got = %v, want %v", err, ErrFileTooLarge)
	}
}

// Whatever fails, the error is logged and may reach a caller: it must hold no credential.
func TestNoErrorHoldsASecret(t *testing.T) {
	arrangements := map[string]func(*fakeGitHub, *Client){
		"installation gone": func(f *fakeGitHub, _ *Client) { delete(f.installations, 100) },
		"rate limited":      func(f *fakeGitHub, _ *Client) { f.rateLimit = http.StatusForbidden },
		"bad key":           func(_ *fakeGitHub, c *Client) { c.key, _ = rsa.GenerateKey(rand.Reader, 2048) },
		"server gone":       func(f *fakeGitHub, _ *Client) { f.server.Close() },
		"server error": func(f *fakeGitHub, c *Client) {
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, `{"message": "echo %s"}`, strings.Repeat("y", 5000))
			}))
			f.t.Cleanup(broken.Close)
			c.apiURL, c.webURL = broken.URL, broken.URL
		},
		"repo dropped": func(f *fakeGitHub, _ *Client) { delete(f.installations[100].repos, 1) },
	}
	for name, arrange := range arrangements {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			jwt, err := client.appJWT()
			if err != nil {
				t.Fatal(err)
			}
			readToken, err := client.Token(ctx, 100, "acme/model", Read)
			if err != nil {
				t.Fatal(err)
			}
			arrange(f, client)
			var failures []error
			_, err = client.Installation(ctx, 100)
			failures = append(failures, err)
			_, err = client.Repositories(ctx, 100)
			failures = append(failures, err)
			_, err = client.Token(ctx, 100, "acme/model", Write)
			failures = append(failures, err)
			_, err = client.File(ctx, readToken, "main", ".hypothesisloop/experiment.yaml")
			failures = append(failures, err)
			_, err = client.CreateFromTemplate(ctx, 100, testTemplate, "fresh", "")
			failures = append(failures, err)
			_, err = client.UserInstallations(ctx, testClientSecret, "code-alice")
			failures = append(failures, err)
			secrets := []string{"ghs_", "gho_", testClientSecret, "code-alice", jwt[:40], "PRIVATE KEY", "Bearer"}
			for _, failure := range failures {
				if failure == nil {
					continue
				}
				if len(failure.Error()) > 700 {
					t.Errorf("an error of %d characters repeats what the server sent: %.120s", len(failure.Error()), failure)
				}
				for _, secret := range secrets {
					if strings.Contains(failure.Error(), secret) {
						t.Errorf("error %q holds %q", failure, secret)
					}
				}
			}
		})
	}
}

func TestNewRefusesAKeyItCannotSignWith(t *testing.T) {
	for name, key := range map[string]string{
		"empty": "", "not pem": "ssh-rsa AAAA", "pem of something else": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		_, err := New(config.GitHubConfig{AppID: 1, AppSlug: "s"}, key)
		if err == nil {
			t.Errorf("%s: New accepted the key", name)
		} else if key != "" && strings.Contains(err.Error(), "AAAA") {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
}

func TestValidRepository(t *testing.T) {
	for _, name := range []string{"acme/model", "Acme-Inc/model.v2", "a/b_c-d.e"} {
		if err := ValidRepository(name); err != nil {
			t.Errorf("ValidRepository(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "model", "acme/", "/model", "acme/model/extra", "acme/..", "acme/.", "acme/mo del", "acme/model?x=1", "acme/model#x", "-acme/model", "acme/" + strings.Repeat("a", 200), "https://github.com/acme/model"} {
		if err := ValidRepository(name); !errors.Is(err, ErrInvalidRepository) {
			t.Errorf("ValidRepository(%q) = %v, want a refusal", name, err)
		}
	}
}

func TestTokenNamesTheRepositoryInGitHubsOwnCasing(t *testing.T) {
	f, client := newFakeGitHub(t)
	token, err := client.Token(ctx, 100, "ACME/model", Read)
	if err != nil || token.Repository != "Acme/Model" {
		t.Fatalf("Token = %+v, %v, want the name as GitHub spells it", token, err)
	}
	f.repos["acme/model"].fullName = ""
	if token, err := client.Token(ctx, 100, "acme/model", Read); err == nil {
		t.Fatalf("a token whose answer does not name its repository was handed on: %+v", token)
	}
	f.repos["acme/model"].fullName = "Acme/Model"
	f.ungranted = true
	_, err = client.Token(ctx, 100, "acme/model", Write)
	var missing *PermissionMissingError
	if !errors.Is(err, ErrPermissionNotGranted) || !errors.As(err, &missing) || !slices.Equal(missing.Missing, []string{"contents:write"}) || missing.Installation != 100 {
		t.Fatalf("a permission the installation has not granted: got = %v, want it named as contents:write", err)
	}
}

// wrapped breaks base64 into lines of sixty characters, as GitHub sends a file.
func wrapped(encoded string) string {
	var b strings.Builder
	for len(encoded) > 60 {
		b.WriteString(encoded[:60] + "\n")
		encoded = encoded[60:]
	}
	b.WriteString(encoded + "\n")
	return b.String()
}

// An authorization code is exchanged with the App's client for the person's token, once, and the
// installations listed are those GitHub lists for that person, every page of them.
func TestUserInstallationsAreThoseGitHubListsForThePersonTheCodeWasIssuedTo(t *testing.T) {
	f, client := newFakeGitHub(t)
	if got, err := client.UserInstallations(ctx, testClientSecret, "code-alice"); err != nil || !slices.Equal(got, []int64{100, 300}) {
		t.Fatalf("alice reaches %v, %v, want 100 and 300", got, err)
	}
	if _, err := client.UserInstallations(ctx, testClientSecret, "code-alice"); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("a code used twice: %v, want ErrAuthorizationInvalid", err)
	}
	if _, err := client.UserInstallations(ctx, testClientSecret, "code-made-up"); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("a made-up code: %v, want ErrAuthorizationInvalid", err)
	}
	if _, err := client.UserInstallations(ctx, "a-wrong-secret", "code-mallory"); !errors.Is(err, ErrAppCredentials) {
		t.Fatalf("a wrong client secret: %v, want ErrAppCredentials", err)
	}
	many := make([]int64, pageSize+5)
	for i := range many {
		many[i] = int64(1000 + i)
	}
	f.codes["code-many"], f.reaches["gho_many"] = "gho_many", many
	for _, id := range many {
		f.installations[id] = &fakeInstallation{account: "org" + strconv.FormatInt(id, 10)}
	}
	if got, err := client.UserInstallations(ctx, testClientSecret, "code-many"); err != nil || !slices.Equal(got, many) {
		t.Fatalf("got %d installations, %v, want all %d across pages", len(got), err, len(many))
	}
}
