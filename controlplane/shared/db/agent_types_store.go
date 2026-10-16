package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	// ErrAgentTypeNotFound is an agent type that does not exist or is another org's.
	ErrAgentTypeNotFound = errors.New("agent type not found")
	// ErrAgentTypeBuiltin refuses a write to a built-in agent type, which only the repository changes.
	ErrAgentTypeBuiltin = errors.New("a built-in agent type is read-only")
	// ErrAgentTypeArchived refuses an edit of an archived agent type.
	ErrAgentTypeArchived = errors.New("the agent type is archived")
	// ErrUnknownAgentType refuses an agent pinned to an agent type that is not one of its org's, of
	// its role, and not archived.
	ErrUnknownAgentType = errors.New("no such agent type for this agent")
)

// AgentTypeVersionConflict refuses an edit made from a version that is no longer the current one.
type AgentTypeVersionConflict struct {
	IfVersion, Current int
}

func (e *AgentTypeVersionConflict) Error() string {
	return fmt.Sprintf("if_version is %d and the agent type is at version %d: read it again and edit from there", e.IfVersion, e.Current)
}

// AgentTypeInUse refuses to archive an agent type a platform experiment that is not closed uses.
type AgentTypeInUse struct {
	PlatformExperimentIDs []string
}

func (e *AgentTypeInUse) Error() string {
	return fmt.Sprintf("platform experiments %v that are not closed use the agent type", e.PlatformExperimentIDs)
}

// AgentTypesStore holds the agent types: the built-in ones and those each org made, every version
// of each, and the edits that move the agents running one to its next version.
type AgentTypesStore struct {
	pool *Pool
}

// NewAgentTypesStore returns an AgentTypesStore backed by pool.
func NewAgentTypesStore(pool *Pool) *AgentTypesStore {
	return &AgentTypesStore{pool: pool}
}

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// liveSignupSQL is whether the run of signup s goes on: it did not end (runEndSQL), its machine
// was not given up on and nobody was added in its place.
const liveSignupSQL = `(` + runEndSQL + `) = '' AND NOT (s.hosted AND s.instance_stopped) AND NOT ` + replacedSQL

// liveAgentsSQL counts the agents and the coordinators whose run goes on that run agent type t.
const liveAgentsSQL = `((SELECT count(*) FROM experiment_signups s WHERE s.flavor = t.id AND ` + liveSignupSQL + `)
  + (SELECT count(*) FROM platform_experiments cpe WHERE cpe.coordinator_agent_type = t.id AND cpe.status <> 'closed'))::int`

const currentAgentTypeColumns = `t.id, COALESCE(t.org_id, ''), t.name, t.role, t.current_version, t.archived_at, t.created_by_kind, t.created_by_id, t.created_at,
       v.version, v.prompt, v.settings, v.digest, v.created_by_kind, v.created_by_id, v.created_at, ` + liveAgentsSQL + `
FROM agent_types t JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version`

func scanCurrentAgentType(row pgx.Row) (domain.CurrentAgentType, error) {
	var t domain.CurrentAgentType
	var typeBy, versionBy domain.Actor
	var settings []byte
	if err := row.Scan(&t.ID, &t.OrgID, &t.Name, &t.Role, &t.CurrentVersion, &t.ArchivedAt, &typeBy.Kind, &typeBy.ID, &t.CreatedAt,
		&t.Current.Version, &t.Current.Prompt, &settings, &t.Current.Digest, &versionBy.Kind, &versionBy.ID, &t.Current.CreatedAt, &t.LiveAgents); err != nil {
		return t, err
	}
	t.Builtin, t.Current.TypeID = t.OrgID == "", t.ID
	t.CreatedBy, t.Current.CreatedBy = storedBy(typeBy), storedBy(versionBy)
	return t, json.Unmarshal(settings, &t.Current.Settings)
}

func storedBy(a domain.Actor) *domain.Actor {
	if a.Kind == "" {
		return nil
	}
	a.Name = a.ID
	return &a
}

// readCatalog is the agent types the agents of org orgID may run as, built-in ones first, with
// their current versions: archived ones too, which LookupAgentType passes over.
func readCatalog(ctx context.Context, q rowsQuerier, orgID string) (domain.AgentTypeCatalog, error) {
	rows, err := q.Query(ctx, `SELECT `+currentAgentTypeColumns+`
WHERE t.org_id IS NULL OR t.org_id = $1 ORDER BY t.org_id NULLS FIRST, t.created_at, t.id`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.CurrentAgentType, error) { return scanCurrentAgentType(row) })
}

