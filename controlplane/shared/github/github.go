package github

import (
	"bytes"
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
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

const (
	apiURL = "https://api.github.com"
	webURL = "https://github.com"

	// GitHub refuses an App JWT that lives longer than ten minutes, and one issued in its future.
	// The minute behind covers a clock that runs ahead of GitHub's.
	jwtBackdate = time.Minute
	jwtLifetime = 9 * time.Minute

	maxResponseBytes = 1 << 20
	// MaxFileBytes bounds a file read from a repository.
	MaxFileBytes = 1 << 20
	// A file travels as base64 in lines of sixty characters inside a JSON object that also
	// carries its addresses, so the answer holding a file of MaxFileBytes is this much larger.
	maxFileResponseBytes = MaxFileBytes*4/3 + MaxFileBytes/40 + 64<<10
)

var (
	// ErrAppCredentials reports that GitHub refused the App itself: a wrong id, a private key
	// that was rotated out, or a clock far enough off that the JWT reads as expired.
	ErrAppCredentials = errors.New("github refused the App's own credentials")
	// ErrInstallationGone reports an installation that was removed or suspended by its owner.
	ErrInstallationGone = errors.New("the GitHub App installation was removed or suspended")
	// ErrRepositoryNotInstalled reports a repository the installation does not cover: it was
	// never selected, was taken out of the selection, or does not exist.
	ErrRepositoryNotInstalled = errors.New("the GitHub App is not installed on that repository")
	// ErrRateLimited reports that GitHub is refusing calls for now.
	ErrRateLimited = errors.New("github is rate limiting the platform")
	// ErrFileNotFound reports a path the repository does not hold at the ref asked for.
	ErrFileNotFound = errors.New("the repository has no such file")
	// ErrNotAFile reports a path that holds a directory, a symlink or a submodule.
	ErrNotAFile = errors.New("the path in the repository is not a file")
	// ErrFileTooLarge reports a file over MaxFileBytes.
	ErrFileTooLarge = errors.New("the file is larger than the platform reads from a repository")
	// ErrCodeRejected reports a user authorisation code GitHub did not accept: used, expired or
	// made up.
	ErrCodeRejected = errors.New("github did not accept the authorisation code")
	// ErrPermissionNotGranted reports an installation that has not granted the access the App
	// asks for: its owner has yet to accept the App's permissions.
	ErrPermissionNotGranted = errors.New("the installation has not granted the App the access it asks for")
	// ErrInvalidRepository reports a name that is not owner/name.
	ErrInvalidRepository = errors.New("a repository is named owner/name")
	// ErrCannotCreateRepositories reports an installation that does not let the App create a
	// repository on its account: it has not granted the permission, or the account forbids it.
	ErrCannotCreateRepositories = errors.New("the installation does not let the GitHub App create repositories")
	// ErrRepositoryExists reports that the account already holds a repository of that name.
	ErrRepositoryExists = errors.New("the GitHub account already has a repository of that name")
)

// FirstCommitError reports a repository that was created and did not take its first commit. It
// is left on the account holding what GitHub starts one with: the App creates repositories and
// deletes none.
type FirstCommitError struct {
	Repository string
	Cause      error
}

func (e *FirstCommitError) Error() string {
	return "github: " + e.Repository + " was created and did not take its first commit: " + e.Cause.Error()
}

func (e *FirstCommitError) Unwrap() error { return e.Cause }

// Access is what an installation token may do to the repository's contents.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Installation is one installation of the App: a GitHub account, with the repositories its
// owner chose there.
type Installation struct {
	ID           int64  `json:"installation_id"`
	AccountLogin string `json:"account_login" doc:"The GitHub user or organisation the App is installed on. It is the owner part of every repository the installation covers."`
	// Controlled says the person asked about controls that account: it is their own, or an
	// organisation they are an admin of. Set by UserInstallations alone.
	Controlled bool `json:"-"`
}

// Repository is a repository an installation covers, named owner/name in GitHub's own casing.
type Repository struct {
	FullName string
	Private  bool
}

// Token is an installation token. It works for about an hour and on one repository, which
// Repository names as owner/name the way GitHub does at the moment of minting.
type Token struct {
	Value      string
	ExpiresAt  time.Time
	Repository string
}

// App is what a browser needs to send a person to GitHub.
type App struct {
	Slug         string `json:"slug"`
	ClientID     string `json:"client_id"`
	InstallURL   string `json:"install_url" doc:"Where a person installs the App on a GitHub account and chooses its repositories. GitHub sends them back with installation_id."`
	AuthorizeURL string `json:"authorize_url" doc:"Where a person proves who they are on GitHub. Add redirect_uri and state. GitHub sends them back with code."`
}

// Client is the only code that talks to GitHub. It acts as the platform's GitHub App and keeps
// no token: each is minted for the call that needs it and handed on or dropped.
type Client struct {
	appID        int64
	slug         string
	clientID     string
	clientSecret string
	key          *rsa.PrivateKey
	apiURL       string
	webURL       string
	http         *http.Client
	now          func() time.Time
}

// New builds the client of the configured App. The private key is the PEM GitHub issued.
func New(cfg config.GitHubConfig) (*Client, error) {
	block, _ := pem.Decode([]byte(cfg.PrivateKey))
	if block == nil {
		return nil, errors.New("github: private_key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: private_key is not the RSA key GitHub issues for an App (PKCS#1 PEM)")
	}
	return &Client{
		appID: cfg.AppID, slug: cfg.AppSlug, clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, key: key,
		apiURL: apiURL, webURL: webURL, http: &http.Client{Timeout: 20 * time.Second}, now: time.Now,
	}, nil
}

func (c *Client) App() App {
	return App{
		Slug: c.slug, ClientID: c.clientID,
		InstallURL:   c.webURL + "/apps/" + url.PathEscape(c.slug) + "/installations/new",
		AuthorizeURL: c.webURL + "/login/oauth/authorize?client_id=" + url.QueryEscape(c.clientID),
	}
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// ValidRepository refuses a name that is not owner/name. The name becomes part of a URL, so this
// is also what keeps a caller from steering a request elsewhere.
func ValidRepository(fullName string) error {
	_, name, _ := strings.Cut(fullName, "/")
	if !repositoryName.MatchString(fullName) || name == "." || name == ".." || len(fullName) > 200 {
		return ErrInvalidRepository
	}
	return nil
}

// Host is the host git asks credentials for when it clones a repository.
const Host = "github.com"

// CloneURL is the HTTPS address git clones the repository from.
func CloneURL(fullName string) string {
	return webURL + "/" + fullName + ".git"
}

// UserInstallations returns the installations of the App that the person behind userCode
// reaches on GitHub, each marked with whether they control the account it is on. Reaching one
// takes no more than being a collaborator on a single repository it covers, so only Controlled
// says who may connect it. redirectURI is the one the code was issued to. The person's token
// answers this one question and is revoked before anything is returned.
func (c *Client) UserInstallations(ctx context.Context, userCode, redirectURI string) ([]Installation, error) {
	userToken, err := c.exchangeCode(ctx, userCode, redirectURI)
	if err != nil {
		return nil, err
	}
	found, err := c.installationsOf(ctx, userToken)
	if revokeErr := c.revoke(ctx, userToken); revokeErr != nil && err == nil {
		err = revokeErr
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

const pageSize = 100

func (c *Client) installationsOf(ctx context.Context, userToken string) ([]Installation, error) {
	controlled, err := c.controlledAccounts(ctx, userToken)
	if err != nil {
		return nil, err
	}
	found := []Installation{}
	for page := 1; ; page++ {
		var out struct {
			TotalCount    int `json:"total_count"`
			Installations []struct {
				ID      int64 `json:"id"`
				AppID   int64 `json:"app_id"`
				Account struct {
					ID    int64  `json:"id"`
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"account"`
			} `json:"installations"`
		}
		path := fmt.Sprintf("/user/installations?per_page=%d&page=%d", pageSize, page)
		switch err := c.call(ctx, http.MethodGet, path, userToken, nil, &out); {
		case errors.Is(err, errUnauthorized):
			return nil, ErrCodeRejected
		case err != nil:
			return nil, err
		}
		for _, one := range out.Installations {
			// A token of this App lists this App's installations only, and it is checked all the
			// same: an installation of another App must never be connected.
			if one.AppID != c.appID || one.ID <= 0 || one.Account.Login == "" || one.Account.ID <= 0 || one.Account.Type == "" {
				return nil, fmt.Errorf("github: GET /user/installations answered an installation that is not this App's or has no account")
			}
			found = append(found, Installation{ID: one.ID, AccountLogin: one.Account.Login, Controlled: controlled[account{one.Account.Type, one.Account.ID}]})
		}
		if len(out.Installations) < pageSize || len(found) >= out.TotalCount {
			return found, nil
		}
	}
}

// account is a GitHub account by its type, User or Organization, and its id, which unlike its
// login is never another account's later.
type account struct {
	kind string
	id   int64
}

// controlledAccounts returns the accounts the person behind userToken controls: their own, and
// every organisation they are an active admin of. Both are what GitHub says of the token's
// owner and need no permission of the App.
func (c *Client) controlledAccounts(ctx context.Context, userToken string) (map[account]bool, error) {
	var self struct {
		ID int64 `json:"id"`
	}
	switch err := c.call(ctx, http.MethodGet, "/user", userToken, nil, &self); {
	case errors.Is(err, errUnauthorized):
		return nil, ErrCodeRejected
	case err != nil:
		return nil, err
	}
	if self.ID <= 0 {
		return nil, fmt.Errorf("github: GET /user answered without the person's id")
	}
	controlled := map[account]bool{{"User", self.ID}: true}
	for page := 1; ; page++ {
		var memberships []struct {
			State        string `json:"state"`
			Role         string `json:"role"`
			Organization struct {
				ID int64 `json:"id"`
			} `json:"organization"`
		}
		path := fmt.Sprintf("/user/memberships/orgs?state=active&per_page=%d&page=%d", pageSize, page)
		if err := c.call(ctx, http.MethodGet, path, userToken, nil, &memberships); err != nil {
			return nil, err
		}
		for _, one := range memberships {
			if one.State == "active" && one.Role == "admin" && one.Organization.ID > 0 {
				controlled[account{"Organization", one.Organization.ID}] = true
			}
		}
		if len(memberships) < pageSize {
			return controlled, nil
		}
	}
}

// Installation returns one installation of the App as GitHub states it to the App itself.
func (c *Client) Installation(ctx context.Context, installationID int64) (*Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var out struct {
		ID          int64   `json:"id"`
		SuspendedAt *string `json:"suspended_at"`
		Account     struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10)
	switch err := c.call(ctx, http.MethodGet, path, jwt, nil, &out); {
	case errors.Is(err, errNotFound):
		return nil, ErrInstallationGone
	case errors.Is(err, errUnauthorized):
		return nil, ErrAppCredentials
	case err != nil:
		return nil, err
	}
	if out.SuspendedAt != nil {
		return nil, ErrInstallationGone
	}
	if out.ID != installationID || out.Account.Login == "" {
		return nil, fmt.Errorf("github: GET /app/installations/{id} answered without the installation asked for or its account")
	}
	return &Installation{ID: out.ID, AccountLogin: out.Account.Login}, nil
}

// Repositories lists every repository an installation covers, as GitHub pages them. The token
// it reads with reaches the installation's metadata and nothing else, and does not leave here.
func (c *Client) Repositories(ctx context.Context, installationID int64) ([]Repository, error) {
	token, err := c.mint(ctx, installationID, map[string]any{"permissions": map[string]string{"metadata": "read"}})
	if err != nil {
		return nil, err
	}
	found := []Repository{}
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", pageSize, page)
		if err := c.call(ctx, http.MethodGet, path, token.Token, nil, &out); err != nil {
			return nil, err
		}
		for _, one := range out.Repositories {
			if ValidRepository(one.FullName) != nil {
				return nil, fmt.Errorf("github: GET /installation/repositories answered a repository without its name")
			}
			found = append(found, Repository{FullName: one.FullName, Private: one.Private})
		}
		if len(out.Repositories) < pageSize || len(found) >= out.TotalCount {
			return found, nil
		}
	}
}

var newRepositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// CreateRepository creates a private repository of that name on the account an installation is
// on, an organisation or a person's own, whose default branch holds one commit: files, by path.
// GitHub then counts it among the repositories the installation covers, whichever ones its
// owner selected. The token it creates with holds everything the installation granted and does
// not leave here.
func (c *Client) CreateRepository(ctx context.Context, installationID int64, name, description string, files map[string]string) (*Repository, error) {
	if !newRepositoryName.MatchString(name) || name == "." || name == ".." {
		return nil, ErrInvalidRepository
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("github: a repository is created with the files of its first commit")
	}
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var installation struct {
		SuspendedAt *string `json:"suspended_at"`
		Account     struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	switch err := c.call(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(installationID, 10), jwt, nil, &installation); {
	case errors.Is(err, errNotFound):
		return nil, ErrInstallationGone
	case errors.Is(err, errUnauthorized):
		return nil, ErrAppCredentials
	case err != nil:
		return nil, err
	}
	if installation.SuspendedAt != nil {
		return nil, ErrInstallationGone
	}
	owner := installation.Account.Login
	var path string
	switch installation.Account.Type {
	case "Organization":
		path = "/orgs/" + url.PathEscape(owner) + "/repos"
	case "User":
		path = "/user/repos"
	}
	if owner == "" || path == "" {
		return nil, fmt.Errorf("github: GET /app/installations/{id} answered without the installation's account or its type")
	}
	// GitHub documents no name by which a token is narrowed to creating repositories, so the
	// token is asked for whole.
	token, err := c.mint(ctx, installationID, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	// GitHub writes no tree to a repository with no commit, so it makes the first one itself
	// and firstCommit takes its place.
	body := map[string]any{"name": name, "description": description, "private": true, "auto_init": true}
	switch err := c.call(ctx, http.MethodPost, path, token.Token, body, &out); {
	case errors.Is(err, errUnprocessable):
		return nil, ErrRepositoryExists
	case errors.Is(err, errForbidden), errors.Is(err, errNotFound):
		return nil, ErrCannotCreateRepositories
	case err != nil:
		return nil, err
	}
	if !out.Private || !strings.EqualFold(out.FullName, owner+"/"+name) {
		return nil, fmt.Errorf("github: POST %s answered without the private repository asked for", path)
	}
	write, err := c.Token(ctx, installationID, out.FullName, Write)
	if err != nil {
		return nil, &FirstCommitError{Repository: out.FullName, Cause: err}
	}
	if err := c.firstCommit(ctx, write, out.DefaultBranch, files); err != nil {
		return nil, &FirstCommitError{Repository: out.FullName, Cause: err}
	}
	return &Repository{FullName: out.FullName, Private: true}, nil
}

// firstCommit makes files the only commit of a branch the token's repository has: a tree, a
// commit of it with no parent, and the branch moved there whatever it held.
func (c *Client) firstCommit(ctx context.Context, token *Token, branch string, files map[string]string) error {
	if branch == "" {
		return fmt.Errorf("github: %s names no default branch", token.Repository)
	}
	entries := make([]map[string]string, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		entries = append(entries, map[string]string{"path": path, "mode": "100644", "type": "blob", "content": files[path]})
	}
	git := "/repos/" + token.Repository + "/git/"
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.call(ctx, http.MethodPost, git+"trees", token.Value, map[string]any{"tree": entries}, &tree); err != nil {
		return err
	}
	made := map[string]any{"message": "Start from the HypothesisLoop experiment template", "tree": tree.SHA, "parents": []string{}}
	if err := c.call(ctx, http.MethodPost, git+"commits", token.Value, made, &commit); err != nil {
		return err
	}
	if tree.SHA == "" || commit.SHA == "" {
		return fmt.Errorf("github: %s answered a tree or a commit without its sha", token.Repository)
	}
	return c.call(ctx, http.MethodPatch, git+"refs/heads/"+branch, token.Value, map[string]any{"sha": commit.SHA, "force": true}, nil)
}

// revoke ends a person's token at GitHub, so that nothing of theirs outlives the question it
// was issued to answer.
func (c *Client) revoke(ctx context.Context, userToken string) error {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.clientID+":"+c.clientSecret))
	path := "/applications/" + url.PathEscape(c.clientID) + "/token"
	err := c.request(ctx, http.MethodDelete, path, basic, maxResponseBytes, map[string]string{"access_token": userToken}, nil)
	if errors.Is(err, errUnauthorized) {
		return ErrAppCredentials
	}
	return err
}

// Token mints an installation token for one repository, named owner/name, with the access asked
// for and nothing else. Two platform experiments on one installation each get a token that
// reaches only their own repository.
func (c *Client) Token(ctx context.Context, installationID int64, fullName string, access Access) (*Token, error) {
	if access != Read && access != Write {
		return nil, fmt.Errorf("github: unknown access %q", access)
	}
	if err := ValidRepository(fullName); err != nil {
		return nil, err
	}
	_, name, _ := strings.Cut(fullName, "/")
	out, err := c.mint(ctx, installationID, map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": string(access), "metadata": "read"},
	})
	if err != nil {
		return nil, err
	}
	// The scope is what was asked for, and it is checked all the same: a token that reaches
	// further than one repository must never leave this package. An installation names its
	// repositories without their owner, so the owner is checked here too: the installation of
	// another account would answer for a repository of the same name.
	if len(out.Repositories) != 1 || !strings.EqualFold(out.Repositories[0].FullName, fullName) {
		if len(out.Repositories) == 1 {
			return nil, ErrRepositoryNotInstalled
		}
		return nil, fmt.Errorf("github: the token minted for installation %d is not limited to one repository", installationID)
	}
	return &Token{Value: out.Token, ExpiresAt: out.ExpiresAt, Repository: out.Repositories[0].FullName}, nil
}

type minted struct {
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

func (c *Client) mint(ctx context.Context, installationID int64, body map[string]any) (*minted, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var out minted
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	var refused *refusal
	switch err := c.call(ctx, http.MethodPost, path, jwt, body, &out); {
	case errors.Is(err, errNotFound), errors.Is(err, errForbidden):
		return nil, ErrInstallationGone
	// GitHub answers 422 both for a repository outside the installation and for a permission the
	// installation has not granted, and only its message tells them apart.
	case errors.Is(err, errUnprocessable) && errors.As(err, &refused) && strings.Contains(strings.ToLower(refused.message), "permission"):
		return nil, ErrPermissionNotGranted
	case errors.Is(err, errUnprocessable):
		return nil, ErrRepositoryNotInstalled
	case errors.Is(err, errUnauthorized):
		return nil, ErrAppCredentials
	case err != nil:
		return nil, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("github: POST %s answered without a token or its expiry", routeOf(path))
	}
	return &out, nil
}

// File reads one file of a repository with a token minted for it, at ref, a branch name or a
// commit. An empty ref is the head of the repository's default branch as it stands now.
func (c *Client) File(ctx context.Context, token *Token, ref, path string) ([]byte, error) {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, fmt.Errorf("github: %q is not a path inside a repository", path)
		}
		segments[i] = url.PathEscape(segment)
	}
	target := "/repos/" + token.Repository + "/contents/" + strings.Join(segments, "/")
	if ref != "" {
		target += "?ref=" + url.QueryEscape(ref)
	}
	var raw json.RawMessage
	switch err := c.request(ctx, http.MethodGet, target, "Bearer "+token.Value, maxFileResponseBytes, nil, &raw); {
	case errors.Is(err, errNotFound):
		return nil, ErrFileNotFound
	case err != nil:
		return nil, err
	}
	var file struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	// A directory is answered as a list of its entries, a symlink and a submodule under their
	// own type.
	if json.Unmarshal(raw, &file) != nil || file.Type != "file" {
		return nil, ErrNotAFile
	}
	if file.Size > MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("github: the contents of %s came back in encoding %q", path, file.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: the contents of %s are not base64", path)
	}
	return data, nil
}

// exchangeCode trades the code GitHub handed a person's browser for that person's token.
func (c *Client) exchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	if code == "" {
		return "", ErrCodeRejected
	}
	form := url.Values{"client_id": {c.clientID}, "client_secret": {c.clientSecret}, "code": {code}, "redirect_uri": {redirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("github: build the code exchange request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: the code exchange did not complete: %w", transportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", errors.New("github: the code exchange answer could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: the code exchange answered %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", errors.New("github: the code exchange answer is not JSON")
	}
	switch {
	case out.Error == "bad_verification_code", out.Error == "redirect_uri_mismatch":
		return "", ErrCodeRejected
	case out.Error == "incorrect_client_credentials":
		return "", ErrAppCredentials
	case out.Error != "":
		return "", fmt.Errorf("github: the code exchange was refused: %s", bounded(out.Error))
	case out.AccessToken == "":
		return "", errors.New("github: the code exchange answered without a token")
	}
	return out.AccessToken, nil
}

func (c *Client) appJWT() (string, error) {
	now := c.now()
	header := encoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(), "exp": now.Add(jwtLifetime).Unix(), "iss": strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return "", errors.New("github: encode the App JWT")
	}
	signed := header + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("github: sign the App JWT")
	}
	return signed + "." + encoding.EncodeToString(signature), nil
}

var encoding = base64.RawURLEncoding

// The statuses a caller reads differently depending on what it asked.
var (
	errNotFound      = errors.New("not found")
	errUnauthorized  = errors.New("unauthorized")
	errForbidden     = errors.New("forbidden")
	errUnprocessable = errors.New("unprocessable")
)

// refusal is an answer of GitHub that is not a success: one of the kinds above, or nil for any
// other status, with GitHub's own message.
type refusal struct {
	kind    error
	message string
	detail  string
}

func (r *refusal) Error() string {
	if r.kind == nil {
		return r.detail
	}
	return r.kind.Error() + " (" + r.detail + ")"
}

func (r *refusal) Unwrap() error { return r.kind }

// call makes one API request with a bearer token.
func (c *Client) call(ctx context.Context, method, path, bearer string, body, out any) error {
	return c.request(ctx, method, path, "Bearer "+bearer, maxResponseBytes, body, out)
}

// request makes one API request and decodes a 2xx answer into out, when out is set. No error it
// returns holds the authorization or anything GitHub sent besides its status and its message.
func (c *Client) request(ctx context.Context, method, path, authorization string, limit int, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: encode the body of %s %s", method, routeOf(path))
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return fmt.Errorf("github: build %s %s", method, routeOf(path))
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s did not complete: %w", method, routeOf(path), transportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return fmt.Errorf("github: %s %s: the answer could not be read", method, routeOf(path))
	}
	if len(data) > limit {
		return fmt.Errorf("github: %s %s: %w", method, routeOf(path), ErrFileTooLarge)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("github: %s %s answered %d with a body that is not the JSON expected", method, routeOf(path), resp.StatusCode)
		}
		return nil
	}
	var answer struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &answer)
	refused := &refusal{
		message: bounded(answer.Message),
		detail:  fmt.Sprintf("github: %s %s answered %d: %s", method, routeOf(path), resp.StatusCode, bounded(answer.Message)),
	}
	switch {
	case rateLimited(resp):
		refused.kind = ErrRateLimited
	case resp.StatusCode == http.StatusNotFound:
		refused.kind = errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		refused.kind = errUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		refused.kind = errForbidden
	case resp.StatusCode == http.StatusUnprocessableEntity:
		refused.kind = errUnprocessable
	}
	return refused
}

// rateLimited tells GitHub's two rate limits from an ordinary refusal: the primary one answers
// 403 or 429 with no calls remaining, the secondary one with a Retry-After.
func rateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return resp.StatusCode == http.StatusForbidden &&
		(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")
}

// routeOf drops the query of a path, so an error names where a call went and nothing it carried.
func routeOf(path string) string {
	route, _, _ := strings.Cut(path, "?")
	return route
}

// transportError keeps what failed about a connection and drops the URL *url.Error carries.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func bounded(message string) string {
	const limit = 200
	if len(message) > limit {
		return message[:limit]
	}
	return message
}
