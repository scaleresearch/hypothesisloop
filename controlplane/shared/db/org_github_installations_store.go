package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrGitHubInstallationNotFound reports an installation the org has not connected.
var ErrGitHubInstallationNotFound = errors.New("the org has not connected that GitHub installation")

// ConnectGitHubInstallation records an installation as connected to an org. One that is already
// connected keeps who connected it and when, and takes the account's name as it stands now.
func (s *AccountsStore) ConnectGitHubInstallation(ctx context.Context, in *domain.OrgGitHubInstallation) error {
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO org_github_installations (org_id, installation_id, account_login, connected_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, installation_id) DO UPDATE SET account_login = EXCLUDED.account_login
RETURNING connected_by, connected_at`, in.OrgID, in.InstallationID, in.AccountLogin, in.ConnectedBy).Scan(&in.ConnectedBy, &in.ConnectedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return ErrOrgNotFound
	}
	if err != nil {
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: %w", err)
	}
	return nil
}

// ListGitHubInstallations returns the installations an org connected, oldest first.
func (s *AccountsStore) ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT org_id, installation_id, account_login, connected_by, connected_at
FROM org_github_installations WHERE org_id = $1
ORDER BY connected_at, installation_id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListGitHubInstallations: %w", err)
	}
	defer rows.Close()
	out := []*domain.OrgGitHubInstallation{}
	for rows.Next() {
		in := &domain.OrgGitHubInstallation{}
		if err := rows.Scan(&in.OrgID, &in.InstallationID, &in.AccountLogin, &in.ConnectedBy, &in.ConnectedAt); err != nil {
			return nil, fmt.Errorf("accounts_store.ListGitHubInstallations: scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// GitHubInstallationOfAccount returns the installation an org connected on the GitHub account
// named, whatever its casing, or nil. GitHub allows one installation of an App per account.
func (s *AccountsStore) GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error) {
	in := &domain.OrgGitHubInstallation{}
	err := s.pool.pool.QueryRow(ctx, `
SELECT org_id, installation_id, account_login, connected_by, connected_at
FROM org_github_installations WHERE org_id = $1 AND lower(account_login) = lower($2)
ORDER BY connected_at DESC LIMIT 1`, orgID, accountLogin).
		Scan(&in.OrgID, &in.InstallationID, &in.AccountLogin, &in.ConnectedBy, &in.ConnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GitHubInstallationOfAccount: %w", err)
	}
	return in, nil
}

// DisconnectGitHubInstallation takes an installation away from an org.
// ErrGitHubInstallationNotFound when the org had not connected it.
func (s *AccountsStore) DisconnectGitHubInstallation(ctx context.Context, orgID string, installationID int64) error {
	tag, err := s.pool.pool.Exec(ctx, `DELETE FROM org_github_installations WHERE org_id = $1 AND installation_id = $2`, orgID, installationID)
	if err != nil {
		return fmt.Errorf("accounts_store.DisconnectGitHubInstallation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGitHubInstallationNotFound
	}
	return nil
}
