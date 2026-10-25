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
		Summary: "Start connecting the platform's GitHub App to an org", Tags: []string{"github"},
		Description: "For an owner of the org, as a user or an API key. Answers a state good once, for the caller, for 15 minutes, and install_url, GitHub's authorization page of the App carrying it. " +
			"The person authorizes the App there and GitHub sends them to the App's callback URL with state and code, which POST /github/installations takes without installation_id: " +
			"it connects the installations of the App the person may reach on GitHub, or, with none, answers GitHub's install page with a new state. " +
			"There the person installs the App on their own GitHub account or an organisation; GitHub has them authorize it and returns to the callback URL with state, code, installation_id and setup_action, which POST /github/installations connects. " +
			"return_to is the page of the UI the install starts from, handed back when it finishes. 422 invalid_request when it is not a path of the UI. 503 github_not_configured.",
	}, a.beginInstall)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "create-github-installation", Method: "POST", Path: "/github/installations", DefaultStatus: http.StatusCreated,
		Summary: "Connect the GitHub App installations a person authorized or installed", Tags: []string{"github"},
		Description: "For the user or API key that started the connect, while still an owner of its org. state, code and installation_id are what GitHub handed back to the callback URL. " +
			"The state is taken once whatever the outcome. code is exchanged with GitHub for the token of the GitHub user who returned, and the installation must be one GitHub lists for that user; " +
			"the token is used for nothing else and kept nowhere. The installation is then asked of GitHub as the App: it must exist, not be suspended and have granted every permission the App asks for. " +
			"An installation the org connected before is answered as it is; a new one is connected, and is the org's default for new repositories when it is the org's first. " +
			"Without installation_id, as GitHub returns from its authorization page, every installation GitHub lists for that user that the org has not connected is connected, and installation is the first; " +
			"one GitHub refuses is left out, and is the refusal when every one is. With none to connect, nothing is and install holds GitHub's install page of the App with a new state, for the person to install it there. " +
			"account_type says whether the account is a person's own (User) or an organisation. " +
			"422 install_state_invalid (made up, used, expired or another person's), github_authorization_invalid (code used or expired), installation_not_authorized (not one the GitHub user may reach), " +
			"installation_gone, installation_suspended, git_host_permission_missing naming each permission missing as name:level. 502 git_host_failed, 503 github_not_configured.",
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
			"their next credential call answers 409 installation_not_connected. When no other org connected it, the App is first removed from the GitHub account, " +
			"so it no longer reaches the account's repositories; one GitHub had removed already counts as removed. Should GitHub fail that, nothing changes and the answer is 502 github_uninstall_failed. " +
			"When it was the org's default, the oldest remaining installation becomes the default. 404 not_found when the org had not connected it.",
	}, a.disconnect)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "make-github-installation-default", Method: "PUT", Path: "/orgs/{id}/github-installations/{installation_id}/default", DefaultStatus: http.StatusNoContent,
		Summary: "Make a GitHub installation where an org's new repositories are created", Tags: []string{"github"},
		Description: "For an owner of the org. A platform experiment or draft that names neither repository nor repository_account has its repository created on this installation's account, " +
			"in place of the default before. The org's first installation is its default, and disconnecting the default makes the oldest remaining one the default. " +
			"Such a create is refused 422 repository_account_required only when the default is removed or lacks a permission and several others work. 404 not_found when the org has not connected it.",
	}, a.makeDefault)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "check-github-installation", Method: "GET", Path: "/orgs/{id}/github-installations/{installation_id}/check",
		Summary: "Check with GitHub that a connected installation still works", Tags: []string{"github"},
		Description: "For any member of the org. Asks GitHub now whether the App is still installed on the account and has granted every permission the App asks for (app-manifest.json), " +
			"the check a create runs on the account it creates on. What GitHub says is the answer: working, or reason installation_gone when GitHub removed it, installation_suspended when its owner suspended it, " +
			"git_host_permission_missing naming what it has not granted, each with what to do in message, where a create on the account is refused 422 with that reason. " +
			"404 not_found when the org has not connected it. 502 git_host_failed, 503 github_not_configured.",
	}, a.check)
	register(doc, auth.AudienceCoordinator, huma.Operation{
		OperationID: "list-github-repositories", Method: "GET", Path: "/orgs/{id}/github-repositories",
		Summary: "List the repositories an org's GitHub installations cover", Tags: []string{"github"},
		Description: "For any member of the org: what a platform experiment of the org may name as its repository, sorted by name. q keeps the " +
			"repositories whose owner/name contains it, whatever the casing. Nothing is kept between calls: each one asks GitHub for every " +
			"repository of every connected installation, a hundred to a request, and pages the result here, so X-Total-Count is exact and a call " +
			"costs as many GitHub requests as the installations hold hundreds of repositories. An installation removed on GitHub is left out. " +
			"502 git_host_failed when GitHub answers 5xx or not at all for an installation, the message naming it and GitHub's answer. " +
			"503 github_not_configured.",
	}, a.repositoriesOf)
}

func (a repositoriesAPI) app(ctx context.Context, _ *struct{}) (*struct{ Body domain.GitHubAddresses }, error) {
	app, err := a.repositories.App(ctx)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.GitHubAddresses }{Body: app}, nil
}

type beginInstallRequest struct {
	ReturnTo string `json:"return_to" doc:"A path of the UI, as /settings."`
}

func (a repositoriesAPI) beginInstall(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body beginInstallRequest
}) (*struct{ Body *accounts.InstallStart }, error) {
	started, err := a.repositories.BeginInstall(ctx, in.ID, in.Body.ReturnTo)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *accounts.InstallStart }{Body: started}, nil
}

type finishInstallRequest struct {
	State          string `json:"state" doc:"The state of POST /orgs/{id}/github-install-states, as GitHub handed it back."`
	Code           string `json:"code" doc:"The code GitHub handed back for the GitHub user who authorized the App during the install."`
	InstallationID int64  `json:"installation_id" doc:"The installation GitHub handed back."`
}

func (a repositoriesAPI) finishInstall(ctx context.Context, in *struct {
	Body finishInstallRequest
}) (*struct{ Body *accounts.InstallFinished }, error) {
	finished, err := a.repositories.FinishInstall(ctx, in.Body.State, in.Body.Code, in.Body.InstallationID)
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
}) (*struct{ Body accounts.InstallationCheck }, error) {
	check, err := a.repositories.Check(ctx, in.ID, in.InstallationID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.InstallationCheck }{Body: check}, nil
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
