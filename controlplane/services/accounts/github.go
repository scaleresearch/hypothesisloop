package accounts

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

// GitHubApp is what the platform asks of its GitHub App. *github.Client satisfies it.
type GitHubApp interface {
	App() github.App
	UserInstallations(ctx context.Context, userCode, redirectURI string) ([]github.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]github.Repository, error)
	Token(ctx context.Context, installationID int64, fullName string, access github.Access) (*github.Token, error)
	File(ctx context.Context, token *github.Token, ref, path string) ([]byte, error)
	CreateRepository(ctx context.Context, installationID int64, name, description string, files map[string]string) (*github.Repository, error)
}

// InstallationStore is the persistence of the installations orgs connected. *db.AccountsStore
// satisfies it.
type InstallationStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ConnectGitHubInstallation(ctx context.Context, installation *domain.OrgGitHubInstallation) error
	ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error)
	GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error)
	DisconnectGitHubInstallation(ctx context.Context, orgID string, installationID int64) error
}

// ExperimentRepositories says which repository a platform experiment names and which project
// it is in. *db.PlatformExperimentsStore satisfies it.
type ExperimentRepositories interface {
	PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, err error)
}

// Repositories serves the GitHub installations orgs connect and what they cover, and the git
// credential for the one repository a platform experiment names on the deployment's git host.
type Repositories struct {
	store       InstallationStore
	experiments ExperimentRepositories
	github      GitHubApp
	host        githost.Provider
	logger      *zap.Logger
	access      *auth.Access
}

// NewRepositories takes the App's client, or nil for a deployment whose git host is not GitHub,
// and the git host credentials are minted on.
func NewRepositories(store InstallationStore, experiments ExperimentRepositories, app GitHubApp, host githost.Provider, access *auth.Access, logger *zap.Logger) *Repositories {
	if store == nil || experiments == nil || host == nil || access == nil || logger == nil {
		panic("accounts.NewRepositories: the stores, a git host, an access check and a logger are required")
	}
	return &Repositories{store: store, experiments: experiments, github: app, host: host, logger: logger, access: access}
}

// Repository is a repository an org can name on a platform experiment.
type Repository struct {
	Repository     string `json:"repository" doc:"owner/name, as a platform experiment names it."`
	InstallationID int64  `json:"installation_id" doc:"The connected installation that covers it."`
	Private        bool   `json:"private"`
}

// GitCredential answers git's credential helper: the fields git reads, and when the password
// stops working.
type GitCredential struct {
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Path      string    `json:"path" doc:"The path asked for, which names the one repository the password works on."`
	Username  string    `json:"username"`
	Password  string    `json:"password" doc:"What the git host takes for the repository. On GitHub an installation token minted for this call and stored nowhere."`
	ExpiresAt time.Time `json:"expires_at,omitzero" doc:"Absent for a password that does not run out."`
}

// The reasons a client tells a refusal about GitHub apart by.
const (
	ReasonGitHubNotConfigured        = "github_not_configured"
	ReasonInvalidRequest             = "invalid_request"
	ReasonCodeRejected               = "code_rejected"
	ReasonAppNotInstalled            = "app_not_installed"
	ReasonInstallationChoiceRequired = "installation_choice_required"
	ReasonInstallationNotReachable   = "installation_not_reachable"
	ReasonInstallationNotControlled  = "installation_not_controlled"
	ReasonInstallationNotConnected   = "installation_not_connected"
	ReasonInstallationGone           = "installation_gone"
	ReasonPermissionNotGranted       = "permission_not_granted"
	ReasonInvalidRepository          = "invalid_repository"
	ReasonRepositoryNotReachable     = "repository_not_reachable"
	ReasonNoRepository               = "no_repository"
	ReasonGitHubNotConnected         = "github_not_connected"
	ReasonRepositoryNotCreated       = "repository_not_created"
	ReasonRepositoryAccountRequired  = "repository_account_required"
	ReasonNoExperimentFile           = "no_experiment_file"
	ReasonRunOver                    = "run_over"
	ReasonForbidden                  = "forbidden"
	ReasonUnavailable                = "unavailable"
)

