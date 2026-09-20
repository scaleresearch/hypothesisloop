package accounts

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

// This file is the one place that says who reaches a project's repository.
//
//   - Whoever sees a project reads which repository it has. Its administrators attach and detach.
//   - Attaching needs more than the platform's say: the person must prove on GitHub, with a code
//     from GitHub's own sign-in, that they can push to the repository. The installation id in the
//     request is a number anyone can guess and proves nothing.
//   - A git credential goes only to what the platform launched into a platform experiment, for
//     the repository of that experiment's project and no other: an agent may push, a coordinator
//     and a job read. A person and an API key get none: they have their own access to GitHub.
//   - A credential is minted when asked for and stored nowhere. It stops being handed out the
//     moment the row behind the token says so: the job ended, the platform experiment closed, the
//     agent was cut, or the repository was detached. One already handed out lives out its hour.

// ExperimentBasePath is the file a platform experiment created with from_repo is read from.
const ExperimentBasePath = ".hypothesisloop/experiment.yaml"

// GitHub is what the platform asks of its GitHub App. *github.Client satisfies it.
type GitHub interface {
	App() github.App
	Repository(ctx context.Context, installationID int64, fullName, userCode string) (*github.Repository, error)
	Token(ctx context.Context, installationID, repositoryID int64, access github.Access) (*github.Token, error)
	File(ctx context.Context, installationID, repositoryID int64, ref, path string) ([]byte, error)
}

// RepoStore is the persistence of project repositories. *db.AccountsStore satisfies it.
type RepoStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	AttachProjectRepo(ctx context.Context, repo *domain.ProjectRepo) error
	GetProjectRepo(ctx context.Context, projectID string) (*domain.ProjectRepo, error)
	DetachProjectRepo(ctx context.Context, projectID string) error
}

// Runs says whether what a derived token names is still at work. auth.StoreDirectory satisfies it.
type Runs interface {
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (tenancy *domain.Tenancy, closedAt *time.Time, err error)
	AgentCutAt(ctx context.Context, platformExperimentID, agentID string) (*time.Time, error)
}

// Repos reaches the repositories attached to projects.
type Repos struct {
	store  RepoStore
	runs   Runs
	github GitHub
	logger *zap.Logger
	access *auth.Access
	policy policy
}

// NewRepos takes the App's client, or nil for a deployment that has no GitHub App.
func NewRepos(store RepoStore, runs Runs, app GitHub, logger *zap.Logger) *Repos {
	if logger == nil {
		panic("accounts.NewRepos: a logger is required")
	}
	return &Repos{store: store, runs: runs, github: app, logger: logger}
}

// RepoView is a project's repository as the API shows it.
type RepoView struct {
	domain.ProjectRepo
	CloneURL string `json:"clone_url" doc:"The HTTPS address agents and jobs clone."`
}

func viewOfRepo(repo *domain.ProjectRepo) RepoView {
	return RepoView{ProjectRepo: *repo, CloneURL: github.CloneURL(repo.Repo)}
}

// GitCredential answers git's credential helper: the fields git reads, and when the password
// stops working.
type GitCredential struct {
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Path      string    `json:"path" doc:"owner/name.git: the one repository the password works on."`
	Username  string    `json:"username"`
	Password  string    `json:"password" doc:"A GitHub installation token minted for this call. It is stored nowhere."`
	ExpiresAt time.Time `json:"expires_at"`
}

var errNoApp = huma.Error503ServiceUnavailable("this deployment has no GitHub App configured, so no repository can be attached or cloned through it")

