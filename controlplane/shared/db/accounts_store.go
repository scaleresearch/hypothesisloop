package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	ErrUserNotFound    = errors.New("user not found")
	ErrOrgNotFound     = errors.New("org not found")
	ErrProjectNotFound = errors.New("project not found")
	ErrAPIKeyNotFound  = errors.New("api key not found")
	// ErrAPIKeyCreatorLacksOrg reports a key asked for in the name of a user who may not hold
	// it (apiKeyHeldBy): such a key would not authenticate.
	ErrAPIKeyCreatorLacksOrg = errors.New("the key's creator may not hold such a key in the org")
	// ErrMaxOrgsReached and ErrMaxProjectsReached report a create past what one account may hold.
	ErrMaxOrgsReached     = errors.New("the owner already owns as many orgs as one person may")
	ErrMaxProjectsReached = errors.New("the org already holds as many projects as one org may")
	// ErrProjectNameTaken reports that the org already has a project of that name.
	ErrProjectNameTaken = errors.New("the org already has a project with that name")
)

// AccountsStore holds who exists and what they own: users, orgs, memberships, projects and API
// keys, and the placement of a platform experiment, job or agent within them.
type AccountsStore struct {
	pool *Pool
}

func NewAccountsStore(pool *Pool) *AccountsStore {
	return &AccountsStore{pool: pool}
}

func newAccountID(prefix string) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + "_" + id.String(), nil
}

// EnsureUserHome returns the user and the orgs they belong to, creating the user with a home org
// and its default project on their first request. The common case, a user who exists under the
// name stated, is one read. A first request inserts the user and the org in one transaction
// keyed off the users insert, so two concurrent first requests make one org.
//
// name is what the identity provider states the person is called now. It replaces the name kept
// when it differs, and an empty one, a provider that states none, leaves the kept name alone.
func (s *AccountsStore) EnsureUserHome(ctx context.Context, userID, name, orgName string) (*domain.User, []string, error) {
	user, orgIDs, err := readUserHome(ctx, s.pool.pool, userID)
	if err != nil {
		return nil, nil, err
	}
	if user != nil {
		if name != "" && name != user.Name {
			if _, err := s.pool.pool.Exec(ctx, `UPDATE users SET name = $2 WHERE id = $1`, userID, name); err != nil {
				return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: rename user: %w", err)
			}
			user.Name = name
		}
		return user, orgIDs, nil
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `INSERT INTO users (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, userID, name)
	if err != nil {
		return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: insert user: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if _, err := createOrg(ctx, tx, orgName, userID); err != nil {
			return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: %w", err)
		}
	}
	if user, orgIDs, err = readUserHome(ctx, tx, userID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: commit: %w", err)
	}
	return user, orgIDs, nil
}

// userName is the SQL of the name of the user whose id the expression idExpr yields as an actor
// carries it: their display name, or that id while they carry none.
func userName(idExpr string) string {
	return `COALESCE((SELECT named.name FROM users named WHERE named.id = ` + idExpr + ` AND named.name <> ''), ` + idExpr + `, '')`
}

// userActor is the user id as an actor, under the name userName read for it.
func userActor(id, name string) domain.Actor {
	return domain.Actor{Kind: domain.ActorUser, ID: id, Name: name}
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readUserHome returns a nil user when there is none.
func readUserHome(ctx context.Context, q rowQuerier, userID string) (*domain.User, []string, error) {
	user := &domain.User{ID: userID}
	var orgIDs []string
	err := q.QueryRow(ctx, `
SELECT u.name, u.staff, u.created_at,
       COALESCE(array_agg(m.org_id ORDER BY m.created_at, m.org_id) FILTER (WHERE m.org_id IS NOT NULL), '{}')
FROM users u LEFT JOIN org_members m ON m.user_id = u.id
WHERE u.id = $1 GROUP BY u.id`, userID).Scan(&user.Name, &user.Staff, &user.CreatedAt, &orgIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("accounts_store.EnsureUserHome: read user: %w", err)
	}
	return user, orgIDs, nil
}

// createOrg inserts an org, its owner and its default project.
func createOrg(ctx context.Context, tx pgx.Tx, name, ownerUserID string) (*domain.Org, error) {
	orgID, err := newAccountID("org")
	if err != nil {
		return nil, err
	}
	org := &domain.Org{ID: orgID, Name: name, Role: domain.OrgRoleOwner}
	if err := tx.QueryRow(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2) RETURNING created_at`, orgID, name).Scan(&org.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert org: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)`,
		orgID, ownerUserID, string(domain.OrgRoleOwner)); err != nil {
		return nil, fmt.Errorf("insert owner: %w", err)
	}
	projectID, err := newAccountID("prj")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO projects (id, org_id, name) VALUES ($1, $2, $3)`,
		projectID, orgID, domain.DefaultProjectName); err != nil {
		return nil, fmt.Errorf("insert default project: %w", err)
	}
	return org, nil
}

