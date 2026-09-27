package accounts

import (
	"context"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/github"
)

// GitHubHost is GitHub as the git host: a repository is owner/name, reached through the
// installation of the platform's App that the org of the project connected on its owner.
type GitHubHost struct {
	store  InstallationStore
	github GitHubApp
	logger *zap.Logger
}

var _ githost.Provider = (*GitHubHost)(nil)

// NewGitHubHost takes the App's client, or nil for a deployment that has no GitHub App.
func NewGitHubHost(store InstallationStore, app GitHubApp, logger *zap.Logger) *GitHubHost {
	if store == nil || logger == nil {
		panic("accounts.NewGitHubHost: the store and a logger are required")
	}
	return &GitHubHost{store: store, github: app, logger: logger}
}

func (h *GitHubHost) URL(repository string) string { return github.CloneURL(repository) }

// token mints a token for a repository through the installation the org of the project
// connected on the account that owns it.
func (h *GitHubHost) token(ctx context.Context, projectID, repository string, access github.Access) (*github.Token, error) {
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, notFound("project")
	}
	if h.github == nil {
		return nil, errNoApp
	}
	if err := github.ValidRepository(repository); err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repository, "/")
	installation, err := h.store.GitHubInstallationOfAccount(ctx, project.OrgID, owner)
	if err != nil {
		return nil, err
	}
	if installation == nil {
		return nil, errNotConnected
	}
	return h.github.Token(ctx, installation.InstallationID, repository, access)
}

// Reachable returns a repository as GitHub names it when an installation the org of the project
// connected covers it.
func (h *GitHubHost) Reachable(ctx context.Context, projectID, repository string) (string, error) {
	token, err := h.token(ctx, projectID, repository, github.Read)
	if err != nil {
		return "", refusal(h.logger, "check-repository", err, http.StatusUnprocessableEntity)
	}
	return token.Repository, nil
}

// Create makes a private repository on the GitHub account named, one the org of the project
// connected, or with none named on the only one it connected: with several, which one is the
// request's to say. On a deployment that has no GitHub App it makes none and returns nothing:
// there a platform experiment needs no repository.
func (h *GitHubHost) Create(ctx context.Context, projectID, account, name, description string) (string, error) {
	const op = "create-repository"
	if h.github == nil {
		return "", nil
	}
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return "", refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if project == nil {
		return "", notFound("project")
	}
	installations, err := h.store.ListGitHubInstallations(ctx, project.OrgID)
	if err != nil {
		return "", refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if len(installations) == 0 {
		return "", refusal(h.logger, op, errNoInstallation, http.StatusUnprocessableEntity)
	}
	var chosen *domain.OrgGitHubInstallation
	logins := []string{}
	for _, one := range installations {
		logins = append(logins, one.AccountLogin)
		if strings.EqualFold(one.AccountLogin, account) || (account == "" && len(installations) == 1) {
			chosen = one
		}
	}
	switch {
	case chosen == nil && account == "":
		return "", invalid(ReasonRepositoryAccountRequired, "the org has connected several GitHub accounts ("+strings.Join(logins, ", ")+"): say in repository_account which one the new repository is created on, or name a repository")
	case chosen == nil:
		return "", refusal(h.logger, op, errNotConnected, http.StatusUnprocessableEntity)
	}
	created, err := h.github.CreateRepository(ctx, chosen.InstallationID, name, description)
	if err != nil {
		return "", refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	h.logger.Info("repository created for a platform experiment", zap.String("repository", created.FullName), zap.String("orgID", project.OrgID))
	return created.FullName, nil
}

func (h *GitHubHost) File(ctx context.Context, projectID, repository, path string) ([]byte, error) {
	const op = "read-repository-file"
	token, err := h.token(ctx, projectID, repository, github.Read)
	if err != nil {
		return nil, refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	data, err := h.github.File(ctx, token, "", path)
	if err != nil {
		return nil, refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	return data, nil
}

// Credential mints an installation token for the one repository. The repository is one a
// platform experiment already names, so one the org no longer reaches is a conflict.
func (h *GitHubHost) Credential(ctx context.Context, projectID, repository string, write bool) (*githost.Credential, error) {
	access := github.Read
	if write {
		access = github.Write
	}
	token, err := h.token(ctx, projectID, repository, access)
	if err != nil {
		return nil, refusal(h.logger, "create-git-credential", err, http.StatusConflict)
	}
	return &githost.Credential{Username: "x-access-token", Password: token.Value, ExpiresAt: token.ExpiresAt}, nil
}

// Serves takes owner/name with or without .git, or its LFS endpoint under it, in any casing.
func (h *GitHubHost) Serves(repository, protocol, host, path string) bool {
	path = strings.TrimSuffix(strings.Trim(path, "/"), "/info/lfs")
	return protocol == "https" && strings.EqualFold(host, github.Host) && strings.EqualFold(strings.TrimSuffix(path, ".git"), repository)
}