// refusal turns what GitHub or the store refused into what the caller is told. Anything else is
// the platform's or GitHub's failure: it is logged and answered without its text.
func (r *Repos) refusal(op string, err error) error {
	var status huma.StatusError
	switch {
	case errors.As(err, &status):
		return err
	case errors.Is(err, db.ErrProjectNotFound):
		return huma.Error404NotFound("project not found")
	case errors.Is(err, db.ErrProjectRepoNotFound):
		return huma.Error404NotFound("no repository is attached to this project")
	case errors.Is(err, github.ErrInvalidRepository):
		return huma.Error400BadRequest("repo must be owner/name, as in the repository's address on GitHub")
	case errors.Is(err, github.ErrCodeRejected):
		return huma.Error400BadRequest("GitHub did not accept the code: it works once and for ten minutes, so authorise again and send the new one")
	case errors.Is(err, github.ErrUserCannotPush):
		return auth.Forbidden("forbidden: your GitHub account cannot push to that repository, and agents would")
	case errors.Is(err, github.ErrRepositoryNotInstalled):
		return huma.Error422UnprocessableEntity("the GitHub App is not installed on that repository under that installation, or your GitHub account does not see it")
	case errors.Is(err, github.ErrInstallationGone):
		return huma.Error409Conflict("the GitHub App installation behind the repository was removed or suspended: install the App again and attach the repository")
	case errors.Is(err, github.ErrFileNotFound):
		return huma.Error422UnprocessableEntity("the repository has no " + ExperimentBasePath + " on its default branch")
	case errors.Is(err, github.ErrFileTooLarge):
		return huma.Error422UnprocessableEntity(ExperimentBasePath + " is larger than the platform reads from a repository")
	case errors.Is(err, github.ErrRateLimited):
		r.logger.Warn("github is rate limiting the platform", zap.String("operation", op))
		return huma.Error503ServiceUnavailable("GitHub is rate limiting the platform: try again shortly")
	case errors.Is(err, github.ErrAppCredentials):
		r.logger.Error("github refused the App's credentials", zap.String("operation", op), zap.Error(err))
		return huma.Error502BadGateway("the platform could not authenticate to GitHub")
	default:
		r.logger.Error("repository request failed", zap.String("operation", op), zap.Error(err))
		return huma.Error500InternalServerError(auth.InternalError)
	}
}

// credentialRefusal is refusal for a caller that holds an attached repository: what an attach
// reads as a mistake in the request is here a repository that left the installation.
func (r *Repos) credentialRefusal(op string, err error) error {
	if errors.Is(err, github.ErrRepositoryNotInstalled) {
		return huma.Error409Conflict("the repository is no longer part of the GitHub App installation: add it to the installation again, or attach another")
	}
	return r.refusal(op, err)
}

func (r *Repos) app() (GitHub, error) {
	if r.github == nil {
		return nil, errNoApp
	}
	return r.github, nil
}

// visibleProject admits an account holder to a project it sees. One it does not, whether another
// org's, a sibling of the one its key is bound to, or absent, is not found.
func (r *Repos) visibleProject(ctx context.Context, projectID string) error {
	p, err := r.access.Account(ctx)
	if err != nil {
		return err
	}
	project, err := r.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project == nil || !p.Scope().Admits(domain.Tenancy{OrgID: project.OrgID, ProjectID: project.ID}) {
		return auth.NotFound("project")
	}
	return nil
}

func (r *Repos) administeredProject(ctx context.Context, projectID string) error {
	if _, err := r.access.Account(ctx); err != nil {
		return err
	}
	project, err := r.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	return r.policy.changeProject(ctx, project)
}

