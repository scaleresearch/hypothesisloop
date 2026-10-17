package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var (
	ErrOrgMemberNotFound = errors.New("member not found")
	// ErrLastOwner reports a change that would leave an org with no owner.
	ErrLastOwner         = errors.New("an org keeps at least one owner")
	ErrAlreadyOrgMember  = errors.New("already a member of the org")
	ErrOrgInviteNotFound = errors.New("invite not found")
	ErrOrgInviteUsed     = errors.New("the invite was already accepted")
	ErrOrgInviteRevoked  = errors.New("the invite was revoked")
	ErrOrgInviteExpired  = errors.New("the invite has expired")
)

// ListOrgMembers returns one page of an org's members, oldest first, and how many it has.
func (s *AccountsStore) ListOrgMembers(ctx context.Context, orgID string, limit, offset int) ([]*domain.OrgMember, int, error) {
	var total int
	if err := s.pool.pool.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("accounts_store.ListOrgMembers: count: %w", err)
	}
	rows, err := s.pool.pool.Query(ctx, `
SELECT org_id, user_id, `+userName("user_id")+`, role, created_at FROM org_members WHERE org_id = $1
ORDER BY created_at, user_id LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("accounts_store.ListOrgMembers: %w", err)
	}
	defer rows.Close()
	out := []*domain.OrgMember{}
	for rows.Next() {
		m := &domain.OrgMember{}
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.Name, &m.Role, &m.JoinedAt); err != nil {
			return nil, 0, fmt.Errorf("accounts_store.ListOrgMembers: scan: %w", err)
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// SetOrgMemberRole changes a member's role. ErrOrgMemberNotFound when the user is not in the
// org, ErrLastOwner when the change would leave the org with no owner. A member who stops being
// an owner takes the invites they made with them.
func (s *AccountsStore) SetOrgMemberRole(ctx context.Context, orgID, userID string, role domain.OrgRole) (*domain.OrgMember, error) {
	if !domain.ValidOrgRole(role) {
		return nil, fmt.Errorf("accounts_store.SetOrgMemberRole: unknown role %q", role)
	}
	member := &domain.OrgMember{OrgID: orgID, UserID: userID, Role: role}
	err := s.changeMembership(ctx, orgID, userID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2 RETURNING created_at`,
			orgID, userID, string(role)).Scan(&member.JoinedAt)
	}, role != domain.OrgRoleOwner)
	if err != nil {
		return nil, wrapUnlessRefusal("SetOrgMemberRole", err)
	}
	return member, nil
}

