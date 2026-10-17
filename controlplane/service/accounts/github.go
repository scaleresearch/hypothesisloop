package accounts

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost/github"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// GitHubApp is what the platform asks of its GitHub App. *github.Client satisfies it.
type GitHubApp interface {
	App() github.App
	Installation(ctx context.Context, installationID int64) (*github.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]github.Repository, error)
	Token(ctx context.Context, installationID int64, fullName string, access github.Access) (*github.Token, error)
	File(ctx context.Context, token *github.Token, ref, path string) ([]byte, error)
	CreateRepository(ctx context.Context, installationID int64, name, description string, files map[string]string) (*github.Repository, error)
	StartFresh(ctx context.Context, token *github.Token, files map[string]string) error
}

// InstallationStore is the persistence of the installations orgs connected and of the installs
// people started. *db.Store satisfies it.
type InstallationStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ConnectGitHubInstallation(ctx context.Context, installation *domain.OrgGitHubInstallation) error
	ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error)
	GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error)
	IssueGitHubInstallState(ctx context.Context, orgID string, actor domain.Actor, returnTo string, lifetime time.Duration) (string, time.Time, error)
	TakeGitHubInstallState(ctx context.Context, state string, actor domain.Actor) (*db.GitHubInstallState, error)
	DisconnectGitHubInstallation(ctx context.Context, orgID string, installationID int64) error
	MakeGitHubInstallationDefault(ctx context.Context, orgID string, installationID int64) error
}

// Repositories serves the GitHub installations orgs connect and what they cover.
type Repositories struct {
	store  InstallationStore
	github GitHubApp
	logger *zap.Logger
	access *auth.Access
}

// NewRepositories takes the App's client, or nil for a deployment whose git host is not GitHub.
func NewRepositories(store InstallationStore, app GitHubApp, access *auth.Access, logger *zap.Logger) *Repositories {
	if store == nil || access == nil || logger == nil {
		panic("accounts.NewRepositories: the store, an access check and a logger are required")
	}
	return &Repositories{store: store, github: app, logger: logger, access: access}
}

// Repository is a repository an org can name on a platform experiment.
type Repository struct {
	Repository     string `json:"repository" doc:"owner/name, as a platform experiment names it."`
	InstallationID int64  `json:"installation_id" doc:"The connected installation that covers it."`
	Private        bool   `json:"private"`
}

// The reasons a client tells a refusal of a connect or a credential apart by. The others are
// the git host's (github.Reason*).
const (
	ReasonGitHubNotConfigured = "github_not_configured"
	ReasonInvalidRequest      = "invalid_request"
	ReasonInstallStateInvalid = "install_state_invalid"
	ReasonInstallationNotNew  = "installation_not_new"
	ReasonForbidden           = "forbidden"
)

const (
	// installStateLifetime is how long a person has from pressing Install GitHub App to GitHub
	// handing the state back.
	installStateLifetime = 15 * time.Minute
	// clockSkew is how far GitHub's clock may run behind the platform's when it says when an
	// installation last changed.
	clockSkew = time.Minute
)

var errNoApp = refusal.Refuse(http.StatusServiceUnavailable, ReasonGitHubNotConfigured,
	"this deployment has no GitHub App configured, so GitHub cannot be connected and no repository cloned through it")

// gitHubRefusal turns what GitHub or the store refused into what the caller is told: the store's
// missing rows here, the rest as the git host answers them (github.Refusal).
func gitHubRefusal(logger *zap.Logger, op string, err error, gone int) error {
	switch {
	case errors.Is(err, db.ErrOrgNotFound):
		return refusal.NotFound("org")
	case errors.Is(err, db.ErrGitHubInstallationNotFound):
		return refusal.NotFound("GitHub installation")
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return refusal.NotFound("platform experiment")
	case errors.Is(err, db.ErrGitHubInstallStateInvalid):
		return refusal.Invalid(ReasonInstallStateInvalid, "this return from GitHub does not match an install you started in the last 15 minutes, or it was used already: press Install GitHub App again")
	}
	return github.Refusal(logger, op, err, gone)
}

func (r *Repositories) app() (GitHubApp, error) {
	if r.github == nil {
		return nil, errNoApp
	}
	return r.github, nil
}

// beginInstall records an install of the App that the caller starts for an org, and returns the
// address on GitHub that carries its state. returnTo is the page of the UI the person returns to.
func (r *Repositories) beginInstall(ctx context.Context, orgID, returnTo string) (*InstallStart, error) {
	p, err := r.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	if !inApp(returnTo) {
		return nil, refusal.Invalid(ReasonInvalidRequest, "return_to is a path of the UI: one leading slash, no second, no backslash or control character")
	}
	actor := p.Actor()
	if actor.ID == "" {
		return nil, refusal.Refuse(http.StatusForbidden, ReasonForbidden, "an install of the GitHub App is started by a user or an API key, the one that finishes it")
	}
	state, expires, err := r.store.IssueGitHubInstallState(ctx, orgID, actor, returnTo, installStateLifetime)
	if err != nil {
		return nil, err
	}
	address, err := url.Parse(app.App().InstallURL)
	if err != nil {
		return nil, err
	}
	query := address.Query()
	query.Set("state", state)
	address.RawQuery = query.Encode()
	return &InstallStart{State: state, InstallURL: address.String(), ExpiresAt: expires}, nil
}