// List is the agent types of org orgID: the built-in ones and its own.
func (s *AgentTypesStore) List(ctx context.Context, orgID string) (domain.AgentTypeCatalog, error) {
	catalog, err := readCatalog(ctx, s.pool.pool, orgID)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.List: %w", err)
	}
	return catalog, nil
}

// Get is agent type id as org orgID reads it: a built-in one or its own. ErrAgentTypeNotFound for
// any other.
func (s *AgentTypesStore) Get(ctx context.Context, orgID, id string) (*domain.CurrentAgentType, error) {
	t, err := getAgentType(ctx, s.pool.pool, orgID, id, false)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Get: %w", err)
	}
	return t, nil
}

func getAgentType(ctx context.Context, q rowsQuerier, orgID, id string, lock bool) (*domain.CurrentAgentType, error) {
	query := `SELECT ` + currentAgentTypeColumns + ` WHERE t.id = $1 AND (t.org_id IS NULL OR t.org_id = $2)`
	if lock {
		query += ` FOR UPDATE OF t`
	}
	t, err := scanCurrentAgentType(q.QueryRow(ctx, query, id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentTypeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const agentTypeVersionColumns = `v.type_id, v.version, v.prompt, v.settings, v.digest, v.created_by_kind, v.created_by_id, v.created_at
FROM agent_type_versions v JOIN agent_types t ON t.id = v.type_id`

func scanAgentTypeVersion(row pgx.Row) (domain.AgentTypeVersion, error) {
	var v domain.AgentTypeVersion
	var by domain.Actor
	var settings []byte
	if err := row.Scan(&v.TypeID, &v.Version, &v.Prompt, &settings, &v.Digest, &by.Kind, &by.ID, &v.CreatedAt); err != nil {
		return v, err
	}
	v.CreatedBy = storedBy(by)
	return v, json.Unmarshal(settings, &v.Settings)
}

// Versions is every version of agent type id as org orgID reads it, the newest first.
func (s *AgentTypesStore) Versions(ctx context.Context, orgID, id string) ([]domain.AgentTypeVersion, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT `+agentTypeVersionColumns+`
WHERE v.type_id = $1 AND (t.org_id IS NULL OR t.org_id = $2) ORDER BY v.version DESC`, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Versions: %w", err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.AgentTypeVersion, error) { return scanAgentTypeVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Versions: %w", err)
	}
	if len(versions) == 0 {
		return nil, ErrAgentTypeNotFound
	}
	return versions, nil
}

// Version is version n of agent type id as org orgID reads it.
func (s *AgentTypesStore) Version(ctx context.Context, orgID, id string, n int) (*domain.AgentTypeVersion, error) {
	v, err := scanAgentTypeVersion(s.pool.pool.QueryRow(ctx, `SELECT `+agentTypeVersionColumns+`
WHERE v.type_id = $1 AND v.version = $2 AND (t.org_id IS NULL OR t.org_id = $3)`, id, n, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentTypeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Version: %w", err)
	}
	return &v, nil
}

func insertAgentTypeVersion(ctx context.Context, tx pgx.Tx, typeID string, version int, prompt string, settings domain.AgentTypeSettings, by domain.Actor) error {
	digest, err := domain.AgentTypeDigest(prompt, settings)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO agent_type_versions (type_id, version, prompt, settings, digest, created_by_kind, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, typeID, version, prompt, encoded, digest, string(by.Kind), by.ID)
	return err
}

// Create makes an agent type of org orgID at version 1. prompt and settings are checked by the
// caller (domain.ValidatePrompt, AgentTypeSettings.Validate).
func (s *AgentTypesStore) Create(ctx context.Context, orgID, name string, role domain.AgentRole, prompt string, settings domain.AgentTypeSettings, by domain.Actor) (*domain.CurrentAgentType, error) {
	id, err := domain.NewID(domain.AgentTypeIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: %w", err)
	}
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_types (id, org_id, name, role, current_version, created_by_kind, created_by_id)
VALUES ($1, $2, $3, $4, 1, $5, $6)`, id, orgID, name, string(role), string(by.Kind), by.ID); err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: %w", err)
	}
	if err := insertAgentTypeVersion(ctx, tx, id, 1, prompt, settings, by); err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: %w", err)
	}
	created, err := getAgentType(ctx, tx, orgID, id, false)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: commit: %w", err)
	}
	return created, nil
}