// RemoveOrgMember ends a membership, under the same refusals as SetOrgMemberRole. The API keys
// the person created stop working with it: a key is read as orphaned from then on (apiKeySelect).
func (s *AccountsStore) RemoveOrgMember(ctx context.Context, orgID, userID string) error {
	err := s.changeMembership(ctx, orgID, userID, func(tx pgx.Tx) error {
		var removed bool
		return tx.QueryRow(ctx, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2 RETURNING true`, orgID, userID).Scan(&removed)
	}, true)
	return wrapUnlessRefusal("RemoveOrgMember", err)
}

// changeMembership applies one change to a member's row. The org's row is locked first, so two
// changes to one org's members run one after the other and the owner that remains is counted
// after the change, inside its transaction: no two of them can each see the other as the owner
// that stays. With losesOwnership the pending invites the member made are revoked in the same step.
func (s *AccountsStore) changeMembership(ctx context.Context, orgID, userID string, change func(pgx.Tx) error, losesOwnership bool) error {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var locked bool
	err = tx.QueryRow(ctx, `SELECT true FROM orgs WHERE id = $1 FOR NO KEY UPDATE`, orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOrgMemberNotFound
	}
	if err != nil {
		return fmt.Errorf("lock org: %w", err)
	}
	if err := change(tx); errors.Is(err, pgx.ErrNoRows) {
		return ErrOrgMemberNotFound
	} else if err != nil {
		return err
	}
	if losesOwnership {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_members WHERE org_id = $1 AND role = 'owner')`, orgID).Scan(&owned); err != nil {
			return fmt.Errorf("count owners: %w", err)
		}
		if !owned {
			return ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `
UPDATE org_invites SET revoked_at = now()
WHERE org_id = $1 AND created_by = $2 AND accepted_at IS NULL AND revoked_at IS NULL`, orgID, userID); err != nil {
			return fmt.Errorf("revoke invites: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func wrapUnlessRefusal(op string, err error) error {
	if err == nil || errors.Is(err, ErrOrgMemberNotFound) || errors.Is(err, ErrLastOwner) {
		return err
	}
	return fmt.Errorf("accounts_store.%s: %w", op, err)
}

var orgInviteColumns = `id, org_id, role, created_by, ` + userName("created_by") + `, created_at, expires_at, COALESCE(accepted_by, ''), ` + userName("accepted_by") + `, accepted_at, revoked_at`

func scanOrgInvite(row rowScanner) (*domain.OrgInvite, error) {
	i := &domain.OrgInvite{}
	var acceptedBy, acceptedByName string
	i.Actor.Kind = domain.ActorUser
	if err := row.Scan(&i.ID, &i.OrgID, &i.Role, &i.Actor.ID, &i.Actor.Name, &i.CreatedAt, &i.ExpiresAt, &acceptedBy, &acceptedByName, &i.AcceptedAt, &i.RevokedAt); err != nil {
		return nil, err
	}
	if acceptedBy != "" {
		accepted := userActor(acceptedBy, acceptedByName)
		i.AcceptedActor = &accepted
	}
	return i, nil
}

// CreateOrgInvite stores an invite under the hash of its token. invite.ID, OrgID, Role,
// Actor.ID and ExpiresAt are required.
func (s *AccountsStore) CreateOrgInvite(ctx context.Context, invite *domain.OrgInvite, tokenHash string) error {
	if !domain.ValidOrgRole(invite.Role) {
		return fmt.Errorf("accounts_store.CreateOrgInvite: unknown role %q", invite.Role)
	}
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO org_invites (id, org_id, role, token_hash, created_by, expires_at) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, `+userName("created_by"), invite.ID, invite.OrgID, string(invite.Role), tokenHash, invite.Actor.ID, invite.ExpiresAt).Scan(&invite.CreatedAt, &invite.Actor.Name)
	invite.Actor.Kind = domain.ActorUser
	if err != nil {
		return fmt.Errorf("accounts_store.CreateOrgInvite: %w", err)
	}
	return nil
}

const orgInvitePending = `accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`

// ListPendingOrgInvites returns one page of the invites of an org that can still be accepted,
// newest first, and how many there are.
func (s *AccountsStore) ListPendingOrgInvites(ctx context.Context, orgID string, limit, offset int) ([]*domain.OrgInvite, int, error) {
	var total int
	if err := s.pool.pool.QueryRow(ctx, `SELECT count(*) FROM org_invites WHERE org_id = $1 AND `+orgInvitePending, orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("accounts_store.ListPendingOrgInvites: count: %w", err)
	}
	rows, err := s.pool.pool.Query(ctx, `
SELECT `+orgInviteColumns+` FROM org_invites WHERE org_id = $1 AND `+orgInvitePending+`
ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("accounts_store.ListPendingOrgInvites: %w", err)
	}
	defer rows.Close()
	out := []*domain.OrgInvite{}
	for rows.Next() {
		i, err := scanOrgInvite(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("accounts_store.ListPendingOrgInvites: scan: %w", err)
		}
		out = append(out, i)
	}
	return out, total, rows.Err()
}

// GetOrgInvite returns an invite by id, or nil when there is none.
func (s *AccountsStore) GetOrgInvite(ctx context.Context, id string) (*domain.OrgInvite, error) {
	invite, err := scanOrgInvite(s.pool.pool.QueryRow(ctx, `SELECT `+orgInviteColumns+` FROM org_invites WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GetOrgInvite: %w", err)
	}
	return invite, nil
}

// RevokeOrgInvite ends an invite. One already revoked keeps its first revocation time.
// ErrOrgInviteNotFound when there is no such invite, ErrOrgInviteUsed when it was accepted: the
// membership it made is ended as a membership.
func (s *AccountsStore) RevokeOrgInvite(ctx context.Context, inviteID string) error {
	tag, err := s.pool.pool.Exec(ctx, `
UPDATE org_invites SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1 AND accepted_at IS NULL`, inviteID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("accounts_store.RevokeOrgInvite: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Accepted is final, so reading it after the write that passed it over cannot be overtaken.
	invite, err := s.GetOrgInvite(ctx, inviteID)
	if err != nil {
		return err
	}
	if invite == nil {
		return ErrOrgInviteNotFound
	}
	return ErrOrgInviteUsed
}

// AcceptOrgInvite makes userID a member of the invite's org with the invite's role, in one
// transaction that marks the invite and inserts the member row. The update takes the invite's
// row, so of two accepts at once the second waits and then finds it used. An invite accepted by
// someone already in the org is refused and stays unused.
func (s *AccountsStore) AcceptOrgInvite(ctx context.Context, tokenHash, userID string) (*domain.OrgMember, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.AcceptOrgInvite: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	member := &domain.OrgMember{UserID: userID}
	err = tx.QueryRow(ctx, `
UPDATE org_invites SET accepted_by = $2, accepted_at = now()
WHERE token_hash = $1 AND `+orgInvitePending+`
RETURNING org_id, role`, tokenHash, userID).Scan(&member.OrgID, &member.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, whyNotAccepted(ctx, tx, tokenHash)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.AcceptOrgInvite: %w", err)
	}
	err = tx.QueryRow(ctx, `
INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO NOTHING RETURNING created_at`, member.OrgID, userID, string(member.Role)).Scan(&member.JoinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlreadyOrgMember
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.AcceptOrgInvite: insert member: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("accounts_store.AcceptOrgInvite: commit: %w", err)
	}
	return member, nil
}

// whyNotAccepted names what stopped an invite from being accepted.
func whyNotAccepted(ctx context.Context, tx pgx.Tx, tokenHash string) error {
	var used, revoked bool
	err := tx.QueryRow(ctx, `SELECT accepted_at IS NOT NULL, revoked_at IS NOT NULL FROM org_invites WHERE token_hash = $1`, tokenHash).Scan(&used, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrOrgInviteNotFound
	case err != nil:
		return fmt.Errorf("accounts_store.AcceptOrgInvite: read invite: %w", err)
	case used:
		return ErrOrgInviteUsed
	case revoked:
		return ErrOrgInviteRevoked
	default:
		return ErrOrgInviteExpired
	}
}
