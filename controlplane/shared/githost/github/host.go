package github

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

// AppClient is what Host asks of the platform's GitHub App. *Client satisfies it.
type AppClient interface {
	Token(ctx context.Context, installationID int64, fullName string, access Access) (*Token, error)
	File(ctx context.Context, token *Token, ref, path string) ([]byte, error)
	CreateRepository(ctx context.Context, installationID int64, name, description string, files map[string]string) (*Repository, error)
	StartFresh(ctx context.Context, token *Token, files map[string]string) error
	Installation(ctx context.Context, installationID int64) (*Installation, error)
}

// InstallationStore is what Host reads of the installations orgs connected.
// *db.GitHubInstallationsStore satisfies it.
type InstallationStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error)
	GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error)
}

// Host is GitHub as the git host: a repository is owner/name, reached through the installation
// of the platform's App that the org of the project connected on its owner.
type Host struct {
	store  InstallationStore
	app    AppClient
	logger *zap.Logger
}

var _ githost.Provider = (*Host)(nil)

func NewHost(store InstallationStore, app AppClient, logger *zap.Logger) *Host {
	if store == nil || app == nil || logger == nil {
		panic("github.NewHost: the store, the App's client and a logger are required")
	}
	return &Host{store: store, app: app, logger: logger}
}

func (h *Host) URL(repository string) string { return CloneURL(repository) }

func notFound(what string) *api.Refusal {
	return api.Refuse(http.StatusNotFound, "not_found", what+" not found")
}

func invalid(reason, message string) *api.Refusal {
	return api.Refuse(http.StatusUnprocessableEntity, reason, message)
}

// token mints a token for a repository through the installation the org of the project
// connected on the account that owns it.
func (h *Host) token(ctx context.Context, projectID, repository string, access Access) (*Token, error) {
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, notFound("project")
	}
	if err := ValidRepository(repository); err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repository, "/")
	installation, err := h.store.GitHubInstallationOfAccount(ctx, project.OrgID, owner)
	if err != nil {
		return nil, err
	}
	if installation == nil {
		return nil, ErrNotConnected
	}
	token, err := h.app.Token(ctx, installation.InstallationID, repository, access)
	if errors.Is(err, ErrInstallationGone) {
		return nil, &InstallationGoneError{Account: installation.AccountLogin, Installation: installation.InstallationID}
	}
	return token, err
}

// Reachable returns a repository as GitHub names it when an installation the org of the project
// connected covers it and lets the platform push to it, as the experiment's agents do.
func (h *Host) Reachable(ctx context.Context, projectID, repository string) (string, error) {
	token, err := h.token(ctx, projectID, repository, Write)
	if err != nil {
		return "", Refusal(h.logger, "check-repository", err, http.StatusUnprocessableEntity)
	}
	return token.Repository, nil
}