func (r *Repos) attach(ctx context.Context, projectID string, installationID int64, repoName, code string) (*RepoView, error) {
	if err := r.administeredProject(ctx, projectID); err != nil {
		return nil, err
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	if installationID <= 0 {
		return nil, huma.Error400BadRequest("installation_id is required: GitHub hands it back after the App is installed")
	}
	if err := github.ValidRepository(repoName); err != nil {
		return nil, err
	}
	if code == "" {
		return nil, huma.Error400BadRequest("code is required: GitHub hands it back after you authorise the App, and it proves you can push to the repository")
	}
	found, err := app.Repository(ctx, installationID, repoName, code)
	if err != nil {
		return nil, err
	}
	repo := &domain.ProjectRepo{
		ProjectID: projectID, InstallationID: installationID, RepositoryID: found.ID, Repo: found.FullName, DefaultBranch: found.DefaultBranch,
	}
	if err := r.store.AttachProjectRepo(ctx, repo); err != nil {
		return nil, err
	}
	view := viewOfRepo(repo)
	return &view, nil
}

// credential mints the caller's git credential. The token already passed the one auth check;
// what is decided here is what remains of rule 14 for something that hands out access for an
// hour: nothing is minted for a run that is over, though its token still files summaries.
func (r *Repos) credential(ctx context.Context) (*GitCredential, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Unauthenticated("the request carries no principal")
	}
	access := github.Read
	switch p.Kind {
	case auth.KindAgent:
		access = github.Write
		_, closedAt, err := r.runs.PlatformExperimentTenancy(ctx, p.PlatformExperimentID)
		if err != nil {
			return nil, err
		}
		if closedAt != nil {
			return nil, auth.Forbidden("forbidden: platform experiment " + p.PlatformExperimentID + " is closed, and no git credential is handed out for it")
		}
		cutAt, err := r.runs.AgentCutAt(ctx, p.PlatformExperimentID, p.AgentID)
		if err != nil {
			return nil, err
		}
		if cutAt != nil {
			return nil, auth.Forbidden("forbidden: agent " + p.AgentID + " was cut, and no git credential is handed out to it")
		}
	case auth.KindCoordinator:
		if p.PlatformExperimentClosed {
			return nil, auth.Forbidden("forbidden: platform experiment " + p.PlatformExperimentID + " is closed, and no git credential is handed out for it")
		}
	case auth.KindJob:
	default:
		return nil, auth.Forbidden("forbidden: a " + string(p.Kind) + " token has no repository of its own: a git credential goes to an agent, a coordinator or a job")
	}
	repo, err := r.store.GetProjectRepo(ctx, p.ProjectID)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, db.ErrProjectRepoNotFound
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	token, err := app.Token(ctx, repo.InstallationID, repo.RepositoryID, access)
	if err != nil {
		return nil, err
	}
	return &GitCredential{
		Protocol: "https", Host: github.Host, Path: repo.Repo + ".git",
		Username: "x-access-token", Password: token.Value, ExpiresAt: token.ExpiresAt,
	}, nil
}

// CloneURL is the address a project's repository is cloned from, empty when none is attached.
func (r *Repos) CloneURL(ctx context.Context, projectID string) (string, error) {
	repo, err := r.store.GetProjectRepo(ctx, projectID)
	if err != nil || repo == nil {
		return "", err
	}
	return github.CloneURL(repo.Repo), nil
}

// ExperimentBase reads ExperimentBasePath at the head of the default branch of a project's
// repository. A refusal the caller can act on is returned as the API answers it.
func (r *Repos) ExperimentBase(ctx context.Context, projectID string) ([]byte, error) {
	const op = "read-experiment-base"
	repo, err := r.store.GetProjectRepo(ctx, projectID)
	if err != nil {
		return nil, r.refusal(op, err)
	}
	if repo == nil {
		return nil, huma.Error422UnprocessableEntity("from_repo needs a repository attached to the project, and this one has none")
	}
	app, err := r.app()
	if err != nil {
		return nil, err
	}
	data, err := app.File(ctx, repo.InstallationID, repo.RepositoryID, "", ExperimentBasePath)
	if err != nil {
		return nil, r.credentialRefusal(op, err)
	}
	return data, nil
}

