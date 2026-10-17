package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ErrGitHubInstallationNotFound reports an installation the org has not connected.
var ErrGitHubInstallationNotFound = errors.New("the org has not connected that GitHub installation")

// installationColumns reads an installation with who connected it: a user or the API key it was
// connected with, under the name each carries now or its id while it carries none, or staff, or
// unverified with no id and no name on one whose connector was not kept.
const installationColumns = `org_id, installation_id, account_login, account_type, connected_by_kind,
       CASE WHEN connected_by_kind IN ('user', 'key') THEN connected_by_id ELSE '' END,
       CASE connected_by_kind
         WHEN 'user' THEN COALESCE((SELECT named.name FROM users named WHERE named.id = connected_by_id AND named.name <> ''), connected_by_id)
         WHEN 'key' THEN COALESCE((SELECT named.name FROM api_keys named WHERE named.id = connected_by_id AND named.name <> ''), connected_by_id)
         WHEN 'staff' THEN 'staff'
         ELSE '' END,
       connected_at, is_default`

func scanInstallation(row rowScanner, in *domain.OrgGitHubInstallation) error {
	return row.Scan(&in.OrgID, &in.InstallationID, &in.AccountLogin, &in.AccountType, &in.ConnectedActor.Kind, &in.ConnectedActor.ID, &in.ConnectedActor.Name, &in.ConnectedAt, &in.Default)
}

// ConnectGitHubInstallation records an installation as connected to an org by in.ConnectedActor:
// a user, a key or staff. One that is already connected keeps who connected it and when, and
// takes the account's name and type as they stand now. GitHub holds one installation of an App
// per account, so another the org connected on the same account was removed there and is
// replaced, its default with it. The org's first installation is its default.
func (s *Store) ConnectGitHubInstallation(ctx context.Context, in *domain.OrgGitHubInstallation) error {
	switch in.ConnectedActor.Kind {
	case domain.ActorUser, domain.ActorKey, domain.ActorStaff:
	default:
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: an installation is connected by a user, a key or staff, got %q", in.ConnectedActor.Kind)
	}
	if in.AccountType == "" {
		return errors.New("accounts_store.ConnectGitHubInstallation: an installation is connected with the type of its account")
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var replacedDefault bool
	err = tx.QueryRow(ctx, `
WITH replaced AS (
  DELETE FROM org_github_installations
  WHERE org_id = $1 AND installation_id <> $2 AND lower(account_login) = lower($3)
  RETURNING is_default
)
SELECT COALESCE(bool_or(is_default), false) FROM replaced`, in.OrgID, in.InstallationID, in.AccountLogin).Scan(&replacedDefault)
	if err != nil {
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: replace the installation removed on the account: %w", err)
	}
	err = scanInstallation(tx.QueryRow(ctx, `
INSERT INTO org_github_installations (org_id, installation_id, account_login, account_type, connected_by_kind, connected_by_id, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7 OR NOT EXISTS (SELECT 1 FROM org_github_installations WHERE org_id = $1))
ON CONFLICT (org_id, installation_id) DO UPDATE SET account_login = EXCLUDED.account_login, account_type = EXCLUDED.account_type
RETURNING `+installationColumns, in.OrgID, in.InstallationID, in.AccountLogin, in.AccountType, string(in.ConnectedActor.Kind), in.ConnectedActor.ID, replacedDefault), in)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return ErrOrgNotFound
	}
	if err != nil {
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("accounts_store.ConnectGitHubInstallation: commit: %w", err)
	}
	return nil
}

