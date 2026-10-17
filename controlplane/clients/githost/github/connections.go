package github

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

// ConnectionsClient is what Connections asks of the platform's GitHub App. *Client satisfies it.
type ConnectionsClient interface {
	App() App
	Installation(ctx context.Context, installationID int64) (*Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]Repository, error)
}

// Connections is the App as the orgs connecting its installations ask of it.
type Connections struct {
	app    ConnectionsClient
	logger *zap.Logger
}

var _ accounts.GitHubApp = (*Connections)(nil)

func NewConnections(app ConnectionsClient, logger *zap.Logger) *Connections {
	return &Connections{app: app, logger: logger}
}

func (c *Connections) Addresses() accounts.GitHubAddresses {
	app := c.app.App()
	return accounts.GitHubAddresses{Slug: app.Slug, InstallURL: app.InstallURL, NewRepositoryURL: app.NewRepositoryURL}
}

// Installation names no account in what it refuses for one removed or suspended: the account is
// what GitHub would have said.
func (c *Connections) Installation(ctx context.Context, installationID int64) (*accounts.GitHubInstallation, error) {
	live, err := c.app.Installation(ctx, installationID)
	if errors.Is(err, ErrInstallationGone) {
		return nil, &InstallationGoneError{Account: "the GitHub account", Installation: installationID, Suspended: errors.Is(err, ErrInstallationSuspended)}
	}
	if err != nil {
		return nil, err
	}
	return &accounts.GitHubInstallation{ID: live.ID, AccountLogin: live.AccountLogin, AccountType: live.AccountType, ChangedAt: live.ChangedAt}, nil
}

func (c *Connections) Repositories(ctx context.Context, installationID int64) ([]accounts.GitHubRepository, error) {
	covered, err := c.app.Repositories(ctx, installationID)
	if errors.Is(err, ErrInstallationGone) {
		return nil, fmt.Errorf("%w: %w", accounts.ErrInstallationGone, err)
	}
	if err != nil {
		return nil, err
	}
	out := make([]accounts.GitHubRepository, len(covered))
	for i, one := range covered {
		out[i] = accounts.GitHubRepository{FullName: one.FullName, Private: one.Private}
	}
	return out, nil
}

func (c *Connections) Check(ctx context.Context, connected *domain.OrgGitHubInstallation) error {
	_, err := Installed(ctx, c.app, connected)
	return err
}

func (c *Connections) Refusal(op string, err error, gone int) error {
	return Refusal(c.logger, op, err, gone)
}
