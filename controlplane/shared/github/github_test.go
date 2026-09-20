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
		content, ok := repo.files[path]
		if ref := r.URL.Query().Get("ref"); !ok || (ref != "" && ref != repo.defaultBranch) {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		f.answer(w, http.StatusOK, map[string]any{
			"type": "file", "size": len(content), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(content))[:4] + "\n" + base64.StdEncoding.EncodeToString([]byte(content))[4:] + "\n",
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
	repositories := []map[string]any{}
	for _, repoID := range body.RepositoryIDs {
		if !installation.repos[repoID] {
			f.refuse(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
		repositories = append(repositories, map[string]any{"id": repoID})
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
	repo, err := client.Repository(ctx, 100, "acme/model", "code-alice")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.ID != 1 || repo.FullName != "Acme/Model" || repo.DefaultBranch != "main" {
		t.Fatalf("repository = %+v, want id 1, GitHub's own casing and its default branch", repo)
	}
	minted := f.lastToken()
	if len(minted.repos) != 1 || minted.repos[0] != 1 || minted.permissions["contents"] != "read" || minted.permissions["metadata"] != "read" || len(minted.permissions) != 2 {
		t.Fatalf("the token used to look the repository up = %+v, want read on repository 1 alone", minted)
	}
	if _, used := f.codes["code-alice"]; used {
		t.Fatal("the authorisation code was not exchanged")
	}
}

// An installation id is a small number. Naming someone else's must not attach their repository.
func TestRepositoryRefusesWhatTheCallerCannotProve(t *testing.T) {
	cases := map[string]struct {
		installation int64
		repo, code   string
		want         error
	}{
		"a repository of another account's installation, guessed": {200, "rival/secret", "code-alice", ErrRepositoryNotInstalled},
		"the right repository under a guessed installation":       {200, "acme/model", "code-alice", ErrRepositoryNotInstalled},
		"an installation that does not exist":                     {999, "acme/model", "code-alice", ErrRepositoryNotInstalled},
		"a repository the App was not installed on":               {100, "acme/unlisted", "code-alice", ErrRepositoryNotInstalled},
		"a repository that does not exist":                        {100, "acme/nothing", "code-alice", ErrRepositoryNotInstalled},
		"a person who can only read it":                           {100, "acme/model", "code-reader", ErrUserCannotPush},
		"a person who cannot see it":                              {100, "acme/model", "code-mallory", ErrRepositoryNotInstalled},
		"a code github never issued":                              {100, "acme/model", "code-made-up", ErrCodeRejected},
		"no code":                                                 {100, "acme/model", "", ErrCodeRejected},
		"a name that is no repository":                            {100, "acme/model/../../app", "code-alice", ErrInvalidRepository},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			repo, err := client.Repository(ctx, tc.installation, tc.repo, tc.code)
			if !errors.Is(err, tc.want) || repo != nil {
				t.Fatalf("Repository = %+v, %v, want %v", repo, err, tc.want)
			}
			if len(f.tokens) != 0 {
				t.Fatalf("an installation token was minted for a refused attach: %+v", f.tokens)
			}
		})
	}
}

func TestACodeWorksOnce(t *testing.T) {
	_, client := newFakeGitHub(t)
	if _, err := client.Repository(ctx, 100, "acme/model", "code-alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Repository(ctx, 100, "acme/other", "code-alice"); !errors.Is(err, ErrCodeRejected) {
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
	data, err := client.File(ctx, 100, 1, "main", ".hypothesisloop/experiment.yaml")
	if err != nil || string(data) != "name: from the repo\n" {
		t.Fatalf("File = %q, %v", data, err)
	}
	if minted := f.lastToken(); minted.permissions["contents"] != "read" || len(minted.repos) != 1 || minted.repos[0] != 1 {
		t.Fatalf("the file was read with %+v, want read on repository 1", minted)
	}
	if data, err := client.File(ctx, 100, 1, "", ".hypothesisloop/experiment.yaml"); err != nil || string(data) != "name: from the repo\n" {
		t.Fatalf("File at the default branch = %q, %v", data, err)
	}
	for name, tc := range map[string]struct {
		ref, path string
		want      error
	}{
		"a path the repository does not hold":   {"main", ".hypothesisloop/job.yaml", ErrFileNotFound},
		"a branch the repository does not have": {"gone", ".hypothesisloop/experiment.yaml", ErrFileNotFound},
		"a directory":                           {"main", ".hypothesisloop", ErrFileNotFound},
	} {
		if _, err := client.File(ctx, 100, 1, tc.ref, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("%s: got = %v, want %v", name, err, tc.want)
		}
	}
	for _, path := range []string{"../other", "a//b", "/etc/passwd", "a/./b", ""} {
		before := len(f.calls)
		if _, err := client.File(ctx, 100, 1, "main", path); err == nil {
			t.Errorf("File(%q) was read", path)
		}
		for _, call := range f.calls[before:] {
			if strings.Contains(call, "contents") {
				t.Errorf("File(%q) reached GitHub as %s", path, call)
			}
		}
	}
	f.repos["acme/model"].files["big"] = strings.Repeat("x", MaxFileBytes+1)
	if _, err := client.File(ctx, 100, 1, "main", "big"); !errors.Is(err, ErrFileTooLarge) {
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
			arrange(f, client)
			var failures []error
			_, err = client.Repository(ctx, 100, "acme/model", "code-alice")
			failures = append(failures, err)
			_, err = client.Token(ctx, 100, 1, Write)
			failures = append(failures, err)
			_, err = client.File(ctx, 100, 1, "main", ".hypothesisloop/experiment.yaml")
			failures = append(failures, err)
			secrets := []string{testClientSecret, "code-alice", "ghs_", "ghu_", jwt[:40], "PRIVATE KEY", "Bearer"}
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
