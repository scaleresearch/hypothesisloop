package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// AppClient is what Host asks of the platform's GitHub App. *Client satisfies it.
type AppClient interface {
	Token(ctx context.Context, installationID int64, fullName string, access Access) (*Token, error)
	File(ctx context.Context, token *Token, ref, path string) ([]byte, error)
	CreateFromTemplate(ctx context.Context, installationID int64, template, name, description string) (*Repository, error)
	HasBranch(ctx context.Context, token *Token, branch string) (bool, error)
	Installation(ctx context.Context, installationID int64) (*Installation, error)
}

// InstallationStore is what Host reads of the installations orgs connected.
// *db.Store satisfies it.
type InstallationStore interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
	ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error)
	GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error)
}

// Host is GitHub as the git host: a repository is owner/name, reached through the installation
// of the platform's App that the org of the project connected on its owner. template is the
// public template repository every repository it creates is generated from.
type Host struct {
	store    InstallationStore
	app      AppClient
	template string
	logger   *zap.Logger
}

var _ githost.Provider = (*Host)(nil)

func NewHost(store InstallationStore, app AppClient, template string, logger *zap.Logger) *Host {
	if store == nil || app == nil || template == "" || logger == nil {
		panic("github.NewHost: the store, the App's client, the experiment template and a logger are required")
	}
	return &Host{store: store, app: app, template: template, logger: logger}
}

func (h *Host) URL(repository string) string { return CloneURL(repository) }

// token mints a token for a repository through the installation the org of the project
// connected on the account that owns it.
func (h *Host) token(ctx context.Context, projectID, repository string, access Access) (*Token, error) {
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, refusal.NotFound("project")
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

// Create generates the repository, owner/name, from the experiment template on owner, a GitHub
// account the org of the project connected, and returns it as GitHub names it.
func (h *Host) Create(ctx context.Context, projectID, repository, description string) (string, error) {
	const op = "create-repository"
	if err := ValidRepository(repository); err != nil {
		return "", Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	owner, name, _ := strings.Cut(repository, "/")
	orgID, chosen, err := h.createsOn(ctx, projectID, owner)
	if err != nil {
		return "", err
	}
	created, err := h.app.CreateFromTemplate(ctx, chosen.InstallationID, h.template, name, description)
	if err != nil {
		return "", Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	h.logger.Info("repository created from the experiment template", zap.String("repository", created.FullName), zap.String("orgID", orgID))
	return created.FullName, nil
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
// one on account, or with none named the org's default or else its only working one. It is refused
// when GitHub removed it or it has not granted every permission the App asks for.
func (h *Host) createsOn(ctx context.Context, projectID, account string) (string, *domain.OrgGitHubInstallation, error) {
	const op = "create-repository"
	project, err := h.store.GetProject(ctx, projectID)
	if err != nil {
		return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if project == nil {
		return "", nil, refusal.NotFound("project")
	}
	installations, err := h.store.ListGitHubInstallations(ctx, project.OrgID)
	if err != nil {
		return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	if len(installations) == 0 {
		return "", nil, Refusal(h.logger, op, ErrNoInstallation, http.StatusUnprocessableEntity)
	}
	if account != "" {
		i := slices.IndexFunc(installations, func(one *domain.OrgGitHubInstallation) bool { return strings.EqualFold(one.AccountLogin, account) })
		if i < 0 {
			return "", nil, Refusal(h.logger, op, ErrNotConnected, http.StatusUnprocessableEntity)
		}
		if _, err := Installed(ctx, h.app, installations[i]); err != nil {
			return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
		}
		return project.OrgID, installations[i], nil
	}
	// With none named: the org's default, or else the one installation that still works. One
	// GitHub removed, suspended or left without a permission is no choice.
	var firstErr error
	if i := slices.IndexFunc(installations, func(one *domain.OrgGitHubInstallation) bool { return one.Default }); i >= 0 {
		_, err := Installed(ctx, h.app, installations[i])
		if err == nil {
			return project.OrgID, installations[i], nil
		}
		if !unusable(err) {
			return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
		}
		firstErr = err
		installations = slices.Delete(slices.Clone(installations), i, i+1)
	}
	working, logins := []*domain.OrgGitHubInstallation{}, []string{}
	for _, one := range installations {
		_, err := Installed(ctx, h.app, one)
		switch {
		case err == nil:
			working, logins = append(working, one), append(logins, one.AccountLogin)
		case !unusable(err):
			return "", nil, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
		case firstErr == nil:
			firstErr = err
		}
	}
	switch len(working) {
	case 0:
		return "", nil, Refusal(h.logger, op, firstErr, http.StatusUnprocessableEntity)
	case 1:
		return project.OrgID, working[0], nil
	}
	return "", nil, refusal.Invalid(ReasonRepositoryAccountRequired, "the org has connected several working GitHub accounts ("+strings.Join(logins, ", ")+") and made none the default for new repositories: say in repository_account which one the new repository is created on, or an owner of the org makes one the default in Settings, GitHub, or name a repository")
}

// unusable says GitHub removed or suspended an installation or it lacks a permission the App
// asks for: what its owner fixes, as opposed to GitHub failing to answer.
func unusable(err error) bool {
	return errors.Is(err, ErrInstallationGone) || errors.Is(err, ErrPermissionNotGranted)
}

func (h *Host) File(ctx context.Context, projectID, repository, ref, path string) ([]byte, error) {
	return h.FileAt(ctx, projectID, repository, ref, path)
}

func (h *Host) HasBranch(ctx context.Context, projectID, repository, branch string) (bool, error) {
	const op = "read-repository-branch"
	token, err := h.token(ctx, projectID, repository, Read)
	if err != nil {
		return false, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	exists, err := h.app.HasBranch(ctx, token, branch)
	if err != nil {
		return false, Refusal(h.logger, op, err, http.StatusUnprocessableEntity)
	}
	return exists, nil
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
		return nil, refusal.Invalid(ReasonUnreadableFile, path+" at "+ref+" is not a file")
	case errors.Is(err, ErrFileTooLarge):
		return nil, refusal.Invalid(ReasonUnreadableFile, path+" at "+ref+" is larger than the platform reads from a repository")
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
