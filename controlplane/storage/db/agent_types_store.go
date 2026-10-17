package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var (
	// ErrAgentTypeNotFound is an agent type that does not exist or is another org's, or a version
	// of one it does not have.
	ErrAgentTypeNotFound = errors.New("agent type not found")
	// ErrAgentTypeBuiltin refuses a write to a built-in agent type, which only the repository changes.
	ErrAgentTypeBuiltin = errors.New("a built-in agent type is read-only")
	// ErrAgentTypeArchived refuses an edit of an archived agent type.
	ErrAgentTypeArchived = errors.New("the agent type is archived")
	// ErrUnknownAgentType refuses an agent or a pool entry naming an agent type that is not one of
	// its org's, of its role, and not archived.
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

// AgentTypeLimit refuses a write above one of the limits on an org's agent types: Types for the
// types it has that are not archived (domain.MaxOrgAgentTypes), else the versions made of one type
// in an hour (domain.MaxAgentTypeEditsAnHour).
type AgentTypeLimit struct {
	Types bool
	Limit int
}

func (e *AgentTypeLimit) Error() string {
	if e.Types {
		return fmt.Sprintf("the org has %d agent types that are not archived, the most it may: archive one it no longer uses", e.Limit)
	}
	return fmt.Sprintf("the agent type had %d versions made in the last hour, the most it may: edit it again later", e.Limit)
}

// liveSignupSQL is whether the run of signup s goes on: it did not end (runEndSQL), its machine
// was not given up on and nobody was added in its place.
const liveSignupSQL = `(` + runEndSQL + `) = '' AND NOT ` + stoppedSQL + ` AND NOT ` + replacedSQL

const agentTypeVersionColumnsOnly = `v.type_id, v.version, v.prompt, v.settings, v.digest, v.release, v.created_by_kind, v.created_by_id, v.created_at`

const currentAgentTypeColumns = `t.id, COALESCE(t.org_id, ''), t.name, t.description, t.role, t.current_version, t.archived_at, t.created_by_kind, t.created_by_id, t.created_at,
       ` + agentTypeVersionColumnsOnly

const currentAgentTypeFrom = `
FROM agent_types t JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version`

func scanCurrentAgentType(row pgx.Row) (domain.CurrentAgentType, error) {
	var t domain.CurrentAgentType
	var typeBy domain.Actor
	scanVersion := versionScanner(&t.Current)
	if err := row.Scan(append([]any{&t.ID, &t.OrgID, &t.Name, &t.Description, &t.Role, &t.CurrentVersion, &t.ArchivedAt, &typeBy.Kind, &typeBy.ID, &t.CreatedAt},
		scanVersion.dest...)...); err != nil {
		return t, err
	}
	t.Builtin = t.OrgID == ""
	t.CreatedBy = storedBy(typeBy)
	return t, scanVersion.done()
}

// versionScan is where agentTypeVersionColumnsOnly is scanned into v, and done finishes v.
type versionScan struct {
	dest []any
	done func() error
}

func versionScanner(v *domain.AgentTypeVersion) versionScan {
	var by domain.Actor
	var settings []byte
	return versionScan{
		dest: []any{&v.TypeID, &v.Version, &v.Prompt, &settings, &v.Digest, &v.Release, &by.Kind, &by.ID, &v.CreatedAt},
		done: func() error {
			v.CreatedBy = storedBy(by)
			return json.Unmarshal(settings, &v.Settings)
		},
	}
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
func readCatalog(ctx context.Context, q queryer, orgID string) (domain.AgentTypeCatalog, error) {
	rows, err := q.Query(ctx, `SELECT `+currentAgentTypeColumns+currentAgentTypeFrom+`
WHERE t.org_id IS NULL OR t.org_id = $1 ORDER BY t.org_id NULLS FIRST, t.created_at, t.id`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.CurrentAgentType, error) { return scanCurrentAgentType(row) })
}

// List is the agent types of org orgID: the built-in ones and its own.
func (s *Store) List(ctx context.Context, orgID string) (domain.AgentTypeCatalog, error) {
	catalog, err := readCatalog(ctx, s.pool.pool, orgID)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.List: %w", err)
	}
	return catalog, nil
}

// Get is agent type id as org orgID reads it: a built-in one or its own. ErrAgentTypeNotFound for
// any other.
func (s *Store) Get(ctx context.Context, orgID, id string) (*domain.CurrentAgentType, error) {
	t, err := getAgentType(ctx, s.pool.pool, orgID, id, false)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Get: %w", err)
	}
	return t, nil
}

