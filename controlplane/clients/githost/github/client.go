package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
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
	// ErrInstallationSuspended is ErrInstallationGone for an installation its owner suspended.
	ErrInstallationSuspended = fmt.Errorf("the GitHub App installation was suspended: %w", ErrInstallationGone)
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
	// ErrPermissionNotGranted reports an installation that has not granted the access the App
	// asks for: its owner has yet to accept the App's permissions.
	ErrPermissionNotGranted = errors.New("the installation has not granted the App the access it asks for")
	// ErrInvalidRepository reports a name that is not owner/name.
	ErrInvalidRepository = errors.New("a repository is named owner/name")
	// ErrCannotCreateRepositories reports an installation that does not let the App create a
	// repository on its account: it has not granted the permission, or the account forbids it.
	// CreationNotPermittedError carries it.
	ErrCannotCreateRepositories = errors.New("the installation does not let the GitHub App create repositories")
	// ErrRepositoryExists reports that the account already holds a repository of that name.
	ErrRepositoryExists = errors.New("the GitHub account already has a repository of that name")
	// ErrNotFresh reports a repository that holds more than the one commit GitHub makes for a
	// repository it creates.
	ErrNotFresh = errors.New("the repository holds commits beyond the one GitHub made at its creation")
)

// NotCoveredError reports a repository that was created and that the installation, which covers
// selected repositories only, does not cover. It is left on the account for its owner to add to
// the installation.
type NotCoveredError struct {
	Repository string
	Cause      error
}

func (e *NotCoveredError) Error() string {
	return "github: " + e.Repository + " was created and not added to the installation: " + e.Cause.Error()
}

func (e *NotCoveredError) Unwrap() error { return e.Cause }

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

// NotAsAskedError reports a repository GitHub created that is not the private one asked for on
// the installation's account: it is public, or on another account, as when the person's token is
// not the account owner's. It is left where GitHub made it.
type NotAsAskedError struct {
	Repository string
}

func (e *NotAsAskedError) Error() string {
	return "github: " + e.Repository + " was created and is not the private repository asked for"
}

// CreationNotPermittedError is GitHub refusing the installation the creation of a repository.
// Accepted is what GitHub says it takes for the call, one permission of several, as in
// "administration=write; repository_creation=write", and empty when it did not say.
type CreationNotPermittedError struct {
	Accepted string
}

func (e *CreationNotPermittedError) Error() string {
	return "github: " + ErrCannotCreateRepositories.Error() + " (GitHub accepts " + strconv.Quote(e.Accepted) + ")"
}

func (e *CreationNotPermittedError) Unwrap() error { return ErrCannotCreateRepositories }

// CreationFailedError is a create GitHub refused for a reason the platform has no name for,
// with the status and the message GitHub answered.
type CreationFailedError struct {
	Status  int
	Message string
}

func (e *CreationFailedError) Error() string {
	return fmt.Sprintf("github: the repository was not created: GitHub answered %d: %s", e.Status, e.Message)
}

// FailedError is GitHub failing a call the platform made for an installation: it answered 5xx,
// with Status and its Message, often empty, or did not answer, with Status 0.
type FailedError struct {
	Installation int64
	Status       int
	Message      string
	Err          error
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("github: installation %d: %v", e.Installation, e.Err)
}

func (e *FailedError) Unwrap() error { return e.Err }

// failedFor names the installation a call GitHub failed was made for.
func failedFor(installationID int64, err error) error {
	if !errors.Is(err, errFailed) {
		return err
	}
	failed := &FailedError{Installation: installationID, Err: err}
	var refused *rejection
	if errors.As(err, &refused) {
		failed.Status, failed.Message = refused.status, refused.message
	}
	return failed
}

//go:embed app-manifest.json
var manifest []byte

// Permissions is every permission the App asks an installation for, by name, as
// app-manifest.json, the one statement of them, declares: administration (write) creates a
// repository, contents (write) reads, commits and pushes, metadata (read) lists repositories.
var Permissions = func() map[string]string {
	var app struct {
		Permissions map[string]string `json:"default_permissions"`
	}
	if err := json.Unmarshal(manifest, &app); err != nil || len(app.Permissions) == 0 {
		panic("github: app-manifest.json states no default_permissions")
	}
	return app.Permissions
}()

