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
	// history is how many commits its default branch holds.
	history int
}

type fakeInstallation struct {
	account string
	// owner is the user whose own account the installation is on, empty for an organisation's.
	owner string
	// creates says the installation granted the App the creation of repositories, and members the
	// organisation permission Members (read), without which GitHub shows the App no membership in
	// the organisation.
	creates   bool
	members   bool
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
	// reaches holds the installations each user reaches, in the order GitHub lists them, and
	// roles the role each holds in the organisation an installation is on: admin or member. A
	// user with no role there reaches it as a collaborator on a repository.
	reaches map[string][]int64
	roles   map[string]map[int64]string
	// foreign makes the user's listing hold an installation of another App.
	foreign bool

	tokens     map[string]mintedToken
	userTokens map[string]string
	// refreshTokens maps each refresh token GitHub still takes to its user. unexpiring is an App
	// whose user tokens do not expire, which issues none and no expiry.
	refreshTokens map[string]string
	unexpiring    bool
	// addStatus, when set, answers the adding of a repository to an installation.
	addStatus int
	sequence  int
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
	// trees holds each tree written, by its sha, and commits the tree of each commit. gitRefuses
	// names the step of writing a commit that is refused: trees, commits or refs.
	trees      map[string]map[string]string
	commits    map[string]string
	gitRefuses string
	// createRefusal, when set, is how the creation of a repository is answered.
	createRefusal *createRefusal
}

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
			100: {account: "Acme", creates: true, members: true, repos: map[int64]bool{1: true, 2: true}},
			200: {account: "rival", members: true, repos: map[int64]bool{3: true}},
			300: {account: "carol", owner: "carol", creates: true, repos: map[int64]bool{}},
		},
		repos: map[string]*fakeRepo{
			"acme/model":    {id: 1, fullName: "Acme/Model", defaultBranch: "main", files: map[string]string{".hypothesisloop/experiment.yaml": "name: from the repo\n"}, directories: map[string]bool{".hypothesisloop": true}},
			"acme/other":    {id: 2, fullName: "acme/other", defaultBranch: "trunk"},
			"rival/secret":  {id: 3, fullName: "rival/secret", defaultBranch: "main"},
			"acme/unlisted": {id: 4, fullName: "acme/unlisted", defaultBranch: "main"},
		},
		codes:   map[string]string{"code-alice": "alice", "code-reader": "reader", "code-mallory": "mallory", "code-carol": "carol"},
		pushers: map[string]map[int64]bool{"alice": {1: true, 2: true, 4: true}},
		readers: map[string]map[int64]bool{"reader": {1: true}},
		reaches: map[string][]int64{"alice": {100}, "reader": {100, 200}, "carol": {100, 300}},
		roles:   map[string]map[int64]string{"alice": {100: "admin"}, "reader": {100: "admin"}, "carol": {100: "member"}},
		tokens:  map[string]mintedToken{}, userTokens: map[string]string{}, refreshTokens: map[string]string{},
		kinds: map[string]string{"link": "symlink", "vendored": "submodule"},
		trees: map[string]map[string]string{}, commits: map[string]string{},
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
		var user string
		if r.PostForm.Get("grant_type") == "refresh_token" {
			// As GitHub does, a refresh token works once.
			spent, ok := f.refreshTokens[r.PostForm.Get("refresh_token")]
			if !ok {
				f.answer(w, http.StatusOK, map[string]string{"error": "bad_refresh_token"})
				return
			}
			delete(f.refreshTokens, r.PostForm.Get("refresh_token"))
			user = spent
		} else {
			f.redirects = append(f.redirects, r.PostForm.Get("redirect_uri"))
			if r.PostForm.Get("redirect_uri") != testRedirect {
				f.answer(w, http.StatusOK, map[string]string{"error": "redirect_uri_mismatch"})
				return
			}
			coded, ok := f.codes[r.PostForm.Get("code")]
			if !ok {
				f.answer(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
				return
			}
			delete(f.codes, r.PostForm.Get("code"))
			user = coded
		}
		f.sequence++
		token := fmt.Sprintf("ghu_user_%d", f.sequence)
		f.userTokens[token] = user
		answer := map[string]any{"access_token": token, "token_type": "bearer"}
		if !f.unexpiring {
			refresh := fmt.Sprintf("ghr_refresh_%d", f.sequence)
			answer["expires_in"], answer["refresh_token"] = 28800, refresh
			f.refreshTokens[refresh] = user
		}
		f.answer(w, http.StatusOK, answer)
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
	case r.URL.Path == "/user/installations":
		user, ok := f.userTokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		all := []map[string]any{}
		for _, id := range f.reaches[user] {
			if f.installations[id] == nil {
				continue
			}
			all = append(all, map[string]any{"id": id, "app_id": testAppID, "account": f.accountOf(id)})
		}
		if f.foreign {
			all = append(all, map[string]any{"id": 900, "app_id": testAppID + 1, "account": map[string]any{"id": 900, "login": "elsewhere", "type": "Organization"}})
		}
		f.answer(w, http.StatusOK, map[string]any{"total_count": len(all), "installations": paged(r, all)})
	case r.URL.Path == "/user":
		user, ok := f.userTokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		f.answer(w, http.StatusOK, map[string]any{"id": userID(user), "login": user})
	case len(segments) == 4 && segments[0] == "user" && segments[1] == "memberships" && segments[2] == "orgs" && r.Method == http.MethodGet:
		user, ok := f.userTokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		for id, installation := range f.installations {
			if installation.owner != "" || !strings.EqualFold(installation.account, segments[3]) {
				continue
			}
			if !installation.members {
				w.Header().Set("X-Accepted-GitHub-Permissions", "members=read")
				f.refuse(w, http.StatusForbidden, "Resource not accessible by integration")
				return
			}
			if role := f.roles[user][id]; role != "" {
				f.answer(w, http.StatusOK, map[string]any{"state": "active", "role": role, "organization": f.accountOf(id)})
				return
			}
		}
		f.refuse(w, http.StatusNotFound, "Not Found")
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
		body := map[string]any{"id": id, "suspended_at": nil, "account": f.accountOf(id), "repository_selection": "selected"}
		if installation.all {
			body["repository_selection"] = "all"
		}
		if installation.suspended {
			body["suspended_at"] = "2026-01-01T00:00:00Z"
		}
		f.answer(w, http.StatusOK, body)
	case r.Method == http.MethodPost && r.URL.Path == "/user/repos":
		var body struct {
			Name     string `json:"name"`
			Private  bool   `json:"private"`
			AutoInit bool   `json:"auto_init"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		// As GitHub does, it creates for the user a token is of, and an installation token is none's.
		if _, ofInstallation := f.tokens[bearer]; ofInstallation {
			f.refuse(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		user, ok := f.userTokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		var own *fakeInstallation
		for _, installation := range f.installations {
			if installation.owner == user {
				own = installation
			}
		}
		switch {
		case f.createRefusal != nil:
			maps.Copy(w.Header(), f.createRefusal.header)
			f.answer(w, f.createRefusal.status, f.createRefusal.body)
		case own == nil || !own.creates:
			w.Header().Set("X-Accepted-GitHub-Permissions", "administration=write; repository_creation=write")
			f.refuse(w, http.StatusForbidden, "Resource not accessible by integration")
		case f.repos[strings.ToLower(user+"/"+body.Name)] != nil:
			f.answer(w, http.StatusUnprocessableEntity, map[string]any{
				"message": "Repository creation failed.",
				"errors":  []map[string]string{{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}},
			})
		case !body.Private || !body.AutoInit:
			f.t.Errorf("a repository was asked for with private %v and auto_init %v", body.Private, body.AutoInit)
		default:
			f.sequence++
			repo := &fakeRepo{id: int64(1000 + f.sequence), fullName: user + "/" + body.Name, defaultBranch: "main", files: map[string]string{"README.md": "# " + body.Name + "\n"}}
			f.repos[strings.ToLower(repo.fullName)] = repo
			// An installation on selected repositories does not cover what its owner makes later.
			if own.all {
				own.repos[repo.id] = true
			}
			f.answer(w, http.StatusCreated, map[string]any{"id": repo.id, "full_name": repo.fullName, "private": true, "default_branch": repo.defaultBranch})
		}
	case r.Method == http.MethodPut && len(segments) == 5 && segments[0] == "user" && segments[1] == "installations" && segments[3] == "repositories":
		id, _ := strconv.ParseInt(segments[2], 10, 64)
		repoID, _ := strconv.ParseInt(segments[4], 10, 64)
		user, ok := f.userTokens[bearer]
		installation, repo := f.installations[id], f.repoByID(repoID)
		switch {
		case !ok:
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
		case f.addStatus != 0:
			f.refuse(w, f.addStatus, "Resource not accessible by integration")
		case installation == nil || installation.owner != user || repo == nil || !strings.HasPrefix(strings.ToLower(repo.fullName), user+"/"):
			f.refuse(w, http.StatusNotFound, "Not Found")
		default:
			installation.repos[repo.id] = true
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPost && len(segments) == 3 && segments[0] == "orgs" && segments[2] == "repos":
		var body struct {
			Name     string `json:"name"`
			Private  bool   `json:"private"`
			AutoInit bool   `json:"auto_init"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		token, ok := f.tokens[bearer]
		if !ok {
			f.refuse(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		installation := f.installations[token.installation]
		switch {
		case installation.owner != "" || !strings.EqualFold(installation.account, segments[1]):
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
		case !body.Private || !body.AutoInit:
			f.t.Errorf("a repository was asked for with private %v and auto_init %v", body.Private, body.AutoInit)
		default:
			f.sequence++
			repo := &fakeRepo{id: int64(1000 + f.sequence), fullName: installation.account + "/" + body.Name, defaultBranch: "main", files: map[string]string{"README.md": "# " + body.Name + "\n"}, history: 1}
			f.repos[strings.ToLower(repo.fullName)] = repo
			installation.repos[repo.id] = true
			f.answer(w, http.StatusCreated, map[string]any{"id": repo.id, "full_name": repo.fullName, "private": true, "default_branch": repo.defaultBranch})
		}
	case len(segments) >= 5 && segments[0] == "repos" && segments[3] == "git":
		f.git(w, r, bearer, segments)
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
	case len(segments) == 4 && segments[0] == "repos" && segments[3] == "commits" && r.Method == http.MethodGet:
		repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
		if repo == nil {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if read, _ := f.visible(bearer, repo); !read {
			f.refuse(w, http.StatusNotFound, "Not Found")
			return
		}
		if repo.history == 0 {
			f.refuse(w, http.StatusConflict, "Git Repository is empty.")
			return
		}
		commits := []map[string]any{}
		for i := repo.history; i > 0 && len(commits) < 2; i-- {
			parents := []map[string]string{}
			if i > 1 {
				parents = append(parents, map[string]string{"sha": fmt.Sprintf("%040d", i-1)})
			}
			commits = append(commits, map[string]any{"sha": fmt.Sprintf("%040d", i), "parents": parents})
		}
		f.answer(w, http.StatusOK, commits)
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

// git answers the calls that write a commit without a clone. As GitHub does, it writes only for
// a token that may push, takes a tree whose files come with their content, and moves a branch
// off its history only when forced.
func (f *fakeGitHub) git(w http.ResponseWriter, r *http.Request, bearer string, segments []string) {
	repo := f.repos[strings.ToLower(segments[1]+"/"+segments[2])]
	if repo == nil {
		f.refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	if _, push := f.visible(bearer, repo); !push {
		f.refuse(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	if f.gitRefuses == segments[4] {
		f.refuse(w, http.StatusConflict, "Git Repository is empty.")
		return
	}
	var body struct {
		Tree     json.RawMessage `json:"tree"`
		BaseTree string          `json:"base_tree"`
		Parents  []string        `json:"parents"`
		Message  string          `json:"message"`
		SHA      string          `json:"sha"`
		Force    bool            `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
	sha := fmt.Sprintf("%040d", len(f.trees)+len(f.commits)+1)
	switch {
	case len(segments) == 5 && segments[4] == "trees" && r.Method == http.MethodPost:
		var entries []struct{ Path, Mode, Type, Content string }
		json.Unmarshal(body.Tree, &entries) //nolint:errcheck
		files := map[string]string{}
		for _, entry := range entries {
			if entry.Mode != "100644" || entry.Type != "blob" || entry.Path == "" || strings.HasPrefix(entry.Path, "/") {
				f.refuse(w, http.StatusUnprocessableEntity, "tree.path contains a malformed path component")
				return
			}
			files[entry.Path] = entry.Content
		}
		if len(files) == 0 || body.BaseTree != "" {
			f.t.Errorf("a tree was asked with %d files on base %q, want the files alone", len(files), body.BaseTree)
		}
		f.trees[sha] = files
		f.answer(w, http.StatusCreated, map[string]string{"sha": sha})
	case len(segments) == 5 && segments[4] == "commits" && r.Method == http.MethodPost:
		var tree string
		json.Unmarshal(body.Tree, &tree) //nolint:errcheck
		if f.trees[tree] == nil {
			f.refuse(w, http.StatusUnprocessableEntity, "Tree SHA does not exist")
			return
		}
		if body.Parents == nil || len(body.Parents) != 0 || body.Message == "" {
			f.t.Errorf("a commit was asked with parents %v and message %q, want no parent and a message", body.Parents, body.Message)
		}
		f.commits[sha] = tree
		f.answer(w, http.StatusCreated, map[string]string{"sha": sha})
	case len(segments) >= 7 && segments[4] == "refs" && segments[5] == "heads" && r.Method == http.MethodPatch:
		tree, known := f.commits[body.SHA]
		switch {
		case strings.Join(segments[6:], "/") != repo.defaultBranch:
			f.refuse(w, http.StatusUnprocessableEntity, "Reference does not exist")
		case !known:
			f.refuse(w, http.StatusUnprocessableEntity, "Object does not exist")
		case !body.Force:
			f.refuse(w, http.StatusUnprocessableEntity, "Update is not a fast forward")
		default:
			repo.files, repo.history = f.trees[tree], 1
			f.answer(w, http.StatusOK, map[string]any{"ref": "refs/heads/" + repo.defaultBranch})
		}
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

func TestUserInstallationsAreThoseThePersonReachesAndTheGrantIsTheCallersToKeepOrRevoke(t *testing.T) {
	f, client := newFakeGitHub(t)
	reader, err := client.UserInstallations(ctx, "code-reader", testRedirect)
	// reader is an admin of Acme, reaches rival as a collaborator on one repository, and has
	// account id 200, the number rival the organisation has too.
	want := []Installation{{ID: 100, AccountLogin: "Acme", AccountType: AccountOrganization, Controlled: true}, {ID: 200, AccountLogin: "rival", AccountType: AccountOrganization}}
	if err != nil || !slices.Equal(reader.Installations, want) {
		t.Fatalf("UserInstallations = %+v, %v, want %+v", reader, err, want)
	}
	// carol is a plain member of Acme and holds her own account.
	carol, err := client.UserInstallations(ctx, "code-carol", testRedirect)
	want = []Installation{{ID: 100, AccountLogin: "Acme", AccountType: AccountOrganization}, {ID: 300, AccountLogin: "carol", AccountType: AccountUser, Controlled: true}}
	if err != nil || !slices.Equal(carol.Installations, want) {
		t.Fatalf("UserInstallations = %+v, %v, want %+v", carol, err, want)
	}
	if len(f.revoked) != 0 || f.refreshTokens[refreshTokenOf(reader)] != "reader" || f.refreshTokens[refreshTokenOf(carol)] != "carol" || len(f.redirects) != 2 || f.redirects[0] != testRedirect {
		t.Fatalf("revoked %v, grants %+v and %+v, redirects %v: want each grant with its own refresh token, none revoked, and the redirect sent", f.revoked, reader, carol, f.redirects)
	}
	if err := client.RevokeUser(ctx, reader.AccessToken); err != nil || len(f.revoked) != 1 || len(f.userTokens) != 1 {
		t.Fatalf("RevokeUser = %v, revoked %v with %d user tokens left, want the one grant ended", err, f.revoked, len(f.userTokens))
	}
	if len(f.tokens) != 0 {
		t.Fatalf("an installation token was minted to answer who reaches what: %+v", f.tokens)
	}
	// A person who reaches none is answered with none, which is no failure.
	if found, err := client.UserInstallations(ctx, "code-mallory", testRedirect); err != nil || len(found.Installations) != 0 || found.Installations == nil {
		t.Fatalf("a person with no installation: %+v, %v, want an empty list", found, err)
	}
}

// An owner who installs the App on their organisation reaches the new installation, and GitHub
// shows a user access token their admin role there only once the installation has granted the App
// the organisation permission Members (read). Until then the person is told so, and is not taken
// for a collaborator who controls nothing.
func TestAnOrganisationsOwnerControlsItsInstallationOnceTheAppMayReadItsMembers(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.installations[168837676] = &fakeInstallation{account: "carols-lab", repos: map[int64]bool{}}
	f.reaches["carol"] = append(f.reaches["carol"], 168837676)
	f.roles["carol"][168837676] = "admin"
	lab := func() Installation {
		t.Helper()
		f.codes["code-carol"] = "carol"
		grant, err := client.UserInstallations(ctx, "code-carol", testRedirect)
		if err != nil || len(grant.Installations) != 3 || !grant.Installations[1].Controlled {
			t.Fatalf("UserInstallations = %+v, %v, want Acme, carol's own account controlled, and her organisation", grant, err)
		}
		return grant.Installations[2]
	}
	if got := lab(); got.Controlled || !got.MembershipHidden {
		t.Fatalf("an organisation whose installation hides its members = %+v, want its membership hidden", got)
	}
	f.installations[168837676].members = true
	if got := lab(); !got.Controlled || got.MembershipHidden {
		t.Fatalf("an organisation whose installation shows its members = %+v, want it controlled by its owner", got)
	}
	f.roles["carol"][168837676] = "member"
	if got := lab(); got.Controlled || got.MembershipHidden {
		t.Fatalf("an organisation the person is a plain member of = %+v, want it neither controlled nor hidden", got)
	}
}

func refreshTokenOf(grant *UserGrant) string {
	return strings.TrimPrefix(grant.Authorization, byRefreshToken)
}

// An App whose user tokens do not expire issues no refresh token: the grant is kept by the
// token itself, which acts as the person at every later use and is traded for nothing.
func TestAGrantOfAnAppWhoseUserTokensDoNotExpireIsKeptByTheTokenItself(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.unexpiring = true
	grant, err := client.UserInstallations(ctx, "code-carol", testRedirect)
	if err != nil || grant.Authorization != byAccessToken+grant.AccessToken || f.userTokens[grant.AccessToken] != "carol" {
		t.Fatalf("UserInstallations = %+v, %v, want a grant kept by carol's own token", grant, err)
	}
	asked := len(f.calls)
	for range 2 {
		access, next, err := client.UserToken(ctx, grant.Authorization)
		if err != nil || access != grant.AccessToken || next != grant.Authorization {
			t.Fatalf("UserToken = %q, %q, %v, want the same token and the same authorisation at each use", access, next, err)
		}
	}
	if len(f.calls) != asked {
		t.Fatalf("GitHub was asked %v to use a token that does not expire", f.calls[asked:])
	}
	f.installations[300].all = true
	if created, err := client.CreateRepository(ctx, 300, grant.AccessToken, "sweep", "", firstFiles); err != nil || created.FullName != "carol/sweep" {
		t.Fatalf("CreateRepository with the token kept = %+v, %v, want carol/sweep", created, err)
	}
	// Revoked on GitHub, it is an authorisation GitHub no longer takes.
	if err := client.RevokeUser(ctx, grant.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateRepository(ctx, 300, grant.AccessToken, "again", "", firstFiles); !errors.Is(err, ErrUserAuthorization) {
		t.Fatalf("CreateRepository with a revoked token = %v, want %v", err, ErrUserAuthorization)
	}
	// A token that expires and came with nothing to renew it keeps no grant.
	if _, err := (&userTokens{AccessToken: "ghu_x", ExpiresIn: 28800}).authorization(); err == nil {
		t.Fatal("a token that expires with no refresh token was taken for an authorisation")
	}
}

// A refresh token renews a person's authorisation once: GitHub replaces it with each use.
func TestAPersonsAuthorisationIsRenewedByItsRefreshTokenOnce(t *testing.T) {
	f, client := newFakeGitHub(t)
	grant, err := client.UserInstallations(ctx, "code-carol", testRedirect)
	if err != nil {
		t.Fatal(err)
	}
	access, next, err := client.UserToken(ctx, grant.Authorization)
	if err != nil || f.userTokens[access] != "carol" || next == grant.Authorization || f.refreshTokens[strings.TrimPrefix(next, byRefreshToken)] != "carol" {
		t.Fatalf("UserToken = %q, %q, %v, want carol's token and the refresh token that replaces the one used", access, next, err)
	}
	for name, spent := range map[string]string{"a refresh token used before": grant.Authorization, "none": "", "one GitHub never issued": byRefreshToken + "ghr_made_up"} {
		if access, next, err := client.UserToken(ctx, spent); !errors.Is(err, ErrUserAuthorization) || access != "" || next != "" {
			t.Errorf("%s: UserToken = %q, %q, %v, want %v", name, access, next, err, ErrUserAuthorization)
		}
	}
	if _, _, err := client.UserToken(ctx, "ghr_bare"); err == nil || errors.Is(err, ErrUserAuthorization) {
		t.Fatalf("an authorisation this client did not issue: UserToken = %v, want a failure of the platform's", err)
	}
	if _, _, err := client.UserToken(ctx, next); err != nil {
		t.Fatalf("the refresh token that replaced the first: %v", err)
	}
}

func TestUserInstallationsAreReadToTheLastPage(t *testing.T) {
	f, client := newFakeGitHub(t)
	for id := int64(1000); id < 1250; id++ {
		f.installations[id] = &fakeInstallation{account: fmt.Sprintf("org-%d", id)}
		f.reaches["alice"] = append(f.reaches["alice"], id)
	}
	found, err := client.UserInstallations(ctx, "code-alice", testRedirect)
	if err != nil || len(found.Installations) != 251 || found.Installations[250].ID != 1249 {
		t.Fatalf("UserInstallations = %+v, %v, want all 251 over three pages", found, err)
	}
}

func TestUserInstallationsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange        func(*fakeGitHub)
		code, redirect string
		want           error
	}{
		"a code github never issued":            {func(*fakeGitHub) {}, "code-made-up", testRedirect, ErrCodeRejected},
		"no code":                               {func(*fakeGitHub) {}, "", testRedirect, ErrCodeRejected},
		"a redirect the code was not issued to": {func(*fakeGitHub) {}, "code-alice", "https://evil.example/callback", ErrCodeRejected},
		"an installation of another App":        {func(f *fakeGitHub) { f.foreign = true }, "code-alice", testRedirect, nil},
		"github rate limits":                    {func(f *fakeGitHub) { f.rateLimit = http.StatusForbidden }, "code-alice", testRedirect, ErrRateLimited},
	} {
		t.Run(name, func(t *testing.T) {
			f, client := newFakeGitHub(t)
			tc.arrange(f)
			found, err := client.UserInstallations(ctx, tc.code, tc.redirect)
			if err == nil || found != nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("UserInstallations = %+v, %v, want %v", found, err, tc.want)
			}
			if f.rateLimit == 0 && len(f.userTokens) != 0 {
				t.Fatalf("the person's token outlived the refusal: %v", f.userTokens)
			}
		})
	}
}

func TestACodeWorksOnce(t *testing.T) {
	_, client := newFakeGitHub(t)
	if _, err := client.UserInstallations(ctx, "code-alice", testRedirect); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UserInstallations(ctx, "code-alice", testRedirect); !errors.Is(err, ErrCodeRejected) {
		t.Fatalf("the same code a second time: got = %v, want %v", err, ErrCodeRejected)
	}
}

// A person's token that cannot be revoked would outlive the one question it was issued for.
func TestARevocationGitHubRefusesIsAFailure(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotFound, http.StatusUnprocessableEntity} {
		f, client := newFakeGitHub(t)
		grant, err := client.UserInstallations(ctx, "code-alice", testRedirect)
		if err != nil {
			t.Fatal(err)
		}
		f.revokeStatus = status
		if err := client.RevokeUser(ctx, grant.AccessToken); err == nil {
			t.Errorf("revocation answered %d: RevokeUser = nil, want a failure", status)
		}
	}
}

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
	if _, err := client.Installation(ctx, 200); !errors.Is(err, ErrInstallationGone) {
		t.Fatalf("a suspended installation: got = %v, want %v", err, ErrInstallationGone)
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
	created, err := client.CreateRepository(ctx, 100, "", "lr-sweep-1a2b3c4d", "what it is for", firstFiles)
	if err != nil || *created != (Repository{FullName: want, Private: true}) {
		t.Fatalf("CreateRepository = %+v, %v, want %s", created, err, want)
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
		if _, err := client.CreateRepository(ctx, tc.installation, "", tc.repo, "", firstFiles); !errors.Is(err, tc.want) {
			t.Errorf("%s: got = %v, want %v", name, err, tc.want)
		}
		f.ungranted = false
	}
	if f.repos["acme/fresh"] != nil || f.repos["rival/fresh"] != nil {
		t.Fatal("a refused create left a repository behind")
	}
}

// carolsToken is a token that acts as carol, as a create on her own account is handed one.
func carolsToken(t *testing.T, client *Client) string {
	t.Helper()
	grant, err := client.UserInstallations(ctx, "code-carol", testRedirect)
	if err != nil {
		t.Fatal(err)
	}
	return grant.AccessToken
}

// GitHub creates on a person's own account "for the authenticated user", and an installation
// token is no user's: the person's own token creates there, and adds the repository to an
// installation that covers selected repositories only. The first commit is the installation's.
func TestARepositoryIsCreatedOnAPersonsOwnAccountWithTheirTokenAndCoveredByTheInstallation(t *testing.T) {
	for name, all := range map[string]bool{"selected repositories": false, "all repositories": true} {
		f, client := newFakeGitHub(t)
		f.installations[300].all = all
		created, err := client.CreateRepository(ctx, 300, carolsToken(t, client), "sweep", "what it is for", firstFiles)
		if err != nil || *created != (Repository{FullName: "carol/sweep", Private: true}) {
			t.Fatalf("%s: CreateRepository = %+v, %v, want carol/sweep", name, created, err)
		}
		repo := f.repos["carol/sweep"]
		if !f.installations[300].repos[repo.id] || !maps.Equal(repo.files, firstFiles) {
			t.Fatalf("%s: the installation covers it: %v, and it holds %v, want it covered with the files given", name, f.installations[300].repos[repo.id], repo.files)
		}
		added := slices.Contains(f.calls, fmt.Sprintf("PUT /user/installations/300/repositories/%d", repo.id))
		if added == all || slices.Contains(f.calls, "POST /orgs/carol/repos") {
			t.Fatalf("%s: calls %v, want the repository added to the installation only where it covers selected ones", name, f.calls)
		}
		if wrote := f.lastToken(); wrote.permissions["contents"] != "write" || len(wrote.repos) != 1 || wrote.repos[0] != repo.id {
			t.Fatalf("%s: the commit was written with %+v, want an installation token for the one repository", name, wrote)
		}
	}
}

func TestACreateOnAPersonsOwnAccountRefusals(t *testing.T) {
	notAdded := func(err error) bool {
		var notCovered *NotCoveredError
		return errors.As(err, &notCovered) && notCovered.Repository == "carol/sweep"
	}
	for name, tc := range map[string]struct {
		arrange func(*fakeGitHub, *string)
		check   func(error) bool
		left    bool
	}{
		"no token of the person":         {func(_ *fakeGitHub, token *string) { *token = "" }, func(err error) bool { return errors.Is(err, ErrUserAuthorization) }, false},
		"a token GitHub no longer takes": {func(f *fakeGitHub, token *string) { delete(f.userTokens, *token) }, func(err error) bool { return errors.Is(err, ErrUserAuthorization) }, false},
		"an installation that may not create": {func(f *fakeGitHub, _ *string) { f.installations[300].creates = false }, func(err error) bool {
			var notPermitted *CreationNotPermittedError
			return errors.As(err, &notPermitted) && notPermitted.Accepted == "administration=write; repository_creation=write"
		}, false},
		"a name the account has":                                {func(f *fakeGitHub, _ *string) { f.repos["carol/sweep"] = &fakeRepo{id: 77, fullName: "carol/sweep"} }, func(err error) bool { return errors.Is(err, ErrRepositoryExists) }, true},
		"an installation GitHub does not let the person add to": {func(f *fakeGitHub, _ *string) { f.addStatus = http.StatusForbidden }, notAdded, true},
		"an installation removed since":                         {func(f *fakeGitHub, _ *string) { f.addStatus = http.StatusNotFound }, notAdded, true},
	} {
		f, client := newFakeGitHub(t)
		token := carolsToken(t, client)
		tc.arrange(f, &token)
		created, err := client.CreateRepository(ctx, 300, token, "sweep", "", firstFiles)
		if created != nil || !tc.check(err) || (f.repos["carol/sweep"] != nil) != tc.left {
			t.Errorf("%s: CreateRepository = %+v, %v, repository left behind %v", name, created, err, f.repos["carol/sweep"] != nil)
		}
		if strings.Contains(fmt.Sprint(err), "ghu_") {
			t.Errorf("%s: the error holds the person's token: %v", name, err)
		}
	}
	// The token of somebody else creates on their account, which is not the installation's: it
	// is refused once GitHub names what it made.
	f, client := newFakeGitHub(t)
	f.installations[400] = &fakeInstallation{account: "alice", owner: "alice", creates: true, all: true, repos: map[int64]bool{}}
	grant, err := client.UserInstallations(ctx, "code-alice", testRedirect)
	if err != nil {
		t.Fatal(err)
	}
	var notAsAsked *NotAsAskedError
	if created, err := client.CreateRepository(ctx, 300, grant.AccessToken, "sweep", "", firstFiles); created != nil || !errors.As(err, &notAsAsked) || notAsAsked.Repository != "alice/sweep" || f.installations[300].repos[f.repos["alice/sweep"].id] {
		t.Fatalf("with another person's token: CreateRepository = %+v, %v, want a failure that names alice/sweep and nothing covered by carol's installation", created, err)
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
		if created, err := client.CreateRepository(ctx, 100, "", "fresh", "", firstFiles); created != nil || !tc.check(err) {
			t.Errorf("%s: CreateRepository = %+v, %v", name, created, err)
		}
	}
}

var firstFiles = map[string]string{"README.md": "what this is\n", ".hypothesisloop/job.yaml": "job:\n  image: one\n"}

// GitHub's own first commit is replaced: the default branch holds the files given and nothing
// else, written with a token for that one repository.
func TestACreatedRepositoryHoldsTheFilesGivenAsItsOnlyCommit(t *testing.T) {
	f, client := newFakeGitHub(t)
	created, err := client.CreateRepository(ctx, 100, "", "sweep", "", firstFiles)
	if err != nil {
		t.Fatal(err)
	}
	if held := f.repos["acme/sweep"].files; !maps.Equal(held, firstFiles) {
		t.Fatalf("the default branch holds %v, want %v", held, firstFiles)
	}
	if wrote := f.lastToken(); wrote.permissions["contents"] != "write" || len(wrote.repos) != 1 || f.repoByID(wrote.repos[0]).fullName != created.FullName {
		t.Fatalf("the commit was written with %+v, want a write token for %s alone", wrote, created.FullName)
	}
	token, err := client.Token(ctx, 100, created.FullName, Read)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range firstFiles {
		if data, err := client.File(ctx, token, "", path); err != nil || string(data) != want {
			t.Errorf("File(%s) = %q, %v, want %q", path, data, err, want)
		}
	}
	if _, err := client.CreateRepository(ctx, 100, "", "bare", "", nil); err == nil || f.repos["acme/bare"] != nil {
		t.Fatalf("a create with no files: got = %v, want it refused before anything is created", err)
	}
}

// The App creates repositories and deletes none, so a repository whose first commit is refused
// stays on the account with GitHub's own commit, and the error names it.
func TestACreateWhoseFirstCommitIsRefusedFailsAndNamesTheRepositoryLeftBehind(t *testing.T) {
	for step, arrange := range map[string]func(*fakeGitHub){
		"trees":   func(f *fakeGitHub) { f.gitRefuses = "trees" },
		"commits": func(f *fakeGitHub) { f.gitRefuses = "commits" },
		"refs":    func(f *fakeGitHub) { f.gitRefuses = "refs" },
	} {
		f, client := newFakeGitHub(t)
		arrange(f)
		created, err := client.CreateRepository(ctx, 100, "", "sweep", "", firstFiles)
		var uncommitted *FirstCommitError
		if created != nil || !errors.As(err, &uncommitted) || uncommitted.Repository != "Acme/sweep" || !strings.Contains(err.Error(), "/git/"+step) {
			t.Fatalf("%s refused: got = %+v, %v, want a FirstCommitError naming Acme/sweep and the call", step, created, err)
		}
		left := f.repos["acme/sweep"]
		if left == nil || left.files[".hypothesisloop/job.yaml"] != "" || left.files["README.md"] == firstFiles["README.md"] {
			t.Fatalf("%s refused: the repository left behind holds %+v, want GitHub's own first commit", step, left)
		}
	}
}

// A create cut off before its first commit leaves GitHub's commit alone on the default branch.
// StartFresh writes the files there, and into a repository with commits of its own nothing.
func TestStartFreshWritesTheFilesOnlyIntoARepositoryHoldingGitHubsFirstCommit(t *testing.T) {
	f, client := newFakeGitHub(t)
	f.gitRefuses = "trees"
	if _, err := client.CreateRepository(ctx, 100, "", "sweep", "", firstFiles); err == nil {
		t.Fatal("the first commit was not refused")
	}
	f.gitRefuses = ""
	token, err := client.Token(ctx, 100, "Acme/sweep", Write)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartFresh(ctx, token, firstFiles); err != nil || !maps.Equal(f.repos["acme/sweep"].files, firstFiles) {
		t.Fatalf("StartFresh on GitHub's first commit alone: %v, holds %v, want %v", err, f.repos["acme/sweep"].files, firstFiles)
	}
	f.repos["acme/sweep"].history = 2
	if err := client.StartFresh(ctx, token, map[string]string{"README.md": "x\n"}); !errors.Is(err, ErrNotFresh) || !maps.Equal(f.repos["acme/sweep"].files, firstFiles) {
		t.Fatalf("StartFresh on a repository with commits of its own: %v, want ErrNotFresh and nothing written", err)
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
		"StartFresh":   func() error { return client.StartFresh(ctx, token, firstFiles) },
		"Installation": func() error { _, err := client.Installation(ctx, 100); return err },
		"Repositories": func() error { _, err := client.Repositories(ctx, 100); return err },
		"Token":        func() error { _, err := client.Token(ctx, 100, "acme/model", Write); return err },
		"CreateRepository": func() error {
			_, err := client.CreateRepository(ctx, 100, "", "fresh", "", firstFiles)
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
			_, err = client.UserInstallations(ctx, "code-alice", testRedirect)
			failures = append(failures, err)
			_, err = client.Installation(ctx, 100)
			failures = append(failures, err)
			_, err = client.Repositories(ctx, 100)
			failures = append(failures, err)
			_, err = client.Token(ctx, 100, "acme/model", Write)
			failures = append(failures, err)
			_, err = client.File(ctx, readToken, "main", ".hypothesisloop/experiment.yaml")
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
	if _, err := client.Token(ctx, 100, "acme/model", Write); !errors.Is(err, ErrPermissionNotGranted) {
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