// CreateOrg creates an org owned by ownerUserID, with its default project. ErrUserNotFound when
// there is no such user. With maxOwned set the owner may own that many orgs and no more, this one
// included, and ErrMaxOrgsReached refuses the next; zero holds them to no limit. The owner's row
// is locked first and the orgs they own are counted after the insert, inside its transaction, so
// two creates at once cannot both take the last place.
func (s *AccountsStore) CreateOrg(ctx context.Context, name, ownerUserID string, maxOwned int) (*domain.Org, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateOrg: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var locked bool
	err = tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR NO KEY UPDATE`, ownerUserID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateOrg: lock owner: %w", err)
	}
	org, err := createOrg(ctx, tx, name, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateOrg: %w", err)
	}
	if maxOwned > 0 {
		var owned int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE user_id = $1 AND role = 'owner'`, ownerUserID).Scan(&owned); err != nil {
			return nil, fmt.Errorf("accounts_store.CreateOrg: count owned: %w", err)
		}
		if owned > maxOwned {
			return nil, ErrMaxOrgsReached
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("accounts_store.CreateOrg: commit: %w", err)
	}
	return org, nil
}

func (s *AccountsStore) RenameOrg(ctx context.Context, id, name string) error {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE orgs SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return fmt.Errorf("accounts_store.RenameOrg: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOrgNotFound
	}
	return nil
}

// GetOrg returns an org, or nil when there is none.
func (s *AccountsStore) GetOrg(ctx context.Context, id string) (*domain.Org, error) {
	org := &domain.Org{}
	err := s.pool.pool.QueryRow(ctx, `SELECT id, name, created_at FROM orgs WHERE id = $1`, id).Scan(&org.ID, &org.Name, &org.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GetOrg: %w", err)
	}
	return org, nil
}

// ListOrgs returns the given orgs, oldest first, with no role set.
func (s *AccountsStore) ListOrgs(ctx context.Context, orgIDs []string) ([]*domain.Org, error) {
	return s.listOrgs(ctx, `WHERE id = ANY($1)`, orgIDs)
}

// ListAllOrgs returns every org, oldest first, with no role set.
func (s *AccountsStore) ListAllOrgs(ctx context.Context) ([]*domain.Org, error) {
	return s.listOrgs(ctx, ``)
}