// Create makes a private repository on the GitHub account named, one the org of the project
// connected, or with none named on the org's only or default account. files are its first
// commit.
func (h *Host) Create(ctx context.Context, projectID, account, name, description string, files map[string]string) (string, error) {
	const op = "create-repository"
	orgID, chosen, err := h.createsOn(ctx, projectID, account)
	if err != nil {
		return "", err
	}
	created, err := h.app.CreateRepository(ctx, chosen.InstallationID, name, description, files)
	if errors.Is(err, ErrRepositoryExists) {
		if adopted, ours := h.madeHere(ctx, chosen, name, files); ours {
			h.logger.Info("repository found as created before, adopted", zap.String("repository", adopted), zap.String("orgID", orgID))
			return adopted, nil
		}
	}
	if err != nil {
		return "", Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
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
func (h *Host) madeHere(ctx context.Context, chosen *domain.OrgGitHubInstallation, name string, files map[string]string) (string, bool) {
	token, err := h.app.Token(ctx, chosen.InstallationID, chosen.AccountLogin+"/"+name, Write)
	if err != nil {
		return "", false
	}
	data, err := h.app.File(ctx, token, "", domain.ExperimentFile)
	switch {
	case err == nil:
		return token.Repository, string(data) == files[domain.ExperimentFile]
	case errors.Is(err, ErrFileNotFound):
		if err := h.app.StartFresh(ctx, token, files); err != nil {
			h.logger.Warn("a repository of the experiment's name is not the one created for it", zap.String("repository", token.Repository), zap.Error(err))
			return "", false
		}
		return token.Repository, true
	}
	return "", false
}

// CreatesOn is what Create checks before it creates: what passes here is created unless GitHub
// refuses the creation itself.
func (h *Host) CreatesOn(ctx context.Context, projectID, account string) (string, error) {
	_, chosen, err := h.createsOn(ctx, projectID, account)
	if err != nil {
		return "", err
	}
	return chosen.AccountLogin, nil
}

// createsOn is the org of the project and the installation a repository is created through: the
// one on account, or with none named the org's only one or its default. It is refused when GitHub
// removed it or it has not granted every permission the App asks for.
func (h *Host) createsOn(ctx context.Context, projectID, account string) (string, *domain.OrgGitHubInstallation, error) {
	const op = "create-repository"
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if project == nil {
		return "", nil, notFound("project")
	}
	installations, err := h.store.ListGitHubInstallations(ctx, project.OrgID)
	if err != nil {
		return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if len(installations) == 0 {
		return "", nil, Refusal(h.logger, op, ErrNoInstallation, http.StatusUnprocessableEntity)
	}
	var chosen *domain.OrgGitHubInstallation
	logins := []string{}
	for _, one := range installations {
		logins = append(logins, one.AccountLogin)
		switch {
		case account != "" && strings.EqualFold(one.AccountLogin, account),
			account == "" && (len(installations) == 1 || one.Default):
			chosen = one
		}
	}
	switch {
	case chosen == nil && account == "":
		return "", nil, invalid(ReasonRepositoryAccountRequired, "the org has connected several GitHub accounts ("+strings.Join(logins, ", ")+") and made none the default for new repositories: say in repository_account which one the new repository is created on, or an owner of the org makes one the default in Settings, GitHub, or name a repository")
	case chosen == nil:
		return "", nil, Refusal(h.logger, op, ErrNotConnected, http.StatusUnprocessableEntity)
	}
	if _, err := Installed(ctx, h.app, chosen); err != nil {
		return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	return project.OrgID, chosen, nil
}

func (h *Host) File(ctx context.Context, projectID, repository, path string) ([]byte, error) {
	const op = "read-repository-file"
	token, err := h.token(ctx, projectID, repository, Read)
	if err != nil {
		return nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	data, err := h.app.File(ctx, token, "", path)
	if err != nil {
		return nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	return data, nil
}

func (h *Host) FileAt(ctx context.Context, projectID, repository, ref, path string) ([]byte, error) {
	const op = "read-repository-file"
	token, err := h.token(ctx, projectID, repository, Read)
	if err != nil {
		return nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	data, err := h.app.File(ctx, token, ref, path)
	switch {
	case errors.Is(err, ErrFileNotFound):
		return nil, githost.ErrFileNotFound
	case errors.Is(err, ErrNotAFile):
		return nil, invalid(ReasonUnreadableFile, path+" at "+ref+" is not a file")
	case errors.Is(err, ErrFileTooLarge):
		return nil, invalid(ReasonUnreadableFile, path+" at "+ref+" is larger than the platform reads from a repository")
	case err != nil:
		return nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	return data, nil
}

// Credential mints an installation token for the one repository. The repository is one a
// platform experiment already names, so one the org no longer reaches is a conflict.
func (h *Host) Credential(ctx context.Context, projectID, repository string, write bool) (*githost.Credential, error) {
	access := Read
	if write {
		access = Write
	}
	token, err := h.token(ctx, projectID, repository, access)
	if err != nil {
		return nil, Refusal(h.logger, "create-git-credential", err, http.StatusConflict)
	}
	return &githost.Credential{Username: "x-access-token", Password: token.Value, ExpiresAt: token.ExpiresAt}, nil
}

// Serves takes owner/name with or without .git, or its LFS endpoint under it, in any casing.
func (h *Host) Serves(repository, protocol, host, path string) bool {
	path = strings.TrimSuffix(strings.Trim(path, "/"), "/info/lfs")
	return protocol == "https" && strings.EqualFold(host, gitHost) && strings.EqualFold(strings.TrimSuffix(path, ".git"), repository)
}

// Installed is a connected installation as GitHub states it now: InstallationGoneError when GitHub
// removed or suspended it, and PermissionMissingError when it has not granted every permission
// the App asks for.
func Installed(ctx context.Context, app interface {
	Installation(ctx context.Context, installationID int64) (*Installation, error)
}, connected *domain.OrgGitHubInstallation) (*Installation, error) {
	live, err := app.Installation(ctx, connected.InstallationID)
	if errors.Is(err, ErrInstallationGone) {
		return nil, &InstallationGoneError{Account: connected.AccountLogin, Installation: connected.InstallationID, Suspended: errors.Is(err, ErrInstallationSuspended)}
	}
	return live, err
}
