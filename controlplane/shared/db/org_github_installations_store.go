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

// installationColumns reads an installation with who connected it: a user or the API key it was
// connected with, under the name each carries now or its id while it carries none, or staff, or
// unverified with no id and no name on one whose connector was not kept.
const installationColumns = `org_id, installation_id, account_login, account_type, connected_by_kind,
       CASE WHEN connected_by_kind IN ('user', 'key') THEN connected_by ELSE '' END,
       CASE connected_by_kind
         WHEN 'user' THEN COALESCE((SELECT named.name FROM users named WHERE named.id = connected_by AND named.name <> ''), connected_by)
         WHEN 'key' THEN COALESCE((SELECT named.name FROM api_keys named WHERE named.id = connected_by AND named.name <> ''), connected_by)
         WHEN 'staff' THEN 'staff'
         ELSE '' END,
       connected_at`

func scanInstallation(row rowScanner, in *domain.OrgGitHubInstallation) error {
	return row.Scan(&in.OrgID, &in.InstallationID, &in.AccountLogin, &in.AccountType, &in.ConnectedActor.Kind, &in.ConnectedActor.ID, &in.ConnectedActor.Name, &in.ConnectedAt)
}

// ConnectGitHubInstallation records an installation as connected to an org by in.ConnectedActor:
// a user, a key or staff. One that is already connected keeps who connected it and when, and
// takes the account's name and type as they stand now.
func (s *AccountsStore) ConnectGitHubInstallation(ctx context.Context, in *domain.OrgGitHubInstallation) error {
	switch in.ConnectedActor.Kind {
	case domain.ActorUser, domain.ActorKey, domain.ActorStaff:
	default:
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: an installation is connected by a user, a key or staff, got %q", in.ConnectedActor.Kind)
	}
	if in.AccountType == "" {
		return errors.New("accounts_store.ConnectGitHubInstallation: an installation is connected with the type of its account")
	}
	err := scanInstallation(s.pool.pool.QueryRow(ctx, `
INSERT INTO org_github_installations (org_id, installation_id, account_login, account_type, connected_by_kind, connected_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (org_id, installation_id) DO UPDATE SET account_login = EXCLUDED.account_login, account_type = EXCLUDED.account_type
RETURNING `+installationColumns, in.OrgID, in.InstallationID, in.AccountLogin, in.AccountType, string(in.ConnectedActor.Kind), in.ConnectedActor.ID), in)
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
SELECT `+installationColumns+`
FROM org_github_installations WHERE org_id = $1
ORDER BY connected_at, installation_id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListGitHubInstallations: %w", err)
	}
	defer rows.Close()
	out := []*domain.OrgGitHubInstallation{}
	for rows.Next() {
		in := &domain.OrgGitHubInstallation{}
		if err := scanInstallation(rows, in); err != nil {
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
	err := scanInstallation(s.pool.pool.QueryRow(ctx, `
SELECT `+installationColumns+`
FROM org_github_installations WHERE org_id = $1 AND lower(account_login) = lower($2)
ORDER BY connected_at DESC LIMIT 1`, orgID, accountLogin), in)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GitHubInstallationOfAccount: %w", err)
	}
	return in, nil
}

// SetGitHubInstallationAccountType records the type of the account an installation is on, on
// every org's record of it.
func (s *AccountsStore) SetGitHubInstallationAccountType(ctx context.Context, installationID int64, accountType string) error {
	if accountType == "" {
		return errors.New("accounts_store.SetGitHubInstallationAccountType: the account's type is required")
	}
	if _, err := s.pool.pool.Exec(ctx, `UPDATE org_github_installations SET account_type = $2 WHERE installation_id = $1`, installationID, accountType); err != nil {
		return fmt.Errorf("accounts_store.SetGitHubInstallationAccountType: %w", err)
	}
	return nil
}

// KeepGitHubUserToken records the refresh token of the person who connected an installation on
// their own account to an org, in place of any kept before. An empty one keeps none.
func (s *AccountsStore) KeepGitHubUserToken(ctx context.Context, orgID string, installationID int64, refreshToken string) error {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE org_github_installations SET user_refresh_token = $3 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID, refreshToken)
	if err != nil {
		return fmt.Errorf("accounts_store.KeepGitHubUserToken: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGitHubInstallationNotFound
	}
	return nil
}

// RenewGitHubUserToken hands renew the refresh token kept for an org's installation, empty when
// none is, and keeps the one renew answers in its place. GitHub takes a refresh token once, so
// the row is held from the read to the write and a second renewal waits for the first. What
// renew returns beside the next token is returned. ErrGitHubInstallationNotFound when the org
// has not connected the installation.
func (s *AccountsStore) RenewGitHubUserToken(ctx context.Context, orgID string, installationID int64, renew func(ctx context.Context, refreshToken string) (accessToken, next string, err error)) (string, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("accounts_store.RenewGitHubUserToken: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var kept string
	err = tx.QueryRow(ctx, `SELECT user_refresh_token FROM org_github_installations WHERE org_id = $1 AND installation_id = $2 FOR UPDATE`, orgID, installationID).Scan(&kept)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrGitHubInstallationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("accounts_store.RenewGitHubUserToken: %w", err)
	}
	accessToken, next, err := renew(ctx, kept)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE org_github_installations SET user_refresh_token = $3 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID, next); err != nil {
		return "", fmt.Errorf("accounts_store.RenewGitHubUserToken: keep the next token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("accounts_store.RenewGitHubUserToken: commit: %w", err)
	}
	return accessToken, nil
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