// AgentTypeEdit is a new version of an org's agent type: its name, which is no part of a version,
// and the prompt and settings of the version, made from version IfVersion.
type AgentTypeEdit struct {
	OrgID     string
	ID        string
	IfVersion int
	Name      string
	Prompt    string
	Settings  domain.AgentTypeSettings
	By        domain.Actor
}

// Edit inserts the next version of an agent type of org e.OrgID, makes it current and moves every
// live agent of the type to it (propagateTx), all in one transaction. A prompt and settings the
// current version already has insert nothing and move nobody. ErrAgentTypeNotFound for a type
// that is not the org's, ErrAgentTypeBuiltin for a built-in one, ErrAgentTypeArchived, and an
// *AgentTypeVersionConflict when IfVersion is not current.
func (s *AgentTypesStore) Edit(ctx context.Context, e AgentTypeEdit, defaults domain.ModelDefaults) (*domain.CurrentAgentType, *domain.Propagation, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := getAgentType(ctx, tx, e.OrgID, e.ID, true)
	if err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: %w", err)
	}
	switch {
	case current.Builtin:
		return nil, nil, ErrAgentTypeBuiltin
	case current.ArchivedAt != nil:
		return nil, nil, ErrAgentTypeArchived
	case current.CurrentVersion != e.IfVersion:
		return nil, nil, &AgentTypeVersionConflict{IfVersion: e.IfVersion, Current: current.CurrentVersion}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET name = $2 WHERE id = $1`, e.ID, e.Name); err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: rename: %w", err)
	}
	digest, err := domain.AgentTypeDigest(e.Prompt, e.Settings)
	if err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: %w", err)
	}
	propagation := &domain.Propagation{Version: current.CurrentVersion}
	if digest != current.Current.Digest {
		if propagation, err = newVersionTx(ctx, tx, current, e.Prompt, e.Settings, e.By, defaults); err != nil {
			return nil, nil, fmt.Errorf("agent_types_store.Edit: %w", err)
		}
	}
	edited, err := getAgentType(ctx, tx, e.OrgID, e.ID, false)
	if err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("agent_types_store.Edit: commit: %w", err)
	}
	return edited, propagation, nil
}

// newVersionTx inserts the version after current's, whose type row the transaction locked, makes it
// current and moves the type's live agents to it.
func newVersionTx(ctx context.Context, tx pgx.Tx, current *domain.CurrentAgentType, prompt string, settings domain.AgentTypeSettings, by domain.Actor, defaults domain.ModelDefaults) (*domain.Propagation, error) {
	next := current.CurrentVersion + 1
	if err := insertAgentTypeVersion(ctx, tx, current.ID, next, prompt, settings, by); err != nil {
		return nil, fmt.Errorf("insert version %d: %w", next, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET current_version = $2 WHERE id = $1`, current.ID, next); err != nil {
		return nil, fmt.Errorf("make version %d current: %w", next, err)
	}
	return propagateTx(ctx, tx, current.ID, current.Role, current.Current.Settings, next, settings, defaults)
}

