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

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost/github"
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
// people started. *db.AccountsStore satisfies it.
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
		return notFound("org")
	case errors.Is(err, db.ErrGitHubInstallationNotFound):
		return notFound("GitHub installation")
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return notFound("platform experiment")
	case errors.Is(err, db.ErrGitHubInstallStateInvalid):
		return invalid(ReasonInstallStateInvalid, "this return from GitHub does not match an install you started in the last 15 minutes, or it was used already: press Install GitHub App again")
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
		return nil, invalid(ReasonInvalidRequest, "return_to is a path of the UI: one leading slash, no second, no backslash or control character")
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
		return nil, invalid(ReasonInvalidRequest, "state and installation_id are required, as GitHub handed them back from the install")
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
		return nil, invalid(ReasonInstallationNotNew, "GitHub says the App on "+live.AccountLogin+" was last changed before this install started, so nothing shows it was you who just installed it. "+
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

// RegisterRepositoriesHuma registers the GitHub operations on the shared API document.
func RegisterRepositoriesHuma(doc *apidocs.Doc, r *Repositories) {
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-github-app", Method: "GET", Path: "/github/app",
		Summary: "The platform's GitHub App", Tags: []string{"github"},
		Description: "The GitHub addresses a browser needs for an org's repositories. new_repository_url is where a person creates a repository themselves. " +
			"install_url is the App's install page; connecting an installation starts at POST /orgs/{id}/github-install-states, whose install_url carries the state. " +
			"Answers 503 github_not_configured on a deployment that has no GitHub App.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body github.App }, error) {
		if _, err := r.access.Account(ctx); err != nil {
			return nil, err
		}
		app, err := r.app()
		if err != nil {
			return nil, err
		}
		return &struct{ Body github.App }{Body: app.App()}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-github-install-state", Method: "POST", Path: "/orgs/{id}/github-install-states", DefaultStatus: http.StatusCreated,
		Summary: "Start installing the platform's GitHub App for an org", Tags: []string{"github"},
		Description: "For an owner of the org, as a user or an API key. Answers a state good once, for the caller, for 15 minutes, and install_url, GitHub's install page of the App carrying it. " +
			"The person installs the App there on their own GitHub account or an organisation, or changes the repositories of an installation they made before. " +
			"GitHub then sends them to the App's setup URL with state, installation_id and setup_action, and POST /github/installations connects the installation. " +
			"return_to is the page of the UI the install starts from, handed back when it finishes. 422 invalid_request when it is not a path of the UI. 503 github_not_configured.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			ReturnTo string `json:"return_to" doc:"A path of the UI, as /settings."`
		}
	}) (*struct{ Body *InstallStart }, error) {
		started, err := r.beginInstall(ctx, in.ID, in.Body.ReturnTo)
		if err != nil {
			return nil, gitHubRefusal(r.logger, "create-github-install-state", err, http.StatusUnprocessableEntity)
		}
		return &struct{ Body *InstallStart }{Body: started}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "create-github-installation", Method: "POST", Path: "/github/installations", DefaultStatus: http.StatusCreated,
		Summary: "Connect the GitHub App installation an install made or changed", Tags: []string{"github"},
		Description: "For the user or API key that started the install, while still an owner of its org. state and installation_id are what GitHub handed back to the setup URL. " +
			"The state is taken once whatever the outcome. The installation is asked of GitHub as the App: it must exist, not be suspended, have granted every permission the App asks for, " +
			"and have been made or changed after the state was issued. An installation the org connected before is answered as it is; a new one is connected, and is the org's default for new repositories when it is the org's first. " +
			"account_type says whether the account is a person's own (User) or an organisation. " +
			"422 install_state_invalid (made up, used, expired or another person's), installation_not_new (last changed before the install started), installation_gone, installation_suspended, " +
			"github_permission_missing naming each permission missing as name:level. 502 github_failed, 503 github_not_configured.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			State          string `json:"state" doc:"The state of POST /orgs/{id}/github-install-states, as GitHub handed it back."`
			InstallationID int64  `json:"installation_id" doc:"The installation GitHub handed back."`
		}
	}) (*struct{ Body *InstallFinished }, error) {
		finished, err := r.finishInstall(ctx, in.Body.State, in.Body.InstallationID)
		if err != nil {
			return nil, gitHubRefusal(r.logger, "create-github-installation", err, http.StatusUnprocessableEntity)
		}
		return &struct{ Body *InstallFinished }{Body: finished}, nil
	})

	type installationsPage struct {
		TotalCount int `header:"X-Total-Count" doc:"The installations the org connected, ignoring limit and offset."`
		Body       []*domain.OrgGitHubInstallation
	}
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-installations", Method: "GET", Path: "/orgs/{id}/github-installations",
		Summary: "List the GitHub App installations an org connected", Tags: []string{"github"},
		Description: "For any member of the org, oldest first. What was recorded at connect: GitHub is not asked, so an installation removed there " +
			"since still shows, and a platform experiment that names a repository of it is refused its git credential. " +
			"account_type is User for a person's own account and Organization, and absent on an installation connected before " +
			"the type was kept, until it is connected again.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
		Page
	}) (*installationsPage, error) {
		const op = "list-github-installations"
		if _, err := r.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		limit, offset, err := in.bounds()
		if err != nil {
			return nil, err
		}
		installations, err := r.store.ListGitHubInstallations(ctx, in.ID)
		if err != nil {
			return nil, gitHubRefusal(r.logger, op, err, http.StatusConflict)
		}
		return &installationsPage{TotalCount: len(installations), Body: pageOf(installations, limit, offset)}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "delete-github-installation", Method: "DELETE", Path: "/orgs/{id}/github-installations/{installation_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Disconnect a GitHub App installation from an org", Tags: []string{"github"},
		Description: "For an owner of the org. No git credential is minted through it from then on: a platform experiment that names a repository of " +
			"it keeps running, its agents and jobs can no longer clone, fetch or push once the credential they hold runs out, within the hour, and " +
			"their next credential call answers 409 installation_not_connected. The App stays installed on GitHub: removing it there is the account " +
			"owner's own step. 404 not_found when the org had not connected it.",
	}, func(ctx context.Context, in *struct {
		ID             string `path:"id"`
		InstallationID int64  `path:"installation_id"`
	}) (*struct{}, error) {
		if err := r.disconnect(ctx, in.ID, in.InstallationID); err != nil {
			return nil, gitHubRefusal(r.logger, "delete-github-installation", err, http.StatusConflict)
		}
		return &struct{}{}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "make-github-installation-default", Method: "PUT", Path: "/orgs/{id}/github-installations/{installation_id}/default", DefaultStatus: http.StatusNoContent,
		Summary: "Make a GitHub installation where an org's new repositories are created", Tags: []string{"github"},
		Description: "For an owner of the org. A platform experiment or draft that names neither repository nor repository_account has its repository created on this installation's account, " +
			"in place of the default before. With one installation connected that one is used whether or not it is the default; with several and none the default, " +
			"such a create is refused 422 repository_account_required. Disconnecting the default leaves none. 404 not_found when the org has not connected it.",
	}, func(ctx context.Context, in *struct {
		ID             string `path:"id"`
		InstallationID int64  `path:"installation_id"`
	}) (*struct{}, error) {
		const op = "make-github-installation-default"
		if _, err := r.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := r.store.MakeGitHubInstallationDefault(ctx, in.ID, in.InstallationID); err != nil {
			return nil, gitHubRefusal(r.logger, op, err, http.StatusConflict)
		}
		return &struct{}{}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "check-github-installation", Method: "GET", Path: "/orgs/{id}/github-installations/{installation_id}/check", DefaultStatus: http.StatusNoContent,
		Summary: "Check with GitHub that a connected installation still works", Tags: []string{"github"},
		Description: "For any member of the org. Asks GitHub now whether the App is still installed on the account and has granted every permission the App asks for (app-manifest.json), " +
			"the check a create runs on the account it creates on. 204 when it has. 422 installation_gone when GitHub removed it, installation_suspended when its owner suspended it, github_permission_missing naming what it has not granted, " +
			"each with what to do. 404 not_found when the org has not connected it. 502 github_failed, 503 github_not_configured.",
	}, func(ctx context.Context, in *struct {
		ID             string `path:"id"`
		InstallationID int64  `path:"installation_id"`
	}) (*struct{}, error) {
		const op = "check-github-installation"
		if _, err := r.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		app, err := r.app()
		if err != nil {
			return nil, err
		}
		connected, err := r.store.ListGitHubInstallations(ctx, in.ID)
		if err != nil {
			return nil, gitHubRefusal(r.logger, op, err, http.StatusUnprocessableEntity)
		}
		i := slices.IndexFunc(connected, func(one *domain.OrgGitHubInstallation) bool { return one.InstallationID == in.InstallationID })
		if i < 0 {
			return nil, notFound("GitHub installation")
		}
		if _, err := github.Installed(ctx, app, connected[i]); err != nil {
			return nil, gitHubRefusal(r.logger, op, err, http.StatusUnprocessableEntity)
		}
		return &struct{}{}, nil
	})

	type repositoriesPage struct {
		TotalCount int `header:"X-Total-Count" doc:"The repositories that match, ignoring limit and offset."`
		Body       []Repository
	}
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-repositories", Method: "GET", Path: "/orgs/{id}/github-repositories",
		Summary: "List the repositories an org's GitHub installations cover", Tags: []string{"github"},
		Description: "For any member of the org: what a platform experiment of the org may name as its repository, sorted by name. q keeps the " +
			"repositories whose owner/name contains it, whatever the casing. Nothing is kept between calls: each one asks GitHub for every " +
			"repository of every connected installation, a hundred to a request, and pages the result here, so X-Total-Count is exact and a call " +
			"costs as many GitHub requests as the installations hold hundreds of repositories. An installation removed on GitHub is left out. " +
			"502 github_failed when GitHub answers 5xx or not at all for an installation, the message naming it and GitHub's answer. " +
			"503 github_not_configured.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
		Q  string `query:"q" doc:"Text the repository's owner/name must contain."`
		Page
	}) (*repositoriesPage, error) {
		const op = "list-github-repositories"
		if _, err := r.access.OrgMember(ctx, in.ID); err != nil {
			return nil, err
		}
		limit, offset, err := in.bounds()
		if err != nil {
			return nil, err
		}
		found, err := r.repositories(ctx, in.ID, in.Q)
		if err != nil {
			return nil, gitHubRefusal(r.logger, op, err, http.StatusConflict)
		}
		return &repositoriesPage{TotalCount: len(found), Body: pageOf(found, limit, offset)}, nil
	})
}

func pageOf[T any](all []T, limit, offset int) []T {
	if offset >= len(all) {
		return []T{}
	}
	return all[offset:min(offset+limit, len(all))]
}