func (s *AccountsStore) listOrgs(ctx context.Context, where string, args ...any) ([]*domain.Org, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT id, name, created_at FROM orgs `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListOrgs: %w", err)
	}
	defer rows.Close()
	out := []*domain.Org{}
	for rows.Next() {
		org := &domain.Org{}
		if err := rows.Scan(&org.ID, &org.Name, &org.CreatedAt); err != nil {
			return nil, fmt.Errorf("accounts_store.ListOrgs: scan: %w", err)
		}
		out = append(out, org)
	}
	return out, rows.Err()
}

// SetUserStaff records whether a user is one of the platform's own operators.
func (s *AccountsStore) SetUserStaff(ctx context.Context, userID string, staff bool) error {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE users SET staff = $2 WHERE id = $1`, userID, staff)
	if err != nil {
		return fmt.Errorf("accounts_store.SetUserStaff: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ListUserOrgs returns the orgs a user belongs to, oldest first, each with the user's role.
func (s *AccountsStore) ListUserOrgs(ctx context.Context, userID string) ([]*domain.Org, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT o.id, o.name, m.role, o.created_at
FROM org_members m JOIN orgs o ON o.id = m.org_id
WHERE m.user_id = $1
ORDER BY m.created_at, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListUserOrgs: %w", err)
	}
	defer rows.Close()
	out := []*domain.Org{}
	for rows.Next() {
		org := &domain.Org{}
		if err := rows.Scan(&org.ID, &org.Name, &org.Role, &org.CreatedAt); err != nil {
			return nil, fmt.Errorf("accounts_store.ListUserOrgs: scan: %w", err)
		}
		out = append(out, org)
	}
	return out, rows.Err()
}

// OrgRole returns a user's role in an org; found is false when they are not a member.
func (s *AccountsStore) OrgRole(ctx context.Context, orgID, userID string) (role domain.OrgRole, found bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("accounts_store.OrgRole: %w", err)
	}
	return role, true, nil
}

// CreateProject creates a project in an org. With maxProjects set the org may hold that many
// and no more, this one included, and ErrMaxProjectsReached refuses the next; zero holds it to no
// limit. The org's row is locked first and its projects are counted after the insert, as
// CreateOrg counts an owner's orgs.
func (s *AccountsStore) CreateProject(ctx context.Context, orgID, name string, maxProjects int) (*domain.Project, error) {
	id, err := newAccountID("prj")
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateProject: %w", err)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateProject: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var locked bool
	err = tx.QueryRow(ctx, `SELECT true FROM orgs WHERE id = $1 FOR NO KEY UPDATE`, orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrgNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.CreateProject: lock org: %w", err)
	}
	p := &domain.Project{ID: id, OrgID: orgID, Name: name}
	err = tx.QueryRow(ctx, `INSERT INTO projects (id, org_id, name) VALUES ($1, $2, $3) RETURNING created_at`,
		id, orgID, name).Scan(&p.CreatedAt)
	if err != nil {
		return nil, projectWriteError("CreateProject", err)
	}
	if maxProjects > 0 {
		var held int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM projects WHERE org_id = $1`, orgID).Scan(&held); err != nil {
			return nil, fmt.Errorf("accounts_store.CreateProject: count projects: %w", err)
		}
		if held > maxProjects {
			return nil, ErrMaxProjectsReached
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("accounts_store.CreateProject: commit: %w", err)
	}
	return p, nil
}

func (s *AccountsStore) RenameProject(ctx context.Context, id, name string) error {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE projects SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return projectWriteError("RenameProject", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

func projectWriteError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return ErrProjectNameTaken
		case pgForeignKeyViolation:
			return ErrOrgNotFound
		}
	}
	return fmt.Errorf("accounts_store.%s: %w", op, err)
}

// GetProject returns a project, or nil when there is none.
func (s *AccountsStore) GetProject(ctx context.Context, id string) (*domain.Project, error) {
	p := &domain.Project{}
	err := s.pool.pool.QueryRow(ctx, `SELECT id, org_id, name, created_at FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.GetProject: %w", err)
	}
	return p, nil
}