// propagateTx moves every live agent of agent type typeID to version, each in a savepoint of its
// own so that one that fails stays on the version it had and the others move: a signup or a
// coordinator whose run goes on, with instructions_xid written for the replay of
// agent.instructions. An agent that runs on the model the old settings resolved to, or with
// their hyperparameters, takes the new ones, as do the entries of the type in the researchers of
// platform experiments that are not closed.
func propagateTx(ctx context.Context, tx pgx.Tx, typeID string, role domain.AgentRole, old domain.AgentTypeSettings, version int, settings domain.AgentTypeSettings, defaults domain.ModelDefaults) (*domain.Propagation, error) {
	oldModel, newModel := old.ResolvedModel(defaults.For(role)), settings.ResolvedModel(defaults.For(role))
	oldHyperparameters, err := json.Marshal(nonNilObject(old.Hyperparameters))
	if err != nil {
		return nil, err
	}
	newHyperparameters, err := json.Marshal(nonNilObject(settings.Hyperparameters))
	if err != nil {
		return nil, err
	}
	propagation := &domain.Propagation{Version: version, Moved: []domain.PinMove{}, Failed: []domain.PinMove{}}
	var live []domain.PinMove
	var move func(domain.PinMove) error
	if role == domain.AgentRoleCoordinator {
		live, err = collectPins(ctx, tx, `SELECT id, '`+domain.CoordinatorRecipient+`' FROM platform_experiments
WHERE coordinator_agent_type = $1 AND status <> 'closed' ORDER BY id`, typeID)
		move = func(pin domain.PinMove) error {
			_, err := tx.Exec(ctx, `
UPDATE platform_experiments SET coordinator_agent_type_version = $2, coordinator_instructions_xid = pg_current_xact_id()::text::bigint,
    coordinator_model = CASE WHEN coordinator_model = $3 THEN $4 ELSE coordinator_model END
WHERE id = $1`, pin.PlatformExperimentID, version, oldModel, newModel)
			return err
		}
	} else {
		live, err = collectPins(ctx, tx, `SELECT s.platform_experiment_id, s.agent_id FROM experiment_signups s
WHERE s.flavor = $1 AND `+liveSignupSQL+` ORDER BY 1, 2`, typeID)
		move = func(pin domain.PinMove) error {
			_, err := tx.Exec(ctx, `
UPDATE experiment_signups SET agent_type_version = $3, instructions_xid = pg_current_xact_id()::text::bigint,
    model = CASE WHEN model = $4 THEN $5 ELSE model END,
    hyperparameters = CASE WHEN hyperparameters = $6::jsonb THEN $7::jsonb ELSE hyperparameters END
WHERE platform_experiment_id = $1 AND agent_id = $2`, pin.PlatformExperimentID, pin.AgentID, version, oldModel, newModel, oldHyperparameters, newHyperparameters)
			return err
		}
	}
	if err != nil {
		return nil, err
	}
	for _, pin := range live {
		if err := inSavepoint(ctx, tx, func() error { return move(pin) }); err != nil {
			pin.Error = err.Error()
			propagation.Failed = append(propagation.Failed, pin)
			continue
		}
		propagation.Moved = append(propagation.Moved, pin)
	}
	if role != domain.AgentRoleResearcher || (oldModel == newModel && string(oldHyperparameters) == string(newHyperparameters)) {
		return propagation, nil
	}
	pools, err := collectPins(ctx, tx, `SELECT id, '' FROM platform_experiments
WHERE status <> 'closed' AND researchers @> jsonb_build_array(jsonb_build_object('agent_type', $1::text)) ORDER BY id`, typeID)
	if err != nil {
		return nil, err
	}
	for _, pin := range pools {
		if err := inSavepoint(ctx, tx, func() error {
			pool, err := lockedPoolTx(ctx, tx, pin.PlatformExperimentID)
			if err != nil {
				return err
			}
			encoded, err := poolJSON(pool.Followed(typeID, oldModel, newModel, old.Hyperparameters, settings.Hyperparameters))
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE platform_experiments SET researchers = $2 WHERE id = $1`, pin.PlatformExperimentID, encoded)
			return err
		}); err != nil {
			pin.Error = err.Error()
			propagation.Failed = append(propagation.Failed, pin)
		}
	}
	return propagation, nil
}

func collectPins(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]domain.PinMove, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.PinMove, error) {
		var pin domain.PinMove
		err := row.Scan(&pin.PlatformExperimentID, &pin.AgentID)
		return pin, err
	})
}

// inSavepoint runs write in a savepoint of tx, rolled back to when write fails.
func inSavepoint(ctx context.Context, tx pgx.Tx, write func() error) error {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		if rollback := savepoint.Rollback(ctx); rollback != nil {
			return errors.Join(err, rollback)
		}
		return err
	}
	return savepoint.Commit(ctx)
}

// Archive archives agent type id of org orgID: no new agent is pinned to it, and the agents that
// are keep running. Archiving one archived changes nothing. ErrAgentTypeBuiltin for a built-in type,
// and an *AgentTypeInUse while a platform experiment that is not closed names it in its
// researchers, for its coordinator or for one of its agents.
func (s *AgentTypesStore) Archive(ctx context.Context, orgID, id string) (*domain.CurrentAgentType, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	current, err := getAgentType(ctx, tx, orgID, id, true)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: %w", err)
	}
	if current.Builtin {
		return nil, ErrAgentTypeBuiltin
	}
	rows, err := tx.Query(ctx, `
SELECT pe.id FROM platform_experiments pe
WHERE pe.status <> 'closed' AND (
    pe.coordinator_agent_type = $1
    OR pe.researchers @> jsonb_build_array(jsonb_build_object('agent_type', $1::text))
    OR EXISTS (SELECT 1 FROM experiment_signups s WHERE s.platform_experiment_id = pe.id AND s.flavor = $1))
ORDER BY pe.id`, id)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: %w", err)
	}
	using, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: %w", err)
	}
	if len(using) > 0 {
		return nil, &AgentTypeInUse{PlatformExperimentIDs: using}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET archived_at = COALESCE(archived_at, now()) WHERE id = $1`, id); err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: %w", err)
	}
	archived, err := getAgentType(ctx, tx, orgID, id, false)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("agent_types_store.Archive: commit: %w", err)
	}
	return archived, nil
}