// RegisterReposHuma registers the repository operations on the shared API document.
func RegisterReposHuma(doc *apidocs.Doc, r *Repos) {
	r.access, r.policy = doc.Access, policy{access: doc.Access}

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-github-app", Method: "GET", Path: "/github/app",
		Summary: "Where to install and authorise the platform's GitHub App", Tags: []string{"repositories"},
		Description: "The two GitHub addresses a browser needs before a repository can be attached. Send the person to install_url to install the App " +
			"on the repositories they choose: GitHub returns them with installation_id. Send them to authorize_url, with redirect_uri and state added, " +
			"to prove who they are: GitHub returns them with code. Answers 503 on a deployment that has no GitHub App.",
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
		OperationID: "attach-project-repo", Method: "POST", Path: "/projects/{id}/repo",
		Summary: "Attach a GitHub repository to a project", Tags: []string{"repositories"},
		Description: "Makes the repository the one this project's agents and jobs clone, in place of any it had. The platform's GitHub App must be installed " +
			"on it under installation_id, and code, from GitHub's authorisation of the App, must belong to a GitHub account that can push to it: " +
			"agents push their branches there, and nobody hands out access they do not hold. The code is used for that one question and no token of " +
			"the person is kept. Agents push branches named hl/<platform experiment>/<agent>; protecting other branches is the repository's own setting.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			InstallationID int64  `json:"installation_id" doc:"The installation GitHub named when the App was installed."`
			Repo           string `json:"repo" doc:"owner/name."`
			Code           string `json:"code" doc:"The code GitHub returned from authorize_url. It works once."`
		}
	}) (*struct{ Body RepoView }, error) {
		view, err := r.attach(ctx, in.ID, in.Body.InstallationID, in.Body.Repo, in.Body.Code)
		if err != nil {
			return nil, r.refusal("attach-project-repo", err)
		}
		return &struct{ Body RepoView }{Body: *view}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-project-repo", Method: "GET", Path: "/projects/{id}/repo",
		Summary: "The repository attached to a project", Tags: []string{"repositories"},
		Description: "What was recorded when the repository was attached. Answers 404 with \"no repository is attached to this project\" when there is none. " +
			"GitHub is not asked: an installation removed since shows when a credential is next minted.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body RepoView }, error) {
		if err := r.visibleProject(ctx, in.ID); err != nil {
			return nil, r.refusal("get-project-repo", err)
		}
		repo, err := r.store.GetProjectRepo(ctx, in.ID)
		if err == nil && repo == nil {
			err = db.ErrProjectRepoNotFound
		}
		if err != nil {
			return nil, r.refusal("get-project-repo", err)
		}
		return &struct{ Body RepoView }{Body: viewOfRepo(repo)}, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "detach-project-repo", Method: "DELETE", Path: "/projects/{id}/repo", DefaultStatus: http.StatusNoContent,
		Summary: "Detach the repository from a project", Tags: []string{"repositories"},
		Description: "No git credential is minted for the project from then on, so agents and jobs of its running platform experiments can no longer clone, " +
			"fetch or push once the credential they hold runs out, within the hour. The App stays installed on GitHub: removing it there is the owner's own step.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		err := r.administeredProject(ctx, in.ID)
		if err == nil {
			err = r.store.DetachProjectRepo(ctx, in.ID)
		}
		if err != nil {
			return nil, r.refusal("detach-project-repo", err)
		}
		return &struct{}{}, nil
	})

	apidocs.Register(doc, apidocs.AudienceJob, huma.Operation{
		OperationID: "get-git-credential", Method: "GET", Path: "/git-credential",
		Summary: "A git credential for your project's repository", Tags: []string{"repositories"},
		Description: "For an agent, a coordinator and a job: a GitHub token minted for this call, good for about an hour and for the one repository attached to " +
			"the caller's project. An agent's may push, a coordinator's and a job's read. `hl git-credential` is git's credential helper over this call, " +
			"so git asks for a fresh one whenever it needs one and nothing has to be stored. Answers 404 when the project has no repository attached, " +
			"and 403 once the platform experiment is closed or the agent cut.",
	}, func(ctx context.Context, _ *struct{}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         GitCredential
	}, error) {
		credential, err := r.credential(ctx)
		if err != nil {
			return nil, r.credentialRefusal("get-git-credential", err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         GitCredential
		}{CacheControl: "no-store", Body: *credential}, nil
	})
}
