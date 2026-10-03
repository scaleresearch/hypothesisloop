package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

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

// ErrGitHubAuthorizationUnreadable reports a kept authorisation that the deployment's secret no
// longer opens: the secret was replaced since the account was connected.
var ErrGitHubAuthorizationUnreadable = errors.New("the GitHub authorisation kept for the installation cannot be read with the deployment's secret")

// GitHubInstallationsStore is the installations orgs connected together with the authorisation
// kept for one on a person's own account. That authorisation acts as the person on GitHub, so it
// is stored sealed (AES-256-GCM) under a key derived from the deployment's signing secret and
// bound to its row, and leaves this store only through RenewGitHubUserToken.
type GitHubInstallationsStore struct {
	*AccountsStore
	seal cipher.AEAD
}

func NewGitHubInstallationsStore(accounts *AccountsStore, signingSecret string) (*GitHubInstallationsStore, error) {
	if accounts == nil || signingSecret == "" {
		return nil, errors.New("github_installations_store: the accounts store and the signing secret are required")
	}
	key, err := hkdf.Key(sha256.New, []byte(signingSecret), nil, "hypothesisloop github user authorisation", 32)
	if err != nil {
		return nil, fmt.Errorf("github_installations_store: derive the key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("github_installations_store: %w", err)
	}
	seal, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github_installations_store: %w", err)
	}
	return &GitHubInstallationsStore{AccountsStore: accounts, seal: seal}, nil
}

func rowOf(orgID string, installationID int64) []byte {
	return fmt.Appendf(nil, "%s/%d", orgID, installationID)
}

func (s *GitHubInstallationsStore) sealed(orgID string, installationID int64, authorization string) ([]byte, error) {
	nonce := make([]byte, s.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.seal.Seal(nonce, nonce, []byte(authorization), rowOf(orgID, installationID)), nil
}

// opened is the authorisation a row keeps, and empty for a row that keeps none.
func (s *GitHubInstallationsStore) opened(orgID string, installationID int64, kept []byte) (string, error) {
	if kept == nil {
		return "", nil
	}
	if len(kept) < s.seal.NonceSize() {
		return "", ErrGitHubAuthorizationUnreadable
	}
	nonce, sealed := kept[:s.seal.NonceSize()], kept[s.seal.NonceSize():]
	authorization, err := s.seal.Open(nil, nonce, sealed, rowOf(orgID, installationID))
	if err != nil {
		return "", ErrGitHubAuthorizationUnreadable
	}
	return string(authorization), nil
}

// KeepGitHubUserAuthorization records the authorisation of the person who connected an
// installation on their own account to an org, in place of any kept before.
func (s *GitHubInstallationsStore) KeepGitHubUserAuthorization(ctx context.Context, orgID string, installationID int64, authorization string) error {
	if authorization == "" {
		return errors.New("github_installations_store.KeepGitHubUserAuthorization: there is no authorisation to keep")
	}
	sealed, err := s.sealed(orgID, installationID, authorization)
	if err != nil {
		return fmt.Errorf("github_installations_store.KeepGitHubUserAuthorization: %w", err)
	}
	tag, err := s.pool.pool.Exec(ctx, `UPDATE org_github_installations SET user_authorization = $3 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID, sealed)
	if err != nil {
		return fmt.Errorf("github_installations_store.KeepGitHubUserAuthorization: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGitHubInstallationNotFound
	}
	return nil
}

// renewalLimit bounds a renewal, which does not end with the request that asked for it.
const renewalLimit = time.Minute

// RenewGitHubUserToken hands renew the authorisation kept for an org's installation, empty when
// none is, and keeps the one renew answers in its place. What renew returns beside the next one
// is returned. GitHub takes a refresh token once, so the row is held from the read to the write
// and a second renewal waits for the first, and none of it follows the request's cancellation: a
// caller that went away after GitHub replaced the token would leave the spent one stored.
// ErrGitHubInstallationNotFound when the org has not connected the installation,
// ErrGitHubAuthorizationUnreadable when what is kept was sealed under another secret.
func (s *GitHubInstallationsStore) RenewGitHubUserToken(ctx context.Context, orgID string, installationID int64, renew func(ctx context.Context, authorization string) (accessToken, next string, err error)) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renewalLimit)
	defer cancel()
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("github_installations_store.RenewGitHubUserToken: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var kept []byte
	err = tx.QueryRow(ctx, `SELECT user_authorization FROM org_github_installations WHERE org_id = $1 AND installation_id = $2 FOR UPDATE`, orgID, installationID).Scan(&kept)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrGitHubInstallationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("github_installations_store.RenewGitHubUserToken: %w", err)
	}
	authorization, err := s.opened(orgID, installationID, kept)
	if err != nil {
		return "", err
	}
	accessToken, next, err := renew(ctx, authorization)
	if err != nil {
		return "", err
	}
	if next == "" {
		return "", errors.New("github_installations_store.RenewGitHubUserToken: the renewal answered no authorisation to keep")
	}
	sealed, err := s.sealed(orgID, installationID, next)
	if err != nil {
		return "", fmt.Errorf("github_installations_store.RenewGitHubUserToken: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE org_github_installations SET user_authorization = $3 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID, sealed); err != nil {
		return "", fmt.Errorf("github_installations_store.RenewGitHubUserToken: keep the next authorisation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("github_installations_store.RenewGitHubUserToken: commit: %w", err)
	}
	return accessToken, nil
}

// DisconnectGitHubInstallation takes an installation away from an org, with the authorisation
// kept for it.
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