// missingPermissions is what of Permissions an installation has not granted, each as name:level.
func missingPermissions(granted map[string]string) []string {
	missing := []string{}
	for _, name := range slices.Sorted(maps.Keys(Permissions)) {
		if permissionLevels[granted[name]] < permissionLevels[Permissions[name]] {
			missing = append(missing, name+":"+Permissions[name])
		}
	}
	return missing
}

// Access is what an installation token may do to the repository's contents.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// Token is an installation token. It works for about an hour and on one repository, which
// Repository names as owner/name the way GitHub does at the moment of minting.
type Token struct {
	Value        string
	ExpiresAt    time.Time
	Repository   string
	installation int64
}

// App is what a browser needs to send a person to GitHub.
type App struct {
	Slug       string `json:"slug"`
	InstallURL string `json:"install_url" doc:"Where a person installs the App on a GitHub account and chooses its repositories. Connecting goes through POST /orgs/{id}/github-install-states, whose install_url carries the state GitHub hands back."`
	// NewRepositoryURL is GitHub's own form for a new repository.
	NewRepositoryURL string `json:"new_repository_url" doc:"Where a person creates a repository on GitHub themselves. owner and visibility may be added as query parameters."`
}

// Client is the only code that talks to GitHub. It acts as the platform's GitHub App and keeps
// no token: each is minted for the call that needs it and handed on or dropped.
type Client struct {
	appID  int64
	slug   string
	key    *rsa.PrivateKey
	apiURL string
	webURL string
	http   *http.Client
	now    func() time.Time
	// mintRetries is the wait before each retry of a mint GitHub answered 5xx.
	mintRetries []time.Duration
}

// New builds the client of the configured App. The private key is the PEM GitHub issued.
func New(cfg config.GitHubConfig, privateKey string) (*Client, error) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return nil, errors.New("github: the App's private key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: the App's private key is not the RSA key GitHub issues for an App (PKCS#1 PEM)")
	}
	return &Client{
		appID: cfg.AppID, slug: cfg.AppSlug, key: key,
		apiURL: apiURL, webURL: webURL, http: &http.Client{Timeout: 20 * time.Second}, now: time.Now,
		mintRetries: []time.Duration{time.Second, 3 * time.Second},
	}, nil
}

func (c *Client) App() App {
	return App{
		Slug:             c.slug,
		InstallURL:       c.webURL + "/apps/" + url.PathEscape(c.slug) + "/installations/new",
		NewRepositoryURL: c.webURL + "/new",
	}
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
	return &Token{Value: out.Token, ExpiresAt: out.ExpiresAt, Repository: out.Repositories[0].FullName, installation: installationID}, nil
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
	var refused *rejection
	err = c.call(ctx, http.MethodPost, path, jwt, body, &out)
	// GitHub has answered a mint 500 once among many that worked, and the git command an agent
	// ran failed with it: a mint changes nothing, so one GitHub failed is asked again.
	for _, wait := range c.mintRetries {
		if !errors.As(err, &refused) || refused.status < 500 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, failedFor(installationID, err)
		case <-time.After(wait):
		}
		err = c.call(ctx, http.MethodPost, path, jwt, body, &out)
	}
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, errForbidden):
		return nil, ErrInstallationGone
	// GitHub answers 422 both for a repository outside the installation and for a permission the
	// installation has not granted, and only its message tells them apart.
	case errors.Is(err, errUnprocessable) && errors.As(err, &refused) && strings.Contains(strings.ToLower(refused.message), "permission"):
		asked, _ := body["permissions"].(map[string]string)
		return nil, c.permissionsMissing(ctx, installationID, asked)
	case errors.Is(err, errUnprocessable):
		return nil, ErrRepositoryNotInstalled
	case errors.Is(err, errUnauthorized):
		return nil, ErrAppCredentials
	case err != nil:
		return nil, failedFor(installationID, err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("github: POST %s answered without a token or its expiry", routeOf(path))
	}
	return &out, nil
}

