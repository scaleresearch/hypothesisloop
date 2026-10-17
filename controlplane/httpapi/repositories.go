package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

type installationsPage struct {
	TotalCount int `header:"X-Total-Count" doc:"The installations the org connected, ignoring limit and offset."`
	Body       []*domain.OrgGitHubInstallation
}

type repositoriesPage struct {
	TotalCount int `header:"X-Total-Count" doc:"The repositories that match, ignoring limit and offset."`
	Body       []accounts.Repository
}

type repositoriesAPI struct {
	repositories *accounts.Repositories
}

// registerRepositories registers the GitHub operations on the shared API document.
func registerRepositories(doc *Doc, repositories *accounts.Repositories) {
	a := repositoriesAPI{repositories: repositories}
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "get-github-app", Method: "GET", Path: "/github/app",
		Summary: "The platform's GitHub App", Tags: []string{"github"},
		Description: "The GitHub addresses a browser needs for an org's repositories. new_repository_url is where a person creates a repository themselves. " +
			"install_url is the App's install page; connecting an installation starts at POST /orgs/{id}/github-install-states, whose install_url carries the state. " +
			"Answers 503 github_not_configured on a deployment that has no GitHub App.",
	}, a.app)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-github-install-state", Method: "POST", Path: "/orgs/{id}/github-install-states", DefaultStatus: http.StatusCreated,
		Summary: "Start installing the platform's GitHub App for an org", Tags: []string{"github"},
		Description: "For an owner of the org, as a user or an API key. Answers a state good once, for the caller, for 15 minutes, and install_url, GitHub's install page of the App carrying it. " +
			"The person installs the App there on their own GitHub account or an organisation, or changes the repositories of an installation they made before. " +
			"GitHub then sends them to the App's setup URL with state, installation_id and setup_action, and POST /github/installations connects the installation. " +
			"return_to is the page of the UI the install starts from, handed back when it finishes. 422 invalid_request when it is not a path of the UI. 503 github_not_configured.",
	}, a.beginInstall)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-github-installation", Method: "POST", Path: "/github/installations", DefaultStatus: http.StatusCreated,
		Summary: "Connect the GitHub App installation an install made or changed", Tags: []string{"github"},
		Description: "For the user or API key that started the install, while still an owner of its org. state and installation_id are what GitHub handed back to the setup URL. " +
			"The state is taken once whatever the outcome. The installation is asked of GitHub as the App: it must exist, not be suspended, have granted every permission the App asks for, " +
			"and have been made or changed after the state was issued. An installation the org connected before is answered as it is; a new one is connected, and is the org's default for new repositories when it is the org's first. " +
			"account_type says whether the account is a person's own (User) or an organisation. " +
			"422 install_state_invalid (made up, used, expired or another person's), installation_not_new (last changed before the install started), installation_gone, installation_suspended, " +
			"github_permission_missing naming each permission missing as name:level. 502 github_failed, 503 github_not_configured.",
	}, a.finishInstall)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-installations", Method: "GET", Path: "/orgs/{id}/github-installations",
		Summary: "List the GitHub App installations an org connected", Tags: []string{"github"},
		Description: "For any member of the org, oldest first. What was recorded at connect: GitHub is not asked, so an installation removed there " +
			"since still shows, and a platform experiment that names a repository of it is refused its git credential. " +
			"account_type is User for a person's own account and Organization, and absent on an installation connected before " +
			"the type was kept, until it is connected again.",
	}, a.installations)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "delete-github-installation", Method: "DELETE", Path: "/orgs/{id}/github-installations/{installation_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Disconnect a GitHub App installation from an org", Tags: []string{"github"},
		Description: "For an owner of the org. No git credential is minted through it from then on: a platform experiment that names a repository of " +
			"it keeps running, its agents and jobs can no longer clone, fetch or push once the credential they hold runs out, within the hour, and " +
			"their next credential call answers 409 installation_not_connected. The App stays installed on GitHub: removing it there is the account " +
			"owner's own step. 404 not_found when the org had not connected it.",
	}, a.disconnect)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "make-github-installation-default", Method: "PUT", Path: "/orgs/{id}/github-installations/{installation_id}/default", DefaultStatus: http.StatusNoContent,
		Summary: "Make a GitHub installation where an org's new repositories are created", Tags: []string{"github"},
		Description: "For an owner of the org. A platform experiment or draft that names neither repository nor repository_account has its repository created on this installation's account, " +
			"in place of the default before. With one installation connected that one is used whether or not it is the default; with several and none the default, " +
			"such a create is refused 422 repository_account_required. Disconnecting the default leaves none. 404 not_found when the org has not connected it.",
	}, a.makeDefault)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "check-github-installation", Method: "GET", Path: "/orgs/{id}/github-installations/{installation_id}/check", DefaultStatus: http.StatusNoContent,
		Summary: "Check with GitHub that a connected installation still works", Tags: []string{"github"},
		Description: "For any member of the org. Asks GitHub now whether the App is still installed on the account and has granted every permission the App asks for (app-manifest.json), " +
			"the check a create runs on the account it creates on. 204 when it has. 422 installation_gone when GitHub removed it, installation_suspended when its owner suspended it, github_permission_missing naming what it has not granted, " +
			"each with what to do. 404 not_found when the org has not connected it. 502 github_failed, 503 github_not_configured.",
	}, a.check)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-repositories", Method: "GET", Path: "/orgs/{id}/github-repositories",
		Summary: "List the repositories an org's GitHub installations cover", Tags: []string{"github"},
		Description: "For any member of the org: what a platform experiment of the org may name as its repository, sorted by name. q keeps the " +
			"repositories whose owner/name contains it, whatever the casing. Nothing is kept between calls: each one asks GitHub for every " +
			"repository of every connected installation, a hundred to a request, and pages the result here, so X-Total-Count is exact and a call " +
			"costs as many GitHub requests as the installations hold hundreds of repositories. An installation removed on GitHub is left out. " +
			"502 github_failed when GitHub answers 5xx or not at all for an installation, the message naming it and GitHub's answer. " +
			"503 github_not_configured.",
	}, a.repositoriesOf)
}

