package accounts

import (
	"context"
	"errors"
	"net/http"
	"slices"
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
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost/github"
)

// GitHubApp is what the platform asks of its GitHub App. *github.Client satisfies it.
type GitHubApp interface {
	App() github.App
	UserInstallations(ctx context.Context, userCode, redirectURI string) (*github.UserGrant, error)
	RevokeUser(ctx context.Context, accessToken string) error
	UserToken(ctx context.Context, authorization string) (accessToken, next string, err error)
	Installation(ctx context.Context, installationID int64) (*github.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]github.Repository, error)
	Token(ctx context.Context, installationID int64, fullName string, access github.Access) (*github.Token, error)
	File(ctx context.Context, token *github.Token, ref, path string) ([]byte, error)
	CreateRepository(ctx context.Context, installationID int64, userToken, name, description string, files map[string]string) (*github.Repository, error)
	StartFresh(ctx context.Context, token *github.Token, files map[string]string) error
}

// InstallationStore is the persistence of the installations orgs connected.
// *db.GitHubInstallationsStore satisfies it.
type InstallationStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ConnectGitHubInstallation(ctx context.Context, installation *domain.OrgGitHubInstallation) error
	ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error)
	GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error)
	KeepGitHubUserAuthorization(ctx context.Context, orgID string, installationID int64, authorization string) error
	RenewGitHubUserToken(ctx context.Context, orgID string, installationID int64, renew func(ctx context.Context, authorization string) (accessToken, next string, err error)) (string, error)
	DisconnectGitHubInstallation(ctx context.Context, orgID string, installationID int64) error
}

// ExperimentRepositories says which repository a platform experiment names, which project it is
// in, whether it is closed and whether it is still a draft. *db.PlatformExperimentsStore
// satisfies it.
type ExperimentRepositories interface {
	PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, closed, draft bool, err error)
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

// The reasons a client tells a refusal of a connect or a credential apart by. The others are
// the git host's (github.Reason*).
const (
	ReasonGitHubNotConfigured         = "github_not_configured"
	ReasonInvalidRequest              = "invalid_request"
	ReasonAppNotInstalled             = "app_not_installed"
	ReasonInstallationChoiceRequired  = "installation_choice_required"
	ReasonInstallationNotReachable    = "installation_not_reachable"
	ReasonInstallationNotControlled   = "installation_not_controlled"
	ReasonMembersPermissionNotGranted = "members_permission_not_granted"
	ReasonNoRepository                = "no_repository"
	ReasonRunOver                     = "run_over"
	ReasonForbidden                   = "forbidden"
)

// choiceRequired is the refusal of a connect that named no installation while the person
// reaches several: it carries them, for the client to ask which.
type choiceRequired struct {
	api.Refusal
	Installations []github.Installation `json:"installations"`
}

var errNoApp = api.Refuse(http.StatusServiceUnavailable, ReasonGitHubNotConfigured,
	"this deployment has no GitHub App configured, so GitHub cannot be connected and no repository cloned through it")

