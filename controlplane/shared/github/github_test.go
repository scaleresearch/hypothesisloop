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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

const (
	testRedirect     = "http://localhost:3000/settings/repository"
	testAppID        = 4242
	testClientSecret = "client-secret-only-this-test-knows"
)

type fakeRepo struct {
	id            int64
	fullName      string
	defaultBranch string
	files         map[string]string
	directories   map[string]bool
}

type fakeInstallation struct {
	suspended bool
	repos     map[int64]bool
}

type mintedToken struct {
	installation int64
	repos        []int64
	permissions  map[string]string
}

// fakeGitHub answers the calls the client makes the way GitHub does: it checks the App JWT
// against the App's public key, limits an installation token to the repositories it was minted
// for, and limits a user token to what that user can see.
type fakeGitHub struct {
	t             *testing.T
	server        *httptest.Server
	key           *rsa.PublicKey
	now           func() time.Time
	installations map[int64]*fakeInstallation
	repos         map[string]*fakeRepo
	// codes maps a user authorisation code to a user, pushers the repositories each may push to,
	// readers the ones each may only read.
	codes   map[string]string
	pushers map[string]map[int64]bool
	readers map[string]map[int64]bool

	tokens     map[string]mintedToken
	userTokens map[string]string
	sequence   int
	// rateLimit, when set, answers every API call with that status as a rate limit.
	rateLimit int
	// widen makes a minted token claim a second repository.
	widen bool
	// ungranted makes the installation refuse the permissions asked for, saml blocks user tokens
	// from repositories, and revokeStatus, when set, answers a revocation with that status.
	ungranted    bool
	saml         bool
	revokeStatus int
	revoked      []string
	redirects    []string
	// kinds holds what a path is when it is not a file: dir, symlink or submodule.
	kinds map[string]string
	calls []string
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
			100: {repos: map[int64]bool{1: true, 2: true}},
			200: {repos: map[int64]bool{3: true}},
		},
		repos: map[string]*fakeRepo{
			"acme/model":    {id: 1, fullName: "Acme/Model", defaultBranch: "main", files: map[string]string{".hypothesisloop/experiment.yaml": "name: from the repo\n"}, directories: map[string]bool{".hypothesisloop": true}},
			"acme/other":    {id: 2, fullName: "acme/other", defaultBranch: "trunk"},
			"rival/secret":  {id: 3, fullName: "rival/secret", defaultBranch: "main"},
			"acme/unlisted": {id: 4, fullName: "acme/unlisted", defaultBranch: "main"},
		},
		codes:   map[string]string{"code-alice": "alice", "code-reader": "reader", "code-mallory": "mallory"},
		pushers: map[string]map[int64]bool{"alice": {1: true, 2: true, 4: true}},
		readers: map[string]map[int64]bool{"reader": {1: true}},
		tokens:  map[string]mintedToken{}, userTokens: map[string]string{},
		kinds: map[string]string{"link": "symlink", "vendored": "submodule"},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	client, err := New(config.GitHubConfig{
		AppID: testAppID, AppSlug: "hypothesisloop", ClientID: "Iv23test", ClientSecret: testClientSecret,
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.apiURL, client.webURL = f.server.URL, f.server.URL
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
		return false, false
	}
	if user, ok := f.userTokens[bearer]; ok {
		return f.pushers[user][repo.id] || f.readers[user][repo.id], f.pushers[user][repo.id]
	}
	return false, false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/login/oauth/access_token" {
		r.ParseForm() //nolint:errcheck
		if r.PostForm.Get("client_secret") != testClientSecret {
			f.answer(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		f.redirects = append(f.redirects, r.PostForm.Get("redirect_uri"))
		if r.PostForm.Get("redirect_uri") != testRedirect {
			f.answer(w, http.StatusOK, map[string]string{"error": "redirect_uri_mismatch"})
			return
		}
		user, ok := f.codes[r.PostForm.Get("code")]
		if !ok {
			f.answer(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
			return
		}
		delete(f.codes, r.PostForm.Get("code"))
		f.sequence++
		token := fmt.Sprintf("ghu_user_%d", f.sequence)
		f.userTokens[token] = user
		f.answer(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "bearer"})
		return
	}
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
	case len(segments) == 3 && segments[0] == "applications" && segments[2] == "token" && r.Method == http.MethodDelete:
		id, secret, _ := r.BasicAuth()
		var body struct {
			AccessToken string `json:"access_token"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		switch {
		case id != "Iv23test" || secret != testClientSecret || segments[1] != "Iv23test":
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
		case f.revokeStatus != 0:
			f.refuse(w, f.revokeStatus, "revocation failed")
		default:
			if _, known := f.userTokens[body.AccessToken]; !known {
				f.refuse(w, http.StatusUnprocessableEntity, "Validation Failed")
				return
			}
			delete(f.userTokens, body.AccessToken)
			f.revoked = append(f.revoked, body.AccessToken)
			w.WriteHeader(http.StatusNoContent)
		}
	case len(segments) == 4 && segments[0] == "app" && segments[1] == "installations" && segments[3] == "access_tokens" && r.Method == http.MethodPost:
		f.mint(w, r, bearer, segments[2])
	case len(segments) == 4 && segments[0] == "repos" && segments[3] == "installation":
		if !f.appAuthenticated(bearer) {
			f.refuse(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		for id, installation := range f.installations {
			if repo != nil && installation.repos[repo.id] {
				body := map[string]any{"id": id, "suspended_at": nil}
				if installation.suspended {
					body["suspended_at"] = "2026-01-01T00:00:00Z"
				}
				f.answer(w, http.StatusOK, body)
				return
			}
		}
		f.refuse(w, http.StatusNotFound, "Not Found")
	case len(segments) == 3 && segments[0] == "repos":
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		if _, isUser := f.userTokens[bearer]; !isUser {
			if _, isInstallation := f.tokens[bearer]; !isInstallation {
				f.refuse(w, http.StatusUnauthorized, "Bad credentials")
				return
			}
		}
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if _, isUser := f.userTokens[bearer]; isUser && f.saml {
			f.refuse(w, http.StatusForbidden, "Resource protected by organization SAML enforcement. You must grant your OAuth token access to this organization.")
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
	case len(segments) >= 4 && segments[0] == "repositories" && segments[2] == "contents":
		id, _ := strconv.ParseInt(segments[1], 10, 64)
		repo := f.repoByID(id)
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if read, _ := f.visible(bearer, repo); !read {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		path := strings.Join(segments[3:], "/")
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
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.RepositoryIDs) == 0 {
		f.t.Errorf("a token was asked for without naming its repository: %v", err)
		f.refuse(w, http.StatusBadRequest, "bad body")
		return
	}
	if f.ungranted {
		f.refuse(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
		return
	}
	repositories := []map[string]any{}
	for _, repoID := range body.RepositoryIDs {
		if !installation.repos[repoID] {
			f.refuse(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
		repositories = append(repositories, map[string]any{"id": repoID, "full_name": f.repoByID(repoID).fullName})
	}
	if f.widen {
		repositories = append(repositories, map[string]any{"id": 999})
	}
	f.sequence++
	token := fmt.Sprintf("ghs_installation_%d", f.sequence)
	f.tokens[token] = mintedToken{installation: id, repos: body.RepositoryIDs, permissions: body.Permissions}
	f.answer(w, http.StatusCreated, map[string]any{
		"token": token, "expires_at": f.now().Add(time.Hour).UTC().Format(time.RFC3339), "repositories": repositories,
	})
}

func (f *fakeGitHub) lastToken() mintedToken {
	return f.tokens[fmt.Sprintf("ghs_installation_%d", f.sequence)]
}

var ctx = context.Background()

func TestRepositoryIsVerifiedThreeWaysAndReturnedAsGitHubStatesIt(t *testing.T) {
	f, client := newFakeGitHub(t)
	repo, err := client.Repository(ctx, "acme/model", "code-alice", testRedirect)
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.ID != 1 || repo.FullName != "Acme/Model" || repo.InstallationID != 100 {
		t.Fatalf("repository = %+v, want id 1 in GitHub's own casing under the installation GitHub names", repo)
	}
	if len(f.revoked) != 1 || len(f.userTokens) != 0 || len(f.redirects) != 1 || f.redirects[0] != testRedirect {
		t.Fatalf("revoked %v with %d user tokens left, redirects %v: want the person's token revoked and the redirect sent", f.revoked, len(f.userTokens), f.redirects)
	}
	minted := f.lastToken()
	if len(minted.repos) != 1 || minted.repos[0] != 1 || minted.permissions["contents"] != "read" || minted.permissions["metadata"] != "read" || len(minted.permissions) != 2 {
		t.Fatalf("the token used to look the repository up = %+v, want read on repository 1 alone", minted)
	}
	if _, used := f.codes["code-alice"]; used {
		t.Fatal("the authorisation code was not exchanged")
	}
}

// The caller names a repository and nothing else: the installation is GitHub's to state.
func TestRepositoryRefusesWhatTheCallerCannotProve(t *testing.T) {
	cases := map[string]struct {
		repo, code string
		want       error
		codeKept   bool
	}{
		"a repository of another account's installation": {"rival/secret", "code-alice", ErrRepositoryNotInstalled, false},
		"a repository the App was not installed on":      {"acme/unlisted", "code-alice", ErrRepositoryNotInstalled, false},
		"a repository that does not exist":               {"acme/nothing", "code-alice", ErrRepositoryNotInstalled, false},
		"a person who can only read it":                  {"acme/model", "code-reader", ErrUserCannotPush, false},
		"a person who cannot see it":                     {"acme/model", "code-mallory", ErrRepositoryNotInstalled, false},
		"a code github never issued":                     {"acme/model", "code-made-up", ErrCodeRejected, false},
		"no code":                                        {"acme/model", "", ErrCodeRejected, false},
		// A malformed name is refused before the code is spent, so the person need not authorise again.
		"a name that is no repository": {"acme/model/../../app", "code-alice", ErrInvalidRepository, true},
		"an address for a name":        {"https://github.com/acme/model", "code-alice", ErrInvalidRepository, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			repo, err := client.Repository(ctx, tc.repo, tc.code, testRedirect)
			if !errors.Is(err, tc.want) || repo != nil {
				t.Fatalf("Repository = %+v, %v, want %v", repo, err, tc.want)
			}
			if len(f.tokens) != 0 {
				t.Fatalf("an installation token was minted for a refused attach: %+v", f.tokens)
			}
			if len(f.userTokens) != 0 {
				t.Fatalf("a refused attach left the person's token alive: %v", f.userTokens)
			}
			if _, kept := f.codes["code-alice"]; tc.codeKept && !kept {
				t.Fatal("the code was spent on a name that cannot be a repository")
			}
		})
	}
}

// Two installations of the App: each repository is attached under the one GitHub names for it.
func TestRepositoryIsFoundUnderWhicheverInstallationCoversIt(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.pushers["alice"][3] = true
	repo, err := client.Repository(ctx, "rival/secret", "code-alice", testRedirect)
	if err != nil || repo.ID != 3 || repo.InstallationID != 200 {
		t.Fatalf("Repository = %+v, %v, want repository 3 under installation 200", repo, err)
	}
	if minted := f.lastToken(); len(minted.repos) != 1 || minted.repos[0] != 3 {
		t.Fatalf("the token minted = %+v, want repository 3 alone", minted)
	}
}

func TestACodeWorksOnce(t *testing.T) {
	_, client := newFakeGitHub(t)
	if _, err := client.Repository(ctx, "acme/model", "code-alice", testRedirect); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Repository(ctx, "acme/other", "code-alice", testRedirect); !errors.Is(err, ErrCodeRejected) {
		t.Fatalf("a second attach on one code: got = %v, want %v", err, ErrCodeRejected)
	}
}

func TestTokenIsLimitedToOneRepositoryAndTheAccessAsked(t *testing.T) {
	f, client := newFakeGitHub(t)
	for _, access := range []Access{Read, Write} {
		token, err := client.Token(ctx, 100, 2, access)
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
	if _, err := client.Token(ctx, 100, 2, "admin"); err == nil {
		t.Fatal("Token minted an access that is neither read nor write")
	}
}

// Two projects share installation 100. The token of one must not read the other's repository.
func TestATokenOfOneRepositoryDoesNotReachItsSiblingOnTheSameInstallation(t *testing.T) {
	f, client := newFakeGitHub(t)
	token, err := client.Token(ctx, 100, 1, Write)
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
		repo         int64
		want         error
	}{
		"the installation was removed":                     {func(f *fakeGitHub, _ *Client) { delete(f.installations, 100) }, 100, 1, ErrInstallationGone},
		"the installation was suspended":                   {func(f *fakeGitHub, _ *Client) { f.installations[100].suspended = true }, 100, 1, ErrInstallationGone},
		"the repository was taken out of the installation": {func(f *fakeGitHub, _ *Client) { delete(f.installations[100].repos, 1) }, 100, 1, ErrRepositoryNotInstalled},
		"a repository of another installation":             {func(*fakeGitHub, *Client) {}, 100, 3, ErrRepositoryNotInstalled},
		"the App's key was rotated out": {func(_ *fakeGitHub, c *Client) {
			c.key, _ = rsa.GenerateKey(rand.Reader, 2048)
		}, 100, 1, ErrAppCredentials},
		"a clock so far behind that the JWT has expired": {func(_ *fakeGitHub, c *Client) {
			c.now = func() time.Time { return time.Now().Add(-time.Hour) }
		}, 100, 1, ErrAppCredentials},
		"the primary rate limit":   {func(f *fakeGitHub, _ *Client) { f.rateLimit = http.StatusForbidden }, 100, 1, ErrRateLimited},
		"the secondary rate limit": {func(f *fakeGitHub, _ *Client) { f.rateLimit = http.StatusTooManyRequests }, 100, 1, ErrRateLimited},
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

// A clock a little ahead of GitHub's must still produce a JWT GitHub takes as already issued.
func TestTheAppJWTToleratesAClockThatRunsAhead(t *testing.T) {
	_, client := newFakeGitHub(t)
	client.now = func() time.Time { return time.Now().Add(45 * time.Second) }
	if _, err := client.Token(ctx, 100, 1, Read); err != nil {
		t.Fatalf("Token with the clock 45s ahead: %v", err)
	}
}

func TestATokenThatReachesFurtherThanAskedIsNotHandedOn(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.widen = true
	token, err := client.Token(ctx, 100, 1, Read)
	if err == nil || token != nil {
		t.Fatalf("Token = %+v, %v, want a refusal of a token that names two repositories", token, err)
	}
}

func TestFile(t *testing.T) {
	f, client := newFakeGitHub(t)
	readToken, err := client.Token(ctx, 100, 1, Read)
	if err != nil || readToken.Repository != "Acme/Model" {
		t.Fatalf("Token = %+v, %v, want it to name the repository as GitHub does", readToken, err)
	}
	data, err := client.File(ctx, readToken, 1, "main", ".hypothesisloop/experiment.yaml")
	if err != nil || string(data) != "name: from the repo\n" {
		t.Fatalf("File = %q, %v", data, err)
	}
	if minted := f.lastToken(); minted.permissions["contents"] != "read" || len(minted.repos) != 1 || minted.repos[0] != 1 {
		t.Fatalf("the file was read with %+v, want read on repository 1", minted)
	}
	if data, err := client.File(ctx, readToken, 1, "", ".hypothesisloop/experiment.yaml"); err != nil || string(data) != "name: from the repo\n" {
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
		if _, err := client.File(ctx, readToken, 1, tc.ref, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("%s: got = %v, want %v", name, err, tc.want)
		}
	}
	for _, path := range []string{"../other", "a//b", "/etc/passwd", "a/./b", ""} {
		before := len(f.calls)
		if _, err := client.File(ctx, readToken, 1, "main", path); err == nil {
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
		if data, err := client.File(ctx, readToken, 1, "main", "sized"); err != nil || len(data) != size {
			t.Errorf("a file of %d bytes: read %d, %v", size, len(data), err)
		}
	}
	f.repos["acme/model"].files["big"] = strings.Repeat("x", MaxFileBytes+1)
	if _, err := client.File(ctx, readToken, 1, "main", "big"); !errors.Is(err, ErrFileTooLarge) {
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
			readToken, err := client.Token(ctx, 100, 1, Read)
			if err != nil {
				t.Fatal(err)
			}
			arrange(f, client)
			var failures []error
			_, err = client.Repository(ctx, "acme/model", "code-alice", testRedirect)
			failures = append(failures, err)
			_, err = client.Token(ctx, 100, 1, Write)
			failures = append(failures, err)
			_, err = client.File(ctx, readToken, 1, "main", ".hypothesisloop/experiment.yaml")
			failures = append(failures, err)
			secrets := []string{testClientSecret, "code-alice", "ghs_", "ghu_", jwt[:40], "PRIVATE KEY", "Bearer", "Basic"}
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
		_, err := New(config.GitHubConfig{AppID: 1, AppSlug: "s", ClientID: "c", ClientSecret: "s", PrivateKey: key})
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

func TestAppAddresses(t *testing.T) {
	_, client := newFakeGitHub(t)
	client.webURL = webURL
	app := client.App()
	if app.InstallURL != "https://github.com/apps/hypothesisloop/installations/new" ||
		app.AuthorizeURL != "https://github.com/login/oauth/authorize?client_id=Iv23test" || app.Slug != "hypothesisloop" || app.ClientID != "Iv23test" {
		t.Fatalf("app = %+v", app)
	}
	if CloneURL("Acme/Model") != "https://github.com/Acme/Model.git" {
		t.Fatalf("clone url = %s", CloneURL("Acme/Model"))
	}
}

func TestAttachProofRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange  func(*fakeGitHub)
		redirect string
		want     error
	}{
		"an organisation whose single sign-on blocks the token": {func(f *fakeGitHub) { f.saml = true }, testRedirect, ErrUserBlocked},
		"a redirect the code was not issued to":                 {func(*fakeGitHub) {}, "https://evil.example/callback", ErrCodeRejected},
		"an installation that has not granted the access":       {func(f *fakeGitHub) { f.ungranted = true }, testRedirect, ErrPermissionNotGranted},
	} {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			tc.arrange(f)
			if repo, err := client.Repository(ctx, "acme/model", "code-alice", tc.redirect); !errors.Is(err, tc.want) || repo != nil {
				t.Fatalf("Repository = %+v, %v, want %v", repo, err, tc.want)
			}
			if len(f.userTokens) != 0 {
				t.Fatalf("the person's token outlived the refusal: %v", f.userTokens)
			}
		})
	}
}

// A person's token that cannot be revoked would outlive the one question it was issued for.
func TestAnAttachFailsWhenThePersonsTokenCannotBeRevoked(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusUnprocessableEntity} {
		f, client := newFakeGitHub(t)
		f.revokeStatus = status
		repo, err := client.Repository(ctx, "acme/model", "code-alice", testRedirect)
		if err == nil || repo != nil || len(f.tokens) != 0 {
			t.Errorf("revocation answered %d: Repository = %+v, %v with %d installation tokens, want a failure and none", status, repo, err, len(f.tokens))
		}
	}
	// The refusal of the check itself is what the caller hears when both fail.
	f, client := newFakeGitHub(t)
	f.revokeStatus = http.StatusInternalServerError
	if _, err := client.Repository(ctx, "acme/model", "code-reader", testRedirect); !errors.Is(err, ErrUserCannotPush) {
		t.Errorf("a reader whose token could not be revoked: got = %v, want %v", err, ErrUserCannotPush)
	}
}

func TestTokenNamesTheRepositoryAsItStandsNow(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.repos["acme/model"].fullName = "acme/renamed"
	token, err := client.Token(ctx, 100, 1, Read)
	if err != nil || token.Repository != "acme/renamed" {
		t.Fatalf("Token = %+v, %v, want the name after the rename", token, err)
	}
	f.repos["acme/model"].fullName = ""
	if token, err := client.Token(ctx, 100, 1, Read); err == nil {
		t.Fatalf("a token whose answer does not name its repository was handed on: %+v", token)
	}
	f.repos["acme/model"].fullName = "Acme/Model"
	f.ungranted = true
	if _, err := client.Token(ctx, 100, 1, Write); !errors.Is(err, ErrPermissionNotGranted) {
		t.Fatalf("a permission the installation has not granted: got = %v, want %v", err, ErrPermissionNotGranted)
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