// choiceRequired is the refusal of a connect that named no installation while the person
// reaches several: it carries them, for the client to ask which.
type choiceRequired struct {
	api.Refusal
	Installations []github.Installation `json:"installations"`
}

var errNoApp = api.Refuse(http.StatusServiceUnavailable, ReasonGitHubNotConfigured,
	"this deployment has no GitHub App configured, so GitHub cannot be connected and no repository cloned through it")

// errNotConnected reports a repository whose owner is no GitHub account the org connected.
var errNotConnected = errors.New("the org has connected no installation on that GitHub account")

// errNoInstallation reports an org that connected no GitHub account at all.
var errNoInstallation = errors.New("the org has connected no GitHub account")

// refusal turns what GitHub or the store refused into what the caller is told. gone is the
// status of a repository the org's installations do not reach: 422 where a request names it,
// 409 where a platform experiment already does. Anything else is the platform's or GitHub's
// failure: it is logged and answered without its text.
func refusal(logger *zap.Logger, op string, err error, gone int) error {
	var status huma.StatusError
	var uncommitted *github.FirstCommitError
	switch {
	case errors.As(err, &status):
		return err
	case errors.As(err, &uncommitted):
		logger.Error("github created a repository and did not take its first commit", zap.String("operation", op), zap.Error(err))
		return api.Refuse(http.StatusBadGateway, ReasonRepositoryNotCreated, "GitHub created the repository "+uncommitted.Repository+" and did not take its first commit, so nothing else was created. The repository is left on the GitHub account without the experiment's files: name it in repository on the next try, where hl repo init adds them, or delete it on GitHub")
	case errors.Is(err, db.ErrOrgNotFound):
		return notFound("org")
	case errors.Is(err, db.ErrGitHubInstallationNotFound):
		return notFound("GitHub installation")
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return notFound("platform experiment")
	case errors.Is(err, github.ErrInvalidRepository):
		return invalid(ReasonInvalidRepository, "repository must be owner/name, as in the repository's address on GitHub")
	case errors.Is(err, errNoInstallation):
		return invalid(ReasonGitHubNotConnected, "a platform experiment keeps its code in a GitHub repository, and the org has connected no GitHub account: an owner of the org connects one with POST /orgs/{id}/github-installations (Settings, GitHub), and the experiment then names a repository there or the platform creates one")
	case errors.Is(err, github.ErrCannotCreateRepositories):
		return invalid(ReasonRepositoryNotCreated, "the GitHub account the org connected does not let the platform's GitHub App create a repository: its owner grants the App the creation of repositories on GitHub, or the request names a repository the installation covers")
	case errors.Is(err, github.ErrRepositoryExists):
		return api.Refuse(http.StatusConflict, ReasonRepositoryNotCreated, "the GitHub account the org connected already has a repository of the name this platform experiment's would take: create it again, or name a repository")
	case errors.Is(err, github.ErrCodeRejected):
		return invalid(ReasonCodeRejected, "GitHub did not accept the code: it works once, for ten minutes, and with the redirect_uri it was issued to, so authorise again and send the new one")
	case errors.Is(err, errNotConnected):
		return api.Refuse(gone, ReasonInstallationNotConnected, "the org has connected no GitHub installation on that GitHub account, the one that owns the repository or the one a new repository was asked on: an owner of the org connects it with POST /orgs/{id}/github-installations")
	case errors.Is(err, github.ErrRepositoryNotInstalled):
		return api.Refuse(gone, ReasonRepositoryNotReachable, "the GitHub installation the org connected does not cover that repository: its owner adds it to the installation on GitHub")
	case errors.Is(err, github.ErrInstallationGone):
		return api.Refuse(gone, ReasonInstallationGone, "the GitHub App installation was removed or suspended on GitHub: its owner installs the App again and an owner of the org connects it")
	case errors.Is(err, github.ErrPermissionNotGranted):
		return api.Refuse(gone, ReasonPermissionNotGranted, "the GitHub App installation has not granted the access the App asks for: its owner accepts the App's permissions on GitHub")
	case errors.Is(err, github.ErrFileNotFound):
		return invalid(ReasonNoExperimentFile, "the repository has no "+domain.ExperimentFile+" on its default branch")
	case errors.Is(err, github.ErrNotAFile):
		return invalid(ReasonNoExperimentFile, domain.ExperimentFile+" on the repository's default branch is not a file")
	case errors.Is(err, github.ErrFileTooLarge):
		return invalid(ReasonNoExperimentFile, domain.ExperimentFile+" is larger than the platform reads from a repository")
	case errors.Is(err, github.ErrRateLimited):
		logger.Warn("github is rate limiting the platform", zap.String("operation", op))
		return api.Refuse(http.StatusServiceUnavailable, ReasonUnavailable, "GitHub is rate limiting the platform: try again shortly")
	case errors.Is(err, github.ErrAppCredentials):
		logger.Error("github refused the App's credentials", zap.String("operation", op), zap.Error(err))
		return api.Refuse(http.StatusBadGateway, ReasonUnavailable, "the platform could not authenticate to GitHub")
	default:
		return auth.Internal(logger, "github request failed", err, zap.String("operation", op))
	}
}

