package accounts

import (
	"context"
	"errors"
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

func NewGitHubHost(store InstallationStore, app GitHubApp, logger *zap.Logger) *GitHubHost {
	if store == nil || app == nil || logger == nil {
		panic("accounts.NewGitHubHost: the store, the App's client and a logger are required")
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
// connected, or with none named on the only account it connected: with several, which one is
// the request's to say. files are its first commit. On a person's own account it is created
// with that person's token, renewed from the authorisation kept when they connected it.
func (h *GitHubHost) Create(ctx context.Context, projectID, account, name, description string, files map[string]string) (string, error) {
	const op = "create-repository"
	orgID, chosen, err := h.createsOn(ctx, projectID, account)
	if err != nil {
		return "", err
	}
	userToken := ""
	if chosen.AccountType == github.AccountUser {
		if userToken, err = h.store.RenewGitHubUserToken(ctx, orgID, chosen.InstallationID, h.github.UserToken); err != nil {
			return "", refusal(h.logger, op, err, http.StatusUnprocessableEntity)
		}
	}
	created, err := h.github.CreateRepository(ctx, chosen.InstallationID, userToken, name, description, files)
	if errors.Is(err, github.ErrRepositoryExists) {
		if adopted, ours := h.madeHere(ctx, chosen, name, files); ours {
			h.logger.Info("repository found as created before, adopted", zap.String("repository", adopted), zap.String("orgID", orgID))
			return adopted, nil
		}
	}
	if err != nil {
		return "", refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	h.logger.Info("repository created for a platform experiment", zap.String("repository", created.FullName), zap.String("orgID", orgID))
	return created.FullName, nil
}

// madeHere says the repository of that name on the chosen account is the one an earlier Create
// with the same files made, which a restart left unrecorded, and returns it as GitHub names it.
// The installation reaches it, and it holds the template's experiment file or nothing but the
// commit GitHub made at its creation, the first commit not written yet: then files are written
// into it now. The name carries the platform experiment's id, so such a repository is no one
// else's.
func (h *GitHubHost) madeHere(ctx context.Context, chosen *domain.OrgGitHubInstallation, name string, files map[string]string) (string, bool) {
	token, err := h.github.Token(ctx, chosen.InstallationID, chosen.AccountLogin+"/"+name, github.Write)
	if err != nil {
		return "", false
	}
	data, err := h.github.File(ctx, token, "", domain.ExperimentFile)
	switch {
	case err == nil:
		return token.Repository, string(data) == files[domain.ExperimentFile]
	case errors.Is(err, github.ErrFileNotFound):
		if err := h.github.StartFresh(ctx, token, files); err != nil {
			h.logger.Warn("a repository of the experiment's name is not the one created for it", zap.String("repository", token.Repository), zap.Error(err))
			return "", false
		}
		return token.Repository, true
	}
	return "", false
}

func (h *GitHubHost) CreatesOn(ctx context.Context, projectID, account string) (string, error) {
	_, chosen, err := h.createsOn(ctx, projectID, account)
	if err != nil {
		return "", err
	}
	return chosen.AccountLogin, nil
}

// createsOn is the org of the project and the installation a repository is created through: the
// one on account, or with none named the only one the org connected.
func (h *GitHubHost) createsOn(ctx context.Context, projectID, account string) (string, *domain.OrgGitHubInstallation, error) {
	const op = "create-repository"
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return "", nil, refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if project == nil {
		return "", nil, notFound("project")
	}
	installations, err := h.store.ListGitHubInstallations(ctx, project.OrgID)
	if err != nil {
		return "", nil, refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if len(installations) == 0 {
		return "", nil, refusal(h.logger, op, errNoInstallation, http.StatusUnprocessableEntity)
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
		return "", nil, invalid(ReasonRepositoryAccountRequired, "the org has connected several GitHub accounts ("+strings.Join(logins, ", ")+"): say in repository_account which one the new repository is created on, or name a repository")
	case chosen == nil:
		return "", nil, refusal(h.logger, op, errNotConnected, http.StatusUnprocessableEntity)
	}
	return project.OrgID, chosen, nil
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

func (h *GitHubHost) FileAt(ctx context.Context, projectID, repository, ref, path string) ([]byte, error) {
	const op = "read-repository-file"
	token, err := h.token(ctx, projectID, repository, github.Read)
	if err != nil {
		return nil, refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	data, err := h.github.File(ctx, token, ref, path)
	switch {
	case errors.Is(err, github.ErrFileNotFound):
		return nil, githost.ErrFileNotFound
	case errors.Is(err, github.ErrNotAFile):
		return nil, invalid(ReasonUnreadableFile, path+" at "+ref+" is not a file")
	case errors.Is(err, github.ErrFileTooLarge):
		return nil, invalid(ReasonUnreadableFile, path+" at "+ref+" is larger than the platform reads from a repository")
	case err != nil:
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