// SeededAgentType is a built-in type the seeding gave a new version, and what that did to its agents.
type SeededAgentType struct {
	ID          string
	Propagation domain.Propagation
}

// SeedBuiltinAgentTypes writes the built-in agent types as the repository has them, under the lock
// Migrate takes, in one transaction: a type that does not exist yet is created at version 1, and
// one whose prompt and settings match none of its versions gets the next version, which its live
// agents move to (propagateTx). Matching any version, not just the current one, keeps an older
// replica of a rolling deploy from writing its own back. Then every agent and coordinator from
// before agent types is pinned to its type's current version.
func SeedBuiltinAgentTypes(ctx context.Context, pool *Pool, builtins []agenttypes.Builtin, defaults domain.ModelDefaults) ([]SeededAgentType, error) {
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaMigrationLockKey); err != nil {
		return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: lock: %w", err)
	}
	var seeded []SeededAgentType
	for _, b := range builtins {
		propagation, err := seedBuiltinTx(ctx, tx, b, defaults)
		if err != nil {
			return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: %s: %w", b.ID, err)
		}
		if propagation != nil {
			seeded = append(seeded, SeededAgentType{ID: b.ID, Propagation: *propagation})
		}
	}
	if _, err := tx.Exec(ctx, `
UPDATE experiment_signups s SET agent_type_version = t.current_version
FROM agent_types t WHERE s.agent_type_version IS NULL AND t.id = s.flavor`); err != nil {
		return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: pin agents: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE platform_experiments pe SET coordinator_agent_type_version = t.current_version
FROM agent_types t WHERE pe.coordinator_agent_type_version IS NULL AND t.id = pe.coordinator_agent_type`); err != nil {
		return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: pin coordinators: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("db.SeedBuiltinAgentTypes: commit: %w", err)
	}
	return seeded, nil
}

// seedBuiltinTx writes one built-in type, and the propagation of the version it inserted after the
// first, nil when it inserted none.
func seedBuiltinTx(ctx context.Context, tx pgx.Tx, b agenttypes.Builtin, defaults domain.ModelDefaults) (*domain.Propagation, error) {
	platform := domain.Actor{Kind: domain.ActorStaff}
	tag, err := tx.Exec(ctx, `
INSERT INTO agent_types (id, org_id, name, role, current_version, created_by_kind) VALUES ($1, NULL, $2, $3, 1, $4)
ON CONFLICT (id) DO NOTHING`, b.ID, b.Name, string(b.Role), string(platform.Kind))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, insertAgentTypeVersion(ctx, tx, b.ID, 1, b.Prompt, b.Settings, platform)
	}
	current, err := getAgentType(ctx, tx, "", b.ID, true)
	if errors.Is(err, ErrAgentTypeNotFound) {
		return nil, fmt.Errorf("an org's agent type holds the built-in id")
	}
	if err != nil {
		return nil, err
	}
	if current.Role != b.Role {
		return nil, fmt.Errorf("the built-in is of role %s and the stored one of %s: a type's role is fixed", b.Role, current.Role)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET name = $2 WHERE id = $1 AND name <> $2`, b.ID, b.Name); err != nil {
		return nil, err
	}
	digest, err := domain.AgentTypeDigest(b.Prompt, b.Settings)
	if err != nil {
		return nil, err
	}
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_type_versions WHERE type_id = $1 AND digest = $2)`, b.ID, digest).Scan(&known); err != nil {
		return nil, err
	}
	if known {
		return nil, nil
	}
	return newVersionTx(ctx, tx, current, b.Prompt, b.Settings, platform, defaults)
}

// agentTypePinTx is the agent type a new agent of role in platform experiment platformExpID runs,
// and the version it is pinned to: the current one of agent type typeID, or of the role's built-in
// one for none, when it is a built-in or one of the experiment's org, of that role and not
// archived. ErrUnknownAgentType for any other.
func agentTypePinTx(ctx context.Context, q rowQuerier, platformExpID, typeID string, role domain.AgentRole) (string, int, error) {
	if typeID == "" {
		typeID = domain.DefaultAgentType(role)
	}
	var version int
	err := q.QueryRow(ctx, `