func (r *Repositories) app() (GitHubApp, error) {
	if r.github == nil {
		return nil, errNoApp
	}
	return r.github, nil
}

// connect records an installation of the App as an org's. The proof is GitHub's: the person
// behind code controls the account the installation is on, their own or an organisation they
// are an admin of. Reaching it is not enough, since a collaborator on one repository reaches the
// installation that covers it. With no installation named, the only one they control is meant,
// and with several the answer lists them for the client to ask which.
func (r *Repositories) connect(ctx context.Context, orgID, code, redirectURI string, installationID int64) (*domain.OrgGitHubInstallation, error) {
	p, err := r.access.OrgAdmin(ctx, orgID)
	if err != nil {
		return nil, err
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	if code == "" || redirectURI == "" || installationID < 0 {
		return nil, invalid(ReasonInvalidRequest, "code and redirect_uri are required, and installation_id is a positive number or left out: GitHub hands the code back after the person authorises the App, to the redirect_uri it was sent")
	}
	reached, err := app.UserInstallations(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	notControlled := invalid(ReasonInstallationNotControlled, "an installation is connected by whoever controls the GitHub account it is on: that account itself, or an admin of the GitHub organisation. Your GitHub account reaches it and is neither, so ask an admin of that organisation to connect it")
	controlled := []github.Installation{}
	for _, one := range reached {
		if one.Controlled {
			controlled = append(controlled, one)
		}
	}
	var chosen *github.Installation
	switch {
	case len(reached) == 0:
		return nil, invalid(ReasonAppNotInstalled, "your GitHub account reaches no installation of the GitHub App: install it on a GitHub account, at install_url of GET /github/app, and connect again")
	case installationID == 0 && len(controlled) == 0:
		return nil, notControlled
	case installationID == 0 && len(controlled) > 1:
		return nil, &choiceRequired{
			Refusal: *api.Refuse(http.StatusConflict, ReasonInstallationChoiceRequired,
				"your GitHub account controls several accounts the GitHub App is installed on: authorise again and send the installation_id of the one to connect"),
			Installations: controlled,
		}
	case installationID == 0:
		chosen = &controlled[0]
	default:
		for i := range reached {
			if reached[i].ID == installationID {
				chosen = &reached[i]
			}
		}
		if chosen == nil {
			return nil, invalid(ReasonInstallationNotReachable, "your GitHub account does not reach that installation of the GitHub App")
		}
		if !chosen.Controlled {
			return nil, notControlled
		}
	}
	installation := &domain.OrgGitHubInstallation{OrgID: orgID, InstallationID: chosen.ID, AccountLogin: chosen.AccountLogin, ConnectedActor: p.Actor()}
	if err := r.store.ConnectGitHubInstallation(ctx, installation); err != nil {
		return nil, err
	}
	return installation, nil
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

// credential mints the caller's git credential. It goes only to what the platform launched into
// a platform experiment, for the repository that experiment names and for no other address: an
// agent may push, a coordinator and a job read. Nothing is minted for a run that is over, though
// its token still files summaries, since the credential would outlive the token by an hour.
func (r *Repositories) credential(ctx context.Context, protocol, host, path string) (*GitCredential, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Unauthenticated("the request carries no principal")
	}
	write := false
	switch p.Kind {
	case auth.KindAgent:
		write = true
	case auth.KindCoordinator, auth.KindJob:
	default:
		return nil, api.Refuse(http.StatusForbidden, ReasonForbidden, "a "+string(p.Kind)+" token has no repository of its own: a git credential goes to an agent, a coordinator or a job")
	}
	if p.RunOver {
		return nil, api.Refuse(http.StatusForbidden, ReasonRunOver, "the run of this "+string(p.Kind)+" token in platform experiment "+p.PlatformExperimentID+" is over, and no git credential is handed out for it")
	}
	projectID, repository, err := r.experiments.PlatformExperimentRepository(ctx, p.PlatformExperimentID)
	if err != nil {
		return nil, err
	}
	if repository == "" {
		return nil, api.Refuse(http.StatusNotFound, ReasonNoRepository, "platform experiment "+p.PlatformExperimentID+" names no repository")
	}
	if !r.host.Serves(repository, protocol, host, path) {
		return nil, api.Refuse(http.StatusNotFound, ReasonNotFound, "no git credential for that address: this token's is for "+r.host.URL(repository)+" only")
	}
	minted, err := r.host.Credential(ctx, projectID, repository, write)
	if err != nil {
		return nil, err
	}
	return &GitCredential{
		Protocol: protocol, Host: strings.ToLower(host), Path: strings.Trim(path, "/"),
		Username: minted.Username, Password: minted.Password, ExpiresAt: minted.ExpiresAt,
	}, nil
}

// RegisterRepositoriesHuma registers the GitHub operations on the shared API document.
func RegisterRepositoriesHuma(doc *apidocs.Doc, r *Repositories) {
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-github-app", Method: "GET", Path: "/github/app",
		Summary: "Where to install and authorise the platform's GitHub App", Tags: []string{"github"},
		Description: "The two GitHub addresses a browser needs before an org connects GitHub. Send the person to install_url to install the App on a " +
			"GitHub account and choose its repositories: GitHub returns them with installation_id. Send them to authorize_url, with redirect_uri and " +
			"state added, to prove who they are: GitHub returns them with code and state. The client must generate state itself, keep it, and refuse a " +
			"return whose state is not the one it sent, or another site could finish the connect in the person's name. " +
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
		OperationID: "create-github-installation", Method: "POST", Path: "/orgs/{id}/github-installations", DefaultStatus: http.StatusCreated,
		Summary: "Connect a GitHub App installation to an org", Tags: []string{"github"},
		Description: "For an owner of the org. From then on a platform experiment of the org may name any repository the installation covers, and the " +
			"owner of the GitHub account decides that set on GitHub. code, from GitHub's authorisation of the App, must belong to a GitHub account that " +
			"controls the account the installation is on: that account itself, or an admin of the GitHub organisation. GitHub is asked which installations it reaches " +
			"and which accounts it controls, and the person's token is revoked before this call returns. Reaching an installation, as a collaborator on one of its repositories does, connects nothing. " +
			"redirect_uri is the one the code was issued to. Verify state before calling (see GET /github/app). " +
			"installation_id may be left out when GitHub did not hand one back, as when the App was installed before: the one installation the " +
			"person controls is connected, and when they control several the answer is 409 installation_choice_required with installations, a list of " +
			"{installation_id, account_login} to ask them about, after which they authorise again. Connecting one that is connected answers with it. " +
			"422 invalid_request, code_rejected, app_not_installed (the person reaches none), installation_not_reachable (not the one named) or " +
			"installation_not_controlled (reached, and on an account that is not theirs to connect). " +
			"503 github_not_configured.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Code           string `json:"code" doc:"The code GitHub returned from authorize_url. It works once."`
			RedirectURI    string `json:"redirect_uri" doc:"The redirect_uri sent to authorize_url, to which GitHub returned the code."`
			InstallationID int64  `json:"installation_id,omitempty" required:"false" doc:"The installation to connect, as GitHub returned it from install_url or as a 409 of this call listed it."`
		}
	}) (*struct{ Body *domain.OrgGitHubInstallation }, error) {
		installation, err := r.connect(ctx, in.ID, in.Body.Code, in.Body.RedirectURI, in.Body.InstallationID)
		if err != nil {
			return nil, refusal(r.logger, "create-github-installation", err, http.StatusUnprocessableEntity)
		}
		return &struct{ Body *domain.OrgGitHubInstallation }{Body: installation}, nil
	})

	type installationsPage struct {
		TotalCount int `header:"X-Total-Count" doc:"The installations the org connected, ignoring limit and offset."`
		Body       []*domain.OrgGitHubInstallation
	}
	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-installations", Method: "GET", Path: "/orgs/{id}/github-installations",
		Summary: "List the GitHub App installations an org connected", Tags: []string{"github"},
		Description: "For any member of the org, oldest first. What was recorded at connect: GitHub is not asked, so an installation removed there " +
			"since still shows, and a platform experiment that names a repository of it is refused its git credential.",
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
			return nil, refusal(r.logger, op, err, http.StatusConflict)
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
		if _, err := r.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := r.store.DisconnectGitHubInstallation(ctx, in.ID, in.InstallationID); err != nil {
			return nil, refusal(r.logger, "delete-github-installation", err, http.StatusConflict)
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
			return nil, refusal(r.logger, op, err, http.StatusConflict)
		}
		return &repositoriesPage{TotalCount: len(found), Body: pageOf(found, limit, offset)}, nil
	})

	type credentialCreated struct {
		CacheControl string `header:"Cache-Control"`
		Body         GitCredential
	}
	apidocs.Register(doc, apidocs.AudienceJob, huma.Operation{
		OperationID: "create-git-credential", Method: "POST", Path: "/git-credentials", DefaultStatus: http.StatusCreated,
		Summary: "Mint a git credential for your platform experiment's repository", Tags: []string{"github"},
		Description: "For an agent, a coordinator and a job: a GitHub token minted for this call, good for about an hour and for the one repository the " +
			"caller's platform experiment names. An agent's may push, a coordinator's and a job's read. `hl git-credential` is git's credential helper " +
			"over this call, so git asks for a fresh one whenever it needs one and nothing has to be stored. protocol, host and path are what git asked " +
			"a credential for: nothing is minted unless they name that repository over https, as owner/name, owner/name.git or its LFS endpoint " +
			"owner/name.git/info/lfs. 403 run_over once the platform experiment is closed or the agent, for a job the agent it belongs to, was cut, and " +
			"403 forbidden for any other kind of token. 404 no_repository when the platform experiment names none, 404 not_found for another address. " +
			"409 installation_not_connected when the org disconnected the installation, repository_not_reachable when the repository left it, " +
			"installation_gone when the App was removed on GitHub, permission_not_granted when its owner has yet to accept the App's permissions. " +
			"On a deployment whose git host is plain the credential is the deployment's one, the same for every caller and with no expires_at, " +
			"and 404 not_found when that host takes none.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Protocol string `json:"protocol" doc:"The protocol git asked for. Only https is served."`
			Host     string `json:"host" doc:"The host git asked for."`
			Path     string `json:"path" doc:"The path git asked for: git sends it when credential.useHttpPath is true."`
		}
	}) (*credentialCreated, error) {
		credential, err := r.credential(ctx, in.Body.Protocol, in.Body.Host, in.Body.Path)
		if err != nil {
			return nil, refusal(r.logger, "create-git-credential", err, http.StatusConflict)
		}
		return &credentialCreated{CacheControl: "no-store", Body: *credential}, nil
	})
}

func pageOf[T any](all []T, limit, offset int) []T {
	if offset >= len(all) {
		return []T{}
	}
	return all[offset:min(offset+limit, len(all))]
}