// inApp is a path of the UI: one leading slash and no second, and no backslash or control
// character, which a browser reads as a slash or drops and so turns into a second one that
// leaves the origin.
func inApp(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.ContainsFunc(path, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f })
}

// finishInstall connects the installation GitHub handed back with state to the org the state was
// issued for. The state proves who started the install and for which org: it is the caller's, it
// was not taken before and it has not run out. installation_id comes from the browser, so GitHub
// is asked as the App about it: it exists, is not suspended, has granted every permission, and
// was made or changed after the state was issued, which an installation the person did not just
// install or change was not. One the org connected before is answered as it is, after its
// repositories changed.
func (r *Repositories) finishInstall(ctx context.Context, state string, installationID int64) (*InstallFinished, error) {
	p, err := r.access.Account(ctx)
	if err != nil {
		return nil, err
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	if state == "" || installationID <= 0 {
		return nil, refusal.Invalid(ReasonInvalidRequest, "state and installation_id are required, as GitHub handed them back from the install")
	}
	started, err := r.store.TakeGitHubInstallState(ctx, state, p.Actor())
	if err != nil {
		return nil, err
	}
	if _, err := r.access.OrgAdmin(ctx, started.OrgID); err != nil {
		return nil, err
	}
	live, err := app.Installation(ctx, installationID)
	if errors.Is(err, github.ErrInstallationGone) {
		err = &github.InstallationGoneError{Account: "the GitHub account", Installation: installationID, Suspended: errors.Is(err, github.ErrInstallationSuspended)}
	}
	if err != nil {
		return nil, err
	}
	connected, err := r.store.ListGitHubInstallations(ctx, started.OrgID)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(connected, func(one *domain.OrgGitHubInstallation) bool { return one.InstallationID == installationID }); i >= 0 {
		return &InstallFinished{OrgID: started.OrgID, ReturnTo: started.ReturnTo, Installation: connected[i]}, nil
	}
	if live.ChangedAt.Before(started.IssuedAt.Add(-clockSkew)) {
		return nil, refusal.Invalid(ReasonInstallationNotNew, "GitHub says the App on "+live.AccountLogin+" was last changed before this install started, so nothing shows it was you who just installed it. "+
			"On GitHub, change which repositories the App can reach there, or uninstall it and press Install GitHub App again")
	}
	installation := &domain.OrgGitHubInstallation{OrgID: started.OrgID, InstallationID: live.ID, AccountLogin: live.AccountLogin, AccountType: live.AccountType, ConnectedActor: p.Actor()}
	if err := r.store.ConnectGitHubInstallation(ctx, installation); err != nil {
		return nil, err
	}
	return &InstallFinished{OrgID: started.OrgID, ReturnTo: started.ReturnTo, Installation: installation}, nil
}

// InstallStart is an install of the GitHub App started for an org.
type InstallStart struct {
	State      string    `json:"state" doc:"Good once, for the caller, until expires_at. GitHub hands it back to the App's setup URL with installation_id."`
	InstallURL string    `json:"install_url" doc:"GitHub's install page of the App, carrying state. Open it for the person."`
	ExpiresAt  time.Time `json:"expires_at"`
}

// InstallFinished is an installation connected from the install it finished.
type InstallFinished struct {
	OrgID        string                        `json:"org_id"`
	ReturnTo     string                        `json:"return_to" doc:"The page of the UI the install was started from."`
	Installation *domain.OrgGitHubInstallation `json:"installation"`
}

// disconnect takes an installation away from an org.
func (r *Repositories) disconnect(ctx context.Context, orgID string, installationID int64) error {
	if _, err := r.access.OrgAdmin(ctx, orgID); err != nil {
		return err
	}
	return r.store.DisconnectGitHubInstallation(ctx, orgID, installationID)
}

// repositories lists what the org's connected installations cover, read from GitHub now. An
// installation removed on GitHub covers nothing and is left out.
func (r *Repositories) repositories(ctx context.Context, orgID, q string) ([]Repository, error) {
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	installations, err := r.store.ListGitHubInstallations(ctx, orgID)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	out := []Repository{}
	for _, installation := range installations {
		covered, err := app.Repositories(ctx, installation.InstallationID)
		if errors.Is(err, github.ErrInstallationGone) {
			r.logger.Warn("a connected github installation is gone", zap.String("orgID", orgID), zap.Int64("installationID", installation.InstallationID))
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, one := range covered {
			if strings.Contains(strings.ToLower(one.FullName), q) {
				out = append(out, Repository{Repository: one.FullName, InstallationID: installation.InstallationID, Private: one.Private})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Repository) < strings.ToLower(out[j].Repository) })
	return out, nil
}

// GitHubAddresses are the GitHub addresses of the platform's App a browser needs.
type GitHubAddresses = github.App

// App is the platform's GitHub App, as a browser needs its addresses.
func (r *Repositories) App(ctx context.Context) (GitHubAddresses, error) {
	if _, err := r.access.Account(ctx); err != nil {
		return github.App{}, err
	}
	app, err := r.app()
	if err != nil {
		return github.App{}, err
	}
	return app.App(), nil
}

// BeginInstall starts an install of the App for orgID; see beginInstall.
func (r *Repositories) BeginInstall(ctx context.Context, orgID, returnTo string) (*InstallStart, error) {
	started, err := r.beginInstall(ctx, orgID, returnTo)
	if err != nil {
		return nil, gitHubRefusal(r.logger, "create-github-install-state", err, http.StatusUnprocessableEntity)
	}
	return started, nil
}

// FinishInstall connects the installation an install made or changed; see finishInstall.
func (r *Repositories) FinishInstall(ctx context.Context, state string, installationID int64) (*InstallFinished, error) {
	finished, err := r.finishInstall(ctx, state, installationID)
	if err != nil {
		return nil, gitHubRefusal(r.logger, "create-github-installation", err, http.StatusUnprocessableEntity)
	}
	return finished, nil
}

// Installations is one page of the installations orgID connected, and how many there are.
func (r *Repositories) Installations(ctx context.Context, orgID string, page Page) ([]*domain.OrgGitHubInstallation, int, error) {
	if _, err := r.access.OrgMember(ctx, orgID); err != nil {
		return nil, 0, err
	}
	limit, offset, err := page.bounds()
	if err != nil {
		return nil, 0, err
	}
	installations, err := r.store.ListGitHubInstallations(ctx, orgID)
	if err != nil {
		return nil, 0, gitHubRefusal(r.logger, "list-github-installations", err, http.StatusConflict)
	}
	return pageOf(installations, limit, offset), len(installations), nil
}

// Disconnect takes an installation away from orgID.
func (r *Repositories) Disconnect(ctx context.Context, orgID string, installationID int64) error {
	if err := r.disconnect(ctx, orgID, installationID); err != nil {
		return gitHubRefusal(r.logger, "delete-github-installation", err, http.StatusConflict)
	}
	return nil
}

// MakeDefault makes the installation where the new repositories of orgID are created.
func (r *Repositories) MakeDefault(ctx context.Context, orgID string, installationID int64) error {
	if _, err := r.access.OrgAdmin(ctx, orgID); err != nil {
		return err
	}
	if err := r.store.MakeGitHubInstallationDefault(ctx, orgID, installationID); err != nil {
		return gitHubRefusal(r.logger, "make-github-installation-default", err, http.StatusConflict)
	}
	return nil
}

// Check asks GitHub whether a connected installation of orgID still works.
func (r *Repositories) Check(ctx context.Context, orgID string, installationID int64) error {
	const op = "check-github-installation"
	if _, err := r.access.OrgMember(ctx, orgID); err != nil {
		return err
	}
	app, err := r.app()
	if err != nil {
		return err
	}
	connected, err := r.store.ListGitHubInstallations(ctx, orgID)
	if err != nil {
		return gitHubRefusal(r.logger, op, err, http.StatusUnprocessableEntity)
	}
	i := slices.IndexFunc(connected, func(one *domain.OrgGitHubInstallation) bool { return one.InstallationID == installationID })
	if i < 0 {
		return refusal.NotFound("GitHub installation")
	}
	if _, err := github.Installed(ctx, app, connected[i]); err != nil {
		return gitHubRefusal(r.logger, op, err, http.StatusUnprocessableEntity)
	}
	return nil
}

// List is one page of the repositories the installations of orgID cover whose owner/name holds
// q, and how many match.
func (r *Repositories) List(ctx context.Context, orgID, q string, page Page) ([]Repository, int, error) {
	if _, err := r.access.OrgMember(ctx, orgID); err != nil {
		return nil, 0, err
	}
	limit, offset, err := page.bounds()
	if err != nil {
		return nil, 0, err
	}
	found, err := r.repositories(ctx, orgID, q)
	if err != nil {
		return nil, 0, gitHubRefusal(r.logger, "list-github-repositories", err, http.StatusConflict)
	}
	return pageOf(found, limit, offset), len(found), nil
}

func pageOf[T any](all []T, limit, offset int) []T {
	if offset >= len(all) {
		return []T{}
	}
	return all[offset:min(offset+limit, len(all))]
}