// refusal turns what GitHub or the store refused into what the caller is told: the store's
// missing rows here, the rest as the git host answers them (github.Refusal).
func refusal(logger *zap.Logger, op string, err error, gone int) error {
	switch {
	case errors.Is(err, db.ErrOrgNotFound):
		return notFound("org")
	case errors.Is(err, db.ErrGitHubInstallationNotFound):
		return notFound("GitHub installation")
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return notFound("platform experiment")
	}
	return github.Refusal(logger, op, err, gone)
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
// and with several the answer lists them for the client to ask which. The person's grant is
// kept only for an installation on their own account, to create repositories there.
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
	connected, err := r.store.ListGitHubInstallations(ctx, orgID)
	if err != nil {
		return nil, err
	}
	grant, err := app.UserInstallations(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	// On a person's own account GitHub creates a repository for that person alone, so the grant
	// of the one who connects theirs is kept. Any other is ended here.
	chosen, err := chosenInstallation(grant.Installations, connected, installationID)
	if err == nil {
		// GitHub as the App states whether the installation granted every permission it asks for.
		_, err = app.Installation(ctx, chosen.ID)
	}
	if err != nil || chosen.AccountType != github.AccountUser {
		if revokeErr := app.RevokeUser(ctx, grant.AccessToken); revokeErr != nil && err == nil {
			err = revokeErr
		}
	}
	if err != nil {
		return nil, err
	}
	installation := &domain.OrgGitHubInstallation{OrgID: orgID, InstallationID: chosen.ID, AccountLogin: chosen.AccountLogin, AccountType: chosen.AccountType, ConnectedActor: p.Actor()}
	if err := r.store.ConnectGitHubInstallation(ctx, installation); err != nil {
		return nil, err
	}
	if chosen.AccountType == github.AccountUser {
		if err := r.store.KeepGitHubUserAuthorization(ctx, orgID, chosen.ID, grant.Authorization); err != nil {
			return nil, err
		}
	}
	return installation, nil
}

// disconnect takes an installation away from an org. The authorisation kept for one on a
// person's own account is ended at GitHub first, so that none outlives the connection: with
// GitHub not answering, the installation stays connected and the call is made again.
func (r *Repositories) disconnect(ctx context.Context, orgID string, installationID int64) error {
	if _, err := r.access.OrgAdmin(ctx, orgID); err != nil {
		return err
	}
	if r.github != nil {
		accessToken, err := r.store.RenewGitHubUserToken(ctx, orgID, installationID, r.github.UserToken)
		switch {
		case errors.Is(err, github.ErrUserAuthorization):
		case errors.Is(err, db.ErrGitHubAuthorizationUnreadable):
			r.logger.Error("a kept github authorisation cannot be read and is dropped without being ended at GitHub", zap.String("orgID", orgID), zap.Int64("installationID", installationID))
		case err != nil:
			return err
		default:
			if err := r.github.RevokeUser(ctx, accessToken); err != nil && !errors.Is(err, github.ErrUserAuthorization) {
				return err
			}
		}
	}
	return r.store.DisconnectGitHubInstallation(ctx, orgID, installationID)
}

// chosenInstallation is the installation a connect means among those the person reaches: the one
// named, or with none named the only one they control that the org has not connected.
func chosenInstallation(reached []github.Installation, connected []*domain.OrgGitHubInstallation, installationID int64) (*github.Installation, error) {
	notInstalled := invalid(ReasonAppNotInstalled, "Install the GitHub App on the GitHub account to connect, at install_url of GET /github/app, then connect again.")
	if installationID != 0 {
		for i := range reached {
			if reached[i].ID == installationID {
				return &reached[i], controls(reached[i])
			}
		}
		if len(reached) == 0 {
			return nil, notInstalled
		}
		return nil, invalid(ReasonInstallationNotReachable, "Sign in to GitHub as an account that reaches that installation of the GitHub App, then connect again.")
	}
	candidates, hidden := []github.Installation{}, []github.Installation{}
	for _, one := range reached {
		switch {
		case slices.ContainsFunc(connected, func(c *domain.OrgGitHubInstallation) bool { return c.InstallationID == one.ID }):
		case one.Controlled:
			candidates = append(candidates, one)
		case one.MembershipHidden:
			hidden = append(hidden, one)
		}
	}
	switch {
	case len(candidates) == 1:
		return &candidates[0], nil
	case len(candidates) > 1:
		return nil, &choiceRequired{
			Refusal: *api.Refuse(http.StatusConflict, ReasonInstallationChoiceRequired,
				"Choose which GitHub account to connect and send its installation_id."),
			Installations: candidates,
		}
	case len(hidden) > 0:
		return nil, controls(hidden[0])
	}
	return nil, notInstalled
}

// controls refuses an installation the person reaches on an account they do not control, or whose
// organisation GitHub does not say they administer.
func controls(installation github.Installation) error {
	switch {
	case installation.Controlled:
		return nil
	case installation.MembershipHidden:
		return invalid(ReasonMembersPermissionNotGranted, "Accept the GitHub App's request to read the members of "+installation.AccountLogin+" on GitHub, in that organisation's settings under GitHub Apps, then connect again.")
	}
	if installation.AccountType == github.AccountUser {
		return invalid(ReasonInstallationNotControlled, "Sign in to GitHub as "+installation.AccountLogin+" to connect that account.")
	}
	return invalid(ReasonInstallationNotControlled, "Ask an admin of the GitHub organisation "+installation.AccountLogin+" to connect it.")
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
// agent may push once the experiment is past its draft, and a coordinator, a job and the planner
// of a draft read. Nothing is minted for a run that is over, though
// its token still files summaries, since the credential would outlive the token by an hour. The
// close is read again here, so a run closed after the token was read asks nothing of the git host.
func (r *Repositories) credential(ctx context.Context, protocol, host, path string) (*GitCredential, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Unauthenticated("the request carries no principal")
	}
	switch p.Kind {
	case auth.KindAgent, auth.KindCoordinator, auth.KindJob:
	default:
		return nil, api.Refuse(http.StatusForbidden, ReasonForbidden, "a "+string(p.Kind)+" token has no repository of its own: a git credential goes to an agent, a coordinator or a job")
	}
	projectID, repository, closed, draft, err := r.experiments.PlatformExperimentRepository(ctx, p.PlatformExperimentID)
	if err != nil {
		return nil, err
	}
	write := p.Kind == auth.KindAgent && !draft
	if closed {
		return nil, api.Refuse(http.StatusForbidden, ReasonRunOver, "run closed, nothing more is pushed: platform experiment "+p.PlatformExperimentID+" is closed, so no git credential is handed out for it. "+
			"What is not pushed stays out of the repository; the summary and comments still take what matters until the token ends")
	}
	if p.RunOver {
		return nil, api.Refuse(http.StatusForbidden, ReasonRunOver, "run over, nothing more is pushed: the run of this "+string(p.Kind)+" token in platform experiment "+p.PlatformExperimentID+
			" has ended (finished, cut, or for a job its agent's run ended), so no git credential is handed out for it")
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
		Description: "The GitHub addresses a browser needs for an org's repositories. new_repository_url is where a person creates a repository themselves. Send the person to install_url to install the App on a " +
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
			"and which accounts it controls: on an organisation GitHub shows that only through an installation that granted the App the organisation permission Members (read). The person's token is revoked before this call returns, but for an installation on their own account: GitHub creates a repository " +
			"there for its owner alone, so the platform keeps that authorisation, stored sealed, until the installation is disconnected: renewed at each use for up to six months, " +
			"or for as long as GitHub takes it where the GitHub App's user tokens do not expire. " +
			"Reaching an installation, as a collaborator on one of its repositories does, connects nothing. " +
			"redirect_uri is the one the code was issued to. Verify state before calling (see GET /github/app). " +
			"installation_id may be left out when GitHub did not hand one back, as when the App was installed before: the one installation the " +
			"person controls that the org has not connected is connected, with none the answer is 422 app_not_installed and the client sends the person to install_url, " +
			"and with several it is 409 installation_choice_required with installations, a list of " +
			"{installation_id, account_login, account_type} to ask them about, after which they authorise again. Connecting one that is connected answers with it. " +
			"account_type says whether the account is a person's own (User) or an organisation: on an organisation the installation creates a repository, " +
			"on a personal account the owner's kept authorisation does. " +
			"422 invalid_request, code_rejected, app_not_installed (the person controls none the org has not connected, or reaches none), installation_not_reachable (not the one named), " +
			"installation_not_controlled (reached, and on an account that is not theirs to connect) or members_permission_not_granted " +
			"(on an organisation whose installation has not granted Members (read), so GitHub does not say whether the person is its admin). " +
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
			return nil, refusal(r.logger, op, err, http.StatusConflict)
		}
		return &installationsPage{TotalCount: len(installations), Body: pageOf(installations, limit, offset)}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "delete-github-installation", Method: "DELETE", Path: "/orgs/{id}/github-installations/{installation_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Disconnect a GitHub App installation from an org", Tags: []string{"github"},
		Description: "For an owner of the org. No git credential is minted through it from then on: a platform experiment that names a repository of " +
			"it keeps running, its agents and jobs can no longer clone, fetch or push once the credential they hold runs out, within the hour, and " +
			"their next credential call answers 409 installation_not_connected. The authorisation kept for a personal account is ended at GitHub and dropped: " +
			"when GitHub does not answer for that, the installation stays connected and the call is made again. The App stays installed on GitHub: removing it there is the account " +
			"owner's own step. 404 not_found when the org had not connected it.",
	}, func(ctx context.Context, in *struct {
		ID             string `path:"id"`
		InstallationID int64  `path:"installation_id"`
	}) (*struct{}, error) {
		if err := r.disconnect(ctx, in.ID, in.InstallationID); err != nil {
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
			"caller's platform experiment names. An agent's may push once the platform experiment is past its draft; a coordinator's, a job's and a draft's planner's read. `hl git-credential` is git's credential helper " +
			"over this call, so git asks for a fresh one whenever it needs one and nothing has to be stored. protocol, host and path are what git asked " +
			"a credential for: nothing is minted unless they name that repository over https, as owner/name, owner/name.git or its LFS endpoint " +
			"owner/name.git/info/lfs. 403 run_over once the platform experiment is closed or the agent, for a job the agent it belongs to, was cut, and " +
			"403 forbidden for any other kind of token. 404 no_repository when the platform experiment names none, 404 not_found for another address. " +
			"409 installation_not_connected when the org disconnected the installation, repository_not_reachable when the repository left it, " +
			"installation_gone when the App was removed on GitHub, github_permission_missing when its owner has yet to accept the App's permissions, naming each one missing as name:level. " +
			"502 github_failed when GitHub answers 5xx or not at all for the installation, the message naming it and GitHub's answer. " +
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