// ListProjects returns the projects of the given orgs, oldest first within each org.
func (s *AccountsStore) ListProjects(ctx context.Context, orgIDs []string) ([]*domain.Project, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT id, org_id, name, created_at FROM projects WHERE org_id = ANY($1) ORDER BY org_id, created_at, id`, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListProjects: %w", err)
	}
	defer rows.Close()
	out := []*domain.Project{}
	for rows.Next() {
		p := &domain.Project{}
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("accounts_store.ListProjects: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// apiKeySelect reads keys as they stand now. Whether a key works is decided here, at the read,
// from rows that can change after the key was made: revoked_at is the earliest revocation of the
// key or of any key it descends from, and orphaned says its creator could no longer create it.
// Deciding it at the read leaves no moment in which a key minted beside a revocation, or kept
// past its creator's demotion or removal, is live.
var apiKeySelect = `
SELECT k.id, k.org_id, COALESCE(k.project_id, ''), k.created_by, ` + userName("k.created_by") + `, COALESCE(k.minted_by, ''), k.name, k.created_at,
       (WITH RECURSIVE ancestry AS (
            SELECT k.minted_by AS parent, k.revoked_at
            UNION ALL
            SELECT p.minted_by, p.revoked_at FROM api_keys p JOIN ancestry a ON p.id = a.parent
        ) SELECT min(revoked_at) FROM ancestry),
       NOT ` + apiKeyHeldBy + `
FROM api_keys k `

// apiKeyHeldBy is the one statement of who may hold key k, read where a key is created and
// wherever one is read: staff, an owner of its org, and a member of its org when the key is
// bound to one project.
const apiKeyHeldBy = `(EXISTS (SELECT 1 FROM users u WHERE u.id = k.created_by AND u.staff)
        OR EXISTS (SELECT 1 FROM org_members m WHERE m.org_id = k.org_id AND m.user_id = k.created_by
                   AND (m.role = 'owner' OR k.project_id IS NOT NULL)))`

func scanAPIKey(row rowScanner) (*domain.APIKey, error) {
	k := &domain.APIKey{}
	k.Actor.Kind = domain.ActorUser
	if err := row.Scan(&k.ID, &k.OrgID, &k.ProjectID, &k.Actor.ID, &k.Actor.Name, &k.MintedBy, &k.Name, &k.CreatedAt, &k.RevokedAt, &k.Orphaned); err != nil {
		return nil, err
	}
	return k, nil
}

// CreateAPIKey stores a key under the hash of its token. key.ID, OrgID, Actor.ID and Name are
// required; the project, when set, must belong to the org, and the creator must be one who may
// hold the key (apiKeyHeldBy), which is what the key is held to for as long as it lives.
func (s *AccountsStore) CreateAPIKey(ctx context.Context, key *domain.APIKey, keyHash string) error {
	var projectID, mintedBy *string
	if key.MintedBy != "" {
		mintedBy = &key.MintedBy
	}
	if key.ProjectID != "" {
		project, err := s.GetProject(ctx, key.ProjectID)
		if err != nil {
			return fmt.Errorf("accounts_store.CreateAPIKey: %w", err)
		}
		if project == nil || project.OrgID != key.OrgID {
			return ErrProjectNotFound
		}
		projectID = &key.ProjectID
	}
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO api_keys (id, key_hash, org_id, project_id, created_by, minted_by, name)
SELECT k.id, k.key_hash, k.org_id, k.project_id, k.created_by, k.minted_by, k.name
FROM (SELECT $1::text AS id, $2::text AS key_hash, $3::text AS org_id, $4::text AS project_id,
             $5::text AS created_by, $6::text AS minted_by, $7::text AS name) k
WHERE `+apiKeyHeldBy+`
RETURNING created_at, `+userName("created_by"), key.ID, keyHash, key.OrgID, projectID, key.Actor.ID, mintedBy, key.Name).Scan(&key.CreatedAt, &key.Actor.Name)
	key.Actor.Kind = domain.ActorUser
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAPIKeyCreatorLacksOrg
	}
	if err != nil {
		return fmt.Errorf("accounts_store.CreateAPIKey: %w", err)
	}
	return nil
}