// MakeGitHubInstallationDefault makes an installation the org connected the one a new repository
// is created on when a request names no account, in place of any other.
// ErrGitHubInstallationNotFound when the org has not connected it.
func (s *Store) MakeGitHubInstallationDefault(ctx context.Context, orgID string, installationID int64) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("accounts_store.MakeGitHubInstallationDefault: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Cleared first: the index takes one default per org at every row.
	if _, err := tx.Exec(ctx, `UPDATE org_github_installations SET is_default = false WHERE org_id = $1 AND is_default AND installation_id <> $2`, orgID, installationID); err != nil {
		return fmt.Errorf("accounts_store.MakeGitHubInstallationDefault: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE org_github_installations SET is_default = true WHERE org_id = $1 AND installation_id = $2`, orgID, installationID)
	if err != nil {
		return fmt.Errorf("accounts_store.MakeGitHubInstallationDefault: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGitHubInstallationNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("accounts_store.MakeGitHubInstallationDefault: commit: %w", err)
	}
	return nil
}

// ListGitHubInstallations returns the installations an org connected, oldest first.
func (s *Store) ListGitHubInstallations(ctx context.Context, orgID string) ([]*domain.OrgGitHubInstallation, error) {
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
func (s *Store) GitHubInstallationOfAccount(ctx context.Context, orgID, accountLogin string) (*domain.OrgGitHubInstallation, error) {
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

// DisconnectGitHubInstallation takes an installation away from an org.
// ErrGitHubInstallationNotFound when the org had not connected it.
func (s *Store) DisconnectGitHubInstallation(ctx context.Context, orgID string, installationID int64) error {
	tag, err := s.pool.pool.Exec(ctx, `DELETE FROM org_github_installations WHERE org_id = $1 AND installation_id = $2`, orgID, installationID)
	if err != nil {
		return fmt.Errorf("accounts_store.DisconnectGitHubInstallation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGitHubInstallationNotFound
	}
	return nil
}

// ErrGitHubInstallStateInvalid reports an install state that is not one this store issued to the
// caller and still holds: made up, taken before, run out, or another person's.
var ErrGitHubInstallStateInvalid = errors.New("the GitHub install state is unknown, used, expired or another person's")

// GitHubInstallState is an install of the GitHub App a person started for an org.
type GitHubInstallState struct {
	OrgID    string
	Actor    domain.Actor
	ReturnTo string
	IssuedAt time.Time
}

func stateHash(state string) []byte {
	sum := sha256.Sum256([]byte(state))
	return sum[:]
}

// IssueGitHubInstallState records an install of the GitHub App that actor starts for an org and
// returns its state, a random value good once until it expires. States run out are dropped here.
// ErrOrgNotFound when the org does not exist.
func (s *Store) IssueGitHubInstallState(ctx context.Context, orgID string, actor domain.Actor, returnTo string, lifetime time.Duration) (string, time.Time, error) {
	if actor.Kind == "" || actor.ID == "" || lifetime <= 0 {
		return "", time.Time{}, errors.New("accounts_store.IssueGitHubInstallState: a state is issued to an actor with an id, for a lifetime")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", time.Time{}, fmt.Errorf("accounts_store.IssueGitHubInstallState: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(random)
	if _, err := s.pool.pool.Exec(ctx, `DELETE FROM github_install_states WHERE expires_at <= now()`); err != nil {
		return "", time.Time{}, fmt.Errorf("accounts_store.IssueGitHubInstallState: drop the expired: %w", err)
	}
	var expires time.Time
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO github_install_states (state_hash, org_id, actor_kind, actor_id, return_to, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
RETURNING expires_at`, stateHash(state), orgID, string(actor.Kind), actor.ID, returnTo, lifetime.String()).Scan(&expires)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return "", time.Time{}, ErrOrgNotFound
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("accounts_store.IssueGitHubInstallState: %w", err)
	}
	return state, expires, nil
}

// TakeGitHubInstallState takes the install state issued to actor, once and while it holds.
// ErrGitHubInstallStateInvalid for any other.
func (s *Store) TakeGitHubInstallState(ctx context.Context, state string, actor domain.Actor) (*GitHubInstallState, error) {
	taken := &GitHubInstallState{Actor: actor}
	err := s.pool.pool.QueryRow(ctx, `
DELETE FROM github_install_states
WHERE state_hash = $1 AND actor_kind = $2 AND actor_id = $3 AND expires_at > now()
RETURNING org_id, return_to, issued_at`, stateHash(state), string(actor.Kind), actor.ID).Scan(&taken.OrgID, &taken.ReturnTo, &taken.IssuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGitHubInstallStateInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.TakeGitHubInstallState: %w", err)
	}
	return taken, nil
}