func (a repositoriesAPI) app(ctx context.Context, _ *struct{}) (*struct{ Body accounts.GitHubAddresses }, error) {
	app, err := a.repositories.App(ctx)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.GitHubAddresses }{Body: app}, nil
}

func (a repositoriesAPI) beginInstall(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		ReturnTo string `json:"return_to" doc:"A path of the UI, as /settings."`
	}
}) (*struct{ Body *accounts.InstallStart }, error) {
	started, err := a.repositories.BeginInstall(ctx, in.ID, in.Body.ReturnTo)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *accounts.InstallStart }{Body: started}, nil
}

func (a repositoriesAPI) finishInstall(ctx context.Context, in *struct {
	Body struct {
		State          string `json:"state" doc:"The state of POST /orgs/{id}/github-install-states, as GitHub handed it back."`
		InstallationID int64  `json:"installation_id" doc:"The installation GitHub handed back."`
	}
}) (*struct{ Body *accounts.InstallFinished }, error) {
	finished, err := a.repositories.FinishInstall(ctx, in.Body.State, in.Body.InstallationID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *accounts.InstallFinished }{Body: finished}, nil
}

func (a repositoriesAPI) installations(ctx context.Context, in *struct {
	ID string `path:"id"`
	accounts.Page
}) (*installationsPage, error) {
	installations, total, err := a.repositories.Installations(ctx, in.ID, in.Page)
	if err != nil {
		return nil, err
	}
	return &installationsPage{TotalCount: total, Body: installations}, nil
}

func (a repositoriesAPI) disconnect(ctx context.Context, in *struct {
	ID             string `path:"id"`
	InstallationID int64  `path:"installation_id"`
}) (*struct{}, error) {
	if err := a.repositories.Disconnect(ctx, in.ID, in.InstallationID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (a repositoriesAPI) makeDefault(ctx context.Context, in *struct {
	ID             string `path:"id"`
	InstallationID int64  `path:"installation_id"`
}) (*struct{}, error) {
	if err := a.repositories.MakeDefault(ctx, in.ID, in.InstallationID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (a repositoriesAPI) check(ctx context.Context, in *struct {
	ID             string `path:"id"`
	InstallationID int64  `path:"installation_id"`
}) (*struct{}, error) {
	if err := a.repositories.Check(ctx, in.ID, in.InstallationID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (a repositoriesAPI) repositoriesOf(ctx context.Context, in *struct {
	ID string `path:"id"`
	Q  string `query:"q" doc:"Text the repository's owner/name must contain."`
	accounts.Page
}) (*repositoriesPage, error) {
	found, total, err := a.repositories.List(ctx, in.ID, in.Q, in.Page)
	if err != nil {
		return nil, err
	}
	return &repositoriesPage{TotalCount: total, Body: found}, nil
}