// ListAPIKeys returns an org's keys, revoked ones included, newest first.
func (s *AccountsStore) ListAPIKeys(ctx context.Context, orgID string) ([]*domain.APIKey, error) {
	rows, err := s.pool.pool.Query(ctx, apiKeySelect+`WHERE k.org_id = $1 ORDER BY k.created_at DESC, k.id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.ListAPIKeys: %w", err)
	}
	defer rows.Close()
	out := []*domain.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("accounts_store.ListAPIKeys: scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetAPIKey returns a key by id, or nil when there is none.
func (s *AccountsStore) GetAPIKey(ctx context.Context, id string) (*domain.APIKey, error) {
	return s.apiKeyWhere(ctx, "GetAPIKey", `k.id = $1`, id)
}

// APIKeyByHash returns the key stored under a token hash, working or not, or nil when there is none.
func (s *AccountsStore) APIKeyByHash(ctx context.Context, keyHash string) (*domain.APIKey, error) {
	return s.apiKeyWhere(ctx, "APIKeyByHash", `k.key_hash = $1`, keyHash)
}

func (s *AccountsStore) apiKeyWhere(ctx context.Context, op, where, arg string) (*domain.APIKey, error) {
	k, err := scanAPIKey(s.pool.pool.QueryRow(ctx, apiKeySelect+`WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.%s: %w", op, err)
	}
	return k, nil
}

// RevokeAPIKey ends a key. Every key minted from it, directly or through another, ends with it:
// a key is read as revoked while any key it descends from is (apiKeySelect). One already revoked
// keeps its first revocation time.
func (s *AccountsStore) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("accounts_store.RevokeAPIKey: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// PlatformExperimentTenancy places a platform experiment and reports when it closed, nil while
// it has not. A nil tenancy means there is no such platform experiment.
func (s *AccountsStore) PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (*domain.Tenancy, *time.Time, error) {
	t := &domain.Tenancy{PlatformExperimentID: platformExperimentID}
	var closedAt *time.Time
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.project_id, p.org_id, pe.shared, pe.closed_at
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
WHERE pe.id = $1`, platformExperimentID).Scan(&t.ProjectID, &t.OrgID, &t.Shared, &closedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("accounts_store.PlatformExperimentTenancy: %w", err)
	}
	return t, closedAt, nil
}

// ExperimentTenancy places a job through its platform experiment, names the agent it belongs to
// and reports whether the job has ended. nil when there is no such job.
func (s *AccountsStore) ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error) {
	t := &domain.Tenancy{}
	var terminal bool
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.id, pe.project_id, p.org_id, pe.shared, e.agent_id, e.status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED')
FROM experiments e
JOIN platform_experiments pe ON pe.id = e.platform_experiment_id
JOIN projects p ON p.id = pe.project_id
WHERE e.id = $1`, experimentID).Scan(&t.PlatformExperimentID, &t.ProjectID, &t.OrgID, &t.Shared, &t.AgentID, &terminal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("accounts_store.ExperimentTenancy: %w", err)
	}
	return t, terminal, nil
}

// DonationTenancy places a donation request through its platform experiment and names the agent
// that asked. nil when there is no such request.
func (s *AccountsStore) DonationTenancy(ctx context.Context, donationID string) (*domain.Tenancy, error) {
	t := &domain.Tenancy{}
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.id, pe.project_id, p.org_id, pe.shared, d.agent_id
FROM donation_requests d
JOIN platform_experiments pe ON pe.id = d.platform_experiment_id
JOIN projects p ON p.id = pe.project_id
WHERE d.id = $1`, donationID).Scan(&t.PlatformExperimentID, &t.ProjectID, &t.OrgID, &t.Shared, &t.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accounts_store.DonationTenancy: %w", err)
	}
	return t, nil
}

// AgentOrg returns the org an agent was registered in; found is false for an unknown agent.
func (s *AccountsStore) AgentOrg(ctx context.Context, agentID string) (orgID string, found bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `SELECT org_id FROM agents WHERE id = $1`, agentID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("accounts_store.AgentOrg: %w", err)
	}
	return orgID, true, nil
}

// PlatformExperimentIDs lists every platform experiment inside scope, for stores that hold only
// the platform experiment id and cannot join to it.
func (s *AccountsStore) PlatformExperimentIDs(ctx context.Context, scope domain.TenantScope) ([]string, error) {
	q := `SELECT pe.id FROM platform_experiments pe`
	clause, args := tenantClause(scope, "pe.id", nil)
	if clause != "" {
		q += ` WHERE ` + clause
	}
	rows, err := s.pool.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.PlatformExperimentIDs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("accounts_store.PlatformExperimentIDs: %w", err)
	}
	return ids, nil
}