func getAgentType(ctx context.Context, q queryer, orgID, id string, lock bool) (*domain.CurrentAgentType, error) {
	query := `SELECT ` + currentAgentTypeColumns + currentAgentTypeFrom + ` WHERE t.id = $1 AND (t.org_id IS NULL OR t.org_id = $2)`
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

const agentTypeVersionColumns = agentTypeVersionColumnsOnly + `
FROM agent_type_versions v JOIN agent_types t ON t.id = v.type_id`

func scanAgentTypeVersion(row pgx.Row) (domain.AgentTypeVersion, error) {
	var v domain.AgentTypeVersion
	scan := versionScanner(&v)
	if err := row.Scan(scan.dest...); err != nil {
		return v, err
	}
	return v, scan.done()
}

// Versions is every version of agent type id as org orgID reads it, the newest first.
func (s *Store) Versions(ctx context.Context, orgID, id string) ([]domain.AgentTypeVersion, error) {
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
func (s *Store) Version(ctx context.Context, orgID, id string, n int) (*domain.AgentTypeVersion, error) {
	v, err := readVersion(ctx, s.pool.pool, orgID, id, n)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Version: %w", err)
	}
	return v, nil
}

func readVersion(ctx context.Context, q queryer, orgID, id string, n int) (*domain.AgentTypeVersion, error) {
	v, err := scanAgentTypeVersion(q.QueryRow(ctx, `SELECT `+agentTypeVersionColumns+`
WHERE v.type_id = $1 AND v.version = $2 AND (t.org_id IS NULL OR t.org_id = $3)`, id, n, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentTypeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Events is every change of agent type id as org orgID reads it, the newest first.
func (s *Store) Events(ctx context.Context, orgID, id string) ([]domain.AgentTypeEvent, error) {
	if _, err := getAgentType(ctx, s.pool.pool, orgID, id, false); err != nil {
		return nil, fmt.Errorf("agent_types_store.Events: %w", err)
	}
	rows, err := s.pool.pool.Query(ctx, `
SELECT e.id, e.type_id, e.action, e.version, e.detail, e.actor_kind, e.actor_id, e.at
FROM agent_type_events e WHERE e.type_id = $1 ORDER BY e.id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.AgentTypeEvent, error) {
		var e domain.AgentTypeEvent
		var by domain.Actor
		err := row.Scan(&e.ID, &e.TypeID, &e.Action, &e.Version, &e.Detail, &by.Kind, &by.ID, &e.At)
		e.By = storedBy(by)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Events: %w", err)
	}
	return events, nil
}

func recordEventTx(ctx context.Context, tx *Write, typeID string, action domain.AgentTypeChange, version int, detail string, by domain.Actor) error {
	_, err := tx.Exec(ctx, `
INSERT INTO agent_type_events (type_id, action, version, detail, actor_kind, actor_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		typeID, string(action), version, detail, string(by.Kind), by.ID)
	if err != nil {
		return fmt.Errorf("record %s of %s: %w", action, typeID, err)
	}
	return nil
}

func insertAgentTypeVersion(ctx context.Context, tx *Write, typeID string, version int, prompt string, settings domain.AgentTypeSettings, release *int, by domain.Actor) error {
	digest, err := domain.AgentTypeDigest(prompt, settings)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO agent_type_versions (type_id, version, prompt, settings, digest, release, created_by_kind, created_by_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, typeID, version, prompt, encoded, digest, release, string(by.Kind), by.ID)
	return err
}

// Create makes an agent type of org orgID at version 1. prompt and settings are checked by the
// caller (domain.ValidatePrompt, AgentTypeSettings.Validate), and so is its role
// (domain.CheckOrgAgentRole). An *AgentTypeLimit when the org has domain.MaxOrgAgentTypes that
// are not archived.
func (s *Store) Create(ctx context.Context, orgID, name, description string, role domain.AgentRole, prompt string, settings domain.AgentTypeSettings, by domain.Actor) (*domain.CurrentAgentType, error) {
	id, err := domain.NewID(domain.AgentTypeIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("agent_types_store.Create: %w", err)
	}
	var created *domain.CurrentAgentType
	if err := s.write(ctx, func(tx *Write) error {
		var types int
		if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_types WHERE org_id = o.id AND archived_at IS NULL)::int FROM orgs o WHERE o.id = $1 FOR UPDATE`, orgID).Scan(&types); err != nil {
			return fmt.Errorf("agent_types_store.Create: lock org %s: %w", orgID, err)
		}
		if types >= domain.MaxOrgAgentTypes {
			return &AgentTypeLimit{Types: true, Limit: domain.MaxOrgAgentTypes}
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO agent_types (id, org_id, name, description, role, current_version, created_by_kind, created_by_id)
VALUES ($1, $2, $3, $4, $5, 1, $6, $7)`, id, orgID, name, description, string(role), string(by.Kind), by.ID); err != nil {
			return fmt.Errorf("agent_types_store.Create: %w", err)
		}
		if err := insertAgentTypeVersion(ctx, tx, id, 1, prompt, settings, nil, by); err != nil {
			return fmt.Errorf("agent_types_store.Create: %w", err)
		}
		if err := recordEventTx(ctx, tx, id, domain.AgentTypeCreated, 1, "", by); err != nil {
			return fmt.Errorf("agent_types_store.Create: %w", err)
		}
		var err error
		created, err = getAgentType(ctx, tx, orgID, id, false)
		if err != nil {
			return fmt.Errorf("agent_types_store.Create: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return created, nil
}

// AgentTypeEdit is a new version of an org's agent type: its name and description, which are no
// part of a version, and the prompt and settings of the version, made from version IfVersion.
type AgentTypeEdit struct {
	OrgID       string
	ID          string
	IfVersion   int
	Name        string
	Description string
	Prompt      string
	Settings    domain.AgentTypeSettings
	By          domain.Actor
}

// lockOrgTypeTx locks agent type id of org orgID for a change: ErrAgentTypeNotFound for one that
// is not the org's, ErrAgentTypeBuiltin for a built-in one and ErrAgentTypeArchived.
func lockOrgTypeTx(ctx context.Context, tx *Write, orgID, id string) (*domain.CurrentAgentType, error) {
	current, err := getAgentType(ctx, tx, orgID, id, true)
	switch {
	case err != nil:
		return nil, err
	case current.Builtin:
		return nil, ErrAgentTypeBuiltin
	case current.ArchivedAt != nil:
		return nil, ErrAgentTypeArchived
	}
	return current, nil
}

// Edit renames, describes and makes the next version of an agent type of org e.OrgID current, all
// in one transaction, each change kept in agent_type_events. A prompt and settings the current
// version already has make no version and tell nobody; a new version tells every live agent of the
// type (tellTypeTx). The errors are lockOrgTypeTx's, an *AgentTypeVersionConflict when IfVersion
// is not current, and an *AgentTypeLimit above domain.MaxAgentTypeEditsAnHour.
func (s *Store) Edit(ctx context.Context, e AgentTypeEdit) (*domain.CurrentAgentType, *domain.Propagation, error) {
	var edited *domain.CurrentAgentType
	var propagation *domain.Propagation
	if err := s.write(ctx, func(tx *Write) error {
		current, err := lockOrgTypeTx(ctx, tx, e.OrgID, e.ID)
		if err != nil {
			return fmt.Errorf("agent_types_store.Edit: %w", err)
		}
		if current.CurrentVersion != e.IfVersion {
			return &AgentTypeVersionConflict{IfVersion: e.IfVersion, Current: current.CurrentVersion}
		}
		if err := describeTx(ctx, tx, current, e.Name, e.Description, e.By); err != nil {
			return fmt.Errorf("agent_types_store.Edit: %w", err)
		}
		digest, err := domain.AgentTypeDigest(e.Prompt, e.Settings)
		if err != nil {
			return fmt.Errorf("agent_types_store.Edit: %w", err)
		}
		propagation = &domain.Propagation{Version: current.CurrentVersion}
		if digest != current.Current.Digest {
			if err := withinEditLimitTx(ctx, tx, e.ID); err != nil {
				return err
			}
			if propagation, err = newVersionTx(ctx, tx, current, e.Prompt, e.Settings, nil, domain.AgentTypeEdited, "", e.By); err != nil {
				return fmt.Errorf("agent_types_store.Edit: %w", err)
			}
		}
		edited, err = getAgentType(ctx, tx, e.OrgID, e.ID, false)
		if err != nil {
			return fmt.Errorf("agent_types_store.Edit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return edited, propagation, nil
}

// Restore makes a new version of agent type id of org orgID equal to its version n, and current,
// as an edit does. Restoring what the current version already holds makes none. The errors are
// Edit's, and ErrAgentTypeNotFound for a version the type does not have.
func (s *Store) Restore(ctx context.Context, orgID, id string, n int, by domain.Actor) (*domain.CurrentAgentType, *domain.Propagation, error) {
	var restored *domain.CurrentAgentType
	var propagation *domain.Propagation
	if err := s.write(ctx, func(tx *Write) error {
		current, err := lockOrgTypeTx(ctx, tx, orgID, id)
		if err != nil {
			return fmt.Errorf("agent_types_store.Restore: %w", err)
		}
		from, err := readVersion(ctx, tx, orgID, id, n)
		if err != nil {
			return fmt.Errorf("agent_types_store.Restore: version %d: %w", n, err)
		}
		propagation = &domain.Propagation{Version: current.CurrentVersion}
		if from.Digest != current.Current.Digest {
			if err := withinEditLimitTx(ctx, tx, id); err != nil {
				return err
			}
			if propagation, err = newVersionTx(ctx, tx, current, from.Prompt, from.Settings, nil, domain.AgentTypeRestored, fmt.Sprintf("version %d", n), by); err != nil {
				return fmt.Errorf("agent_types_store.Restore: %w", err)
			}
		}
		restored, err = getAgentType(ctx, tx, orgID, id, false)
		if err != nil {
			return fmt.Errorf("agent_types_store.Restore: %w", err)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return restored, propagation, nil
}

// describeTx writes the name and description of current, whose row the transaction locked, each
// one that changed kept as a rename or a description.
func describeTx(ctx context.Context, tx *Write, current *domain.CurrentAgentType, name, description string, by domain.Actor) error {
	if name == current.Name && description == current.Description {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET name = $2, description = $3 WHERE id = $1`, current.ID, name, description); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	if name != current.Name {
		if err := recordEventTx(ctx, tx, current.ID, domain.AgentTypeRenamed, current.CurrentVersion, name, by); err != nil {
			return err
		}
	}
	if description != current.Description {
		return recordEventTx(ctx, tx, current.ID, domain.AgentTypeDescribed, current.CurrentVersion, description, by)
	}
	return nil
}

// withinEditLimitTx refuses another version of agent type id, whose row the transaction locked,
// once domain.MaxAgentTypeEditsAnHour were made in the last hour.
func withinEditLimitTx(ctx context.Context, tx *Write, id string) error {
	var made int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_type_versions WHERE type_id = $1 AND created_at > now() - interval '1 hour'`, id).Scan(&made); err != nil {
		return fmt.Errorf("count the versions of %s: %w", id, err)
	}
	if made >= domain.MaxAgentTypeEditsAnHour {
		return &AgentTypeLimit{Limit: domain.MaxAgentTypeEditsAnHour}
	}
	return nil
}

// newVersionTx inserts the version after current's, whose type row the transaction locked, makes it
// current, keeps the change as action and tells the type's live agents.
func newVersionTx(ctx context.Context, tx *Write, current *domain.CurrentAgentType, prompt string, settings domain.AgentTypeSettings, release *int, action domain.AgentTypeChange, detail string, by domain.Actor) (*domain.Propagation, error) {
	next := current.CurrentVersion + 1
	if err := insertAgentTypeVersion(ctx, tx, current.ID, next, prompt, settings, release, by); err != nil {
		return nil, fmt.Errorf("insert version %d: %w", next, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET current_version = $2 WHERE id = $1`, current.ID, next); err != nil {
		return nil, fmt.Errorf("make version %d current: %w", next, err)
	}
	if err := recordEventTx(ctx, tx, current.ID, action, next, detail, by); err != nil {
		return nil, err
	}
	told, err := tellTx(ctx, tx, `s.agent_type = $1`, `pe.coordinator_agent_type = $1`, current.ID)
	if err != nil {
		return nil, err
	}
	return &domain.Propagation{Version: next, Told: told}, nil
}

// tellTx tells agent.instructions, with the current version of its agent type, to every signup
// whose run goes on and every coordinator of a platform experiment that is not closed that
// signupWhere and coordinatorWhere hold for. It answers how many were told.
func tellTx(ctx context.Context, tx *Write, signupWhere, coordinatorWhere string, args ...any) (int, error) {
	agents, err := tx.Query(ctx, `
SELECT s.platform_experiment_id, s.agent_id, s.agent_type, t.current_version::text
FROM experiment_signups s JOIN agent_types t ON t.id = s.agent_type
WHERE `+signupWhere+` AND `+liveSignupSQL, args...)
	if err != nil {
		return 0, fmt.Errorf("tell the agents: %w", err)
	}
	told, err := pgx.CollectRows(agents, scanInstructionsChange)
	if err != nil {
		return 0, fmt.Errorf("tell the agents: %w", err)
	}
	coordinators, err := tx.Query(ctx, `
SELECT pe.id, '`+domain.CoordinatorRecipient+`', pe.coordinator_agent_type, t.current_version::text
FROM platform_experiments pe JOIN agent_types t ON t.id = pe.coordinator_agent_type
WHERE `+coordinatorWhere+` AND pe.status <> 'closed'`, args...)
	if err != nil {
		return 0, fmt.Errorf("tell the coordinators: %w", err)
	}
	toldCoordinators, err := pgx.CollectRows(coordinators, scanInstructionsChange)
	if err != nil {
		return 0, fmt.Errorf("tell the coordinators: %w", err)
	}
	told = append(told, toldCoordinators...)
	for _, c := range told {
		tx.Change(c)
	}
	return len(told), nil
}

func scanInstructionsChange(row pgx.CollectableRow) (domain.Change, error) {
	c := domain.Change{Kind: platformapi.EventAgentInstructions}
	err := row.Scan(&c.PlatformExperimentID, &c.AgentID, &c.Detail, &c.Value)
	c.Subject = c.AgentID
	return c, err
}

// Archive archives agent type id of org orgID: no new agent runs it, and the agents that do keep
// running. Archiving one archived changes nothing. ErrAgentTypeBuiltin for a built-in type, and an
// *AgentTypeInUse while a platform experiment that is not closed names it in its researchers, for
// its coordinator or for one of its agents.
func (s *Store) Archive(ctx context.Context, orgID, id string, by domain.Actor) (*domain.CurrentAgentType, error) {
	var archived *domain.CurrentAgentType
	if err := s.write(ctx, func(tx *Write) error {
		current, err := getAgentType(ctx, tx, orgID, id, true)
		if err != nil {
			return fmt.Errorf("agent_types_store.Archive: %w", err)
		}
		if current.Builtin {
			return ErrAgentTypeBuiltin
		}
		if current.ArchivedAt != nil {
			archived = current
			return nil
		}
		rows, err := tx.Query(ctx, `
SELECT pe.id FROM platform_experiments pe
WHERE pe.status <> 'closed' AND (
    pe.coordinator_agent_type = $1
    OR pe.researchers @> jsonb_build_array(jsonb_build_object('agent_type', $1::text))
    OR EXISTS (SELECT 1 FROM experiment_signups s WHERE s.platform_experiment_id = pe.id AND s.agent_type = $1))
ORDER BY pe.id`, id)
		if err != nil {
			return fmt.Errorf("agent_types_store.Archive: %w", err)
		}
		using, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("agent_types_store.Archive: %w", err)
		}
		if len(using) > 0 {
			return &AgentTypeInUse{PlatformExperimentIDs: using}
		}
		if err := archiveTx(ctx, tx, current, "", by); err != nil {
			return fmt.Errorf("agent_types_store.Archive: %w", err)
		}
		archived, err = getAgentType(ctx, tx, orgID, id, false)
		if err != nil {
			return fmt.Errorf("agent_types_store.Archive: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return archived, nil
}

// archiveTx archives current, whose row the transaction locked, kept as an archive with detail.
func archiveTx(ctx context.Context, tx *Write, current *domain.CurrentAgentType, detail string, by domain.Actor) error {
	if _, err := tx.Exec(ctx, `UPDATE agent_types SET archived_at = now() WHERE id = $1`, current.ID); err != nil {
		return fmt.Errorf("archive %s: %w", current.ID, err)
	}
	return recordEventTx(ctx, tx, current.ID, domain.AgentTypeArchived, current.CurrentVersion, detail, by)
}

// agentTypeTx is the agent type a new agent of role in platform experiment platformExpID runs:
// typeID, or the role's built-in one for none, when it is a built-in or one of the experiment's
// org, of that role and not archived, its row locked FOR SHARE until the write that names it
// commits, so an archive waits for it and then sees it. ErrUnknownAgentType for any other.
func agentTypeTx(ctx context.Context, q queryer, platformExpID, typeID string, role domain.AgentRole) (string, error) {
	if typeID == "" {
		typeID = domain.AgentTypeOfRole(role)
	}
	var found string
	err := q.QueryRow(ctx, `
SELECT t.id FROM agent_types t
WHERE t.id = $1 AND t.role = $2 AND t.archived_at IS NULL
  AND (t.org_id IS NULL OR t.org_id = `+orgOfSQL("$3")+`)
FOR SHARE OF t`,
		typeID, string(role), platformExpID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %q is no %s agent type of the org of %s", ErrUnknownAgentType, typeID, role, platformExpID)
	}
	return typeID, err
}

// poolTypesTx locks the agent type of each entry of pool, the researchers of platform experiment
// platformExpID, as agentTypeTx does.
func poolTypesTx(ctx context.Context, tx *Write, platformExpID string, pool domain.ResearcherPool) error {
	for _, e := range pool {
		if _, err := agentTypeTx(ctx, tx, platformExpID, e.AgentType, domain.AgentRoleResearcher); err != nil {
			return err
		}
	}
	return nil
}