// PermissionMissingError names the permissions a token was asked with that the installation has
// not granted, each as name:level.
type PermissionMissingError struct {
	Installation int64
	Missing      []string
}

func (e *PermissionMissingError) Error() string {
	return fmt.Sprintf("github: installation %d has not granted the App %s", e.Installation, strings.Join(e.Missing, ", "))
}

func (e *PermissionMissingError) Unwrap() error { return ErrPermissionNotGranted }

var permissionLevels = map[string]int{"read": 1, "write": 2, "admin": 3}

// permissionsMissing asks GitHub, as the App, which of asked an installation has not granted, after
// GitHub refused a token for a permission without saying which. ErrPermissionNotGranted when it
// cannot tell.
func (c *Client) permissionsMissing(ctx context.Context, installationID int64, asked map[string]string) error {
	jwt, err := c.appJWT()
	if err != nil {
		return ErrPermissionNotGranted
	}
	var out struct {
		Permissions map[string]string `json:"permissions"`
	}
	if c.call(ctx, http.MethodGet, "/app/installations/"+strconv.FormatInt(installationID, 10), jwt, nil, &out) != nil {
		return ErrPermissionNotGranted
	}
	missing := []string{}
	for _, name := range slices.Sorted(maps.Keys(asked)) {
		if permissionLevels[out.Permissions[name]] < permissionLevels[asked[name]] {
			missing = append(missing, name+":"+asked[name])
		}
	}
	if len(missing) == 0 {
		return ErrPermissionNotGranted
	}
	return &PermissionMissingError{Installation: installationID, Missing: missing}
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
	// errFailed is GitHub answering 5xx or not answering at all.
	errFailed = errors.New("github failed the call")
)

// rejection is an answer of GitHub that is not a success: one of the kinds above, or nil for any
// other status, with GitHub's own message. accepted is the permissions GitHub says the call
// takes, and nameExists that it refused a name its account already holds.
type rejection struct {
	kind       error
	status     int
	message    string
	accepted   string
	nameExists bool
	detail     string
}

func (r *rejection) Error() string {
	if r.kind == nil {
		return r.detail
	}
	return r.kind.Error() + " (" + r.detail + ")"
}

func (r *rejection) Unwrap() error { return r.kind }

// call makes one API request with a bearer token.
func (c *Client) call(ctx context.Context, method, path, bearer string, body, out any) error {
	return c.request(ctx, method, path, "Bearer "+bearer, maxResponseBytes, body, out)
}

// request makes one API request and decodes a 2xx answer into out, when out is set. No error it
// returns holds the authorization or anything GitHub sent besides its status, its message and
// the permissions it says the call takes.
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
		return fmt.Errorf("%w (github: %s %s did not complete: %w)", errFailed, method, routeOf(path), transportError(err))
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
		Errors  []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(data, &answer)
	refused := &rejection{
		status:   resp.StatusCode,
		message:  bounded(answer.Message),
		accepted: bounded(resp.Header.Get("X-Accepted-GitHub-Permissions")),
		detail:   fmt.Sprintf("github: %s %s answered %d: %s", method, routeOf(path), resp.StatusCode, bounded(answer.Message)),
	}
	for _, one := range answer.Errors {
		if one.Field == "name" && strings.Contains(one.Message, "already exists") {
			refused.nameExists = true
		}
	}
	switch {
	case rateLimited(resp, answer.Message):
		refused.kind = ErrRateLimited
	case resp.StatusCode == http.StatusNotFound:
		refused.kind = errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		refused.kind = errUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		refused.kind = errForbidden
	case resp.StatusCode == http.StatusUnprocessableEntity:
		refused.kind = errUnprocessable
	case resp.StatusCode >= 500:
		refused.kind = errFailed
	}
	return refused
}

// rateLimited tells GitHub's two rate limits from an ordinary refusal: the primary one answers
// 403 or 429 with no calls remaining, the secondary one with a Retry-After or, GitHub documents,
// with neither and only its message to say so.
func rateLimited(resp *http.Response, message string) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return resp.StatusCode == http.StatusForbidden &&
		(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(message), "rate limit"))
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