SELECT t.current_version FROM agent_types t
WHERE t.id = $1 AND t.role = $2 AND t.archived_at IS NULL
  AND (t.org_id IS NULL OR t.org_id = (SELECT p.org_id FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id WHERE pe.id = $3))`,
		typeID, string(role), platformExpID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, fmt.Errorf("%w: %q is no %s agent type of the org of %s", ErrUnknownAgentType, typeID, role, platformExpID)
	}
	return typeID, version, err
}

// AgentTypeCatalog is the agent types the agents of platform experiment platformExpID may run as.
func (s *PlatformExperimentsStore) AgentTypeCatalog(ctx context.Context, platformExpID string) (domain.AgentTypeCatalog, error) {
	var orgID string
	err := s.pool.pool.QueryRow(ctx, `SELECT p.org_id FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id WHERE pe.id = $1`, platformExpID).Scan(&orgID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentTypeCatalog: org of %s: %w", platformExpID, err)
	}
	catalog, err := readCatalog(ctx, s.pool.pool, orgID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentTypeCatalog: %w", err)
	}
	return catalog, nil
}

// ProjectAgentTypeCatalog is the agent types the agents of a platform experiment of project
// projectID may run as.
func (s *PlatformExperimentsStore) ProjectAgentTypeCatalog(ctx context.Context, projectID string) (domain.AgentTypeCatalog, error) {
	var orgID string
	if err := s.pool.pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, projectID).Scan(&orgID); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ProjectAgentTypeCatalog: org of %s: %w", projectID, err)
	}
	catalog, err := readCatalog(ctx, s.pool.pool, orgID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ProjectAgentTypeCatalog: %w", err)
	}
	return catalog, nil
}

// InstructionsSource is what an agent's or a coordinator's instructions are composed from: the
// version it is pinned to, and the values of its own the platform fills in.
type InstructionsSource struct {
	Role            domain.AgentRole
	Version         domain.AgentTypeVersion
	Model           string
	Hyperparameters map[string]any
	RepositoryURL   string
}

// AgentInstructions is what the instructions of agent agentID of platform experiment platformExpID
// are composed from, nil for an agent that is not in it.
func (s *PlatformExperimentsStore) AgentInstructions(ctx context.Context, platformExpID, agentID string) (*InstructionsSource, error) {
	src, err := readInstructions(s.pool.pool.QueryRow(ctx, `
SELECT t.role, `+agentTypeVersionColumnsOnly+`, s.model, s.hyperparameters, pe.repository_url
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
JOIN agent_types t ON t.id = s.flavor
JOIN agent_type_versions v ON v.type_id = s.flavor AND v.version = s.agent_type_version
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2`, platformExpID, agentID))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentInstructions: %w", err)
	}
	return src, nil
}

// CoordinatorInstructions is what the instructions of the coordinator of platform experiment
// platformExpID are composed from, nil for no such platform experiment.
func (s *PlatformExperimentsStore) CoordinatorInstructions(ctx context.Context, platformExpID string) (*InstructionsSource, error) {
	src, err := readInstructions(s.pool.pool.QueryRow(ctx, `
SELECT t.role, `+agentTypeVersionColumnsOnly+`, pe.coordinator_model, '{}'::jsonb, pe.repository_url
FROM platform_experiments pe
JOIN agent_types t ON t.id = pe.coordinator_agent_type
JOIN agent_type_versions v ON v.type_id = pe.coordinator_agent_type AND v.version = pe.coordinator_agent_type_version
WHERE pe.id = $1`, platformExpID))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.CoordinatorInstructions: %w", err)
	}
	return src, nil
}

const agentTypeVersionColumnsOnly = `v.type_id, v.version, v.prompt, v.settings, v.digest, v.created_by_kind, v.created_by_id, v.created_at`

func readInstructions(row pgx.Row) (*InstructionsSource, error) {
	var src InstructionsSource
	var by domain.Actor
	var settings, hyperparameters []byte
	err := row.Scan(&src.Role, &src.Version.TypeID, &src.Version.Version, &src.Version.Prompt, &settings, &src.Version.Digest,
		&by.Kind, &by.ID, &src.Version.CreatedAt, &src.Model, &hyperparameters, &src.RepositoryURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	src.Version.CreatedBy = storedBy(by)
	if err := json.Unmarshal(settings, &src.Version.Settings); err != nil {
		return nil, err
	}
	return &src, json.Unmarshal(hyperparameters, &src.Hyperparameters)
}
