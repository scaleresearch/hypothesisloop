package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// AgentTypeCatalog is the agent types the agents of platform experiment platformExpID may run as.
func (s *Store) AgentTypeCatalog(ctx context.Context, platformExpID string) (domain.AgentTypeCatalog, error) {
	var orgID string
	err := s.pool.pool.QueryRow(ctx, `SELECT `+orgOfSQL("$1"), platformExpID).Scan(&orgID)
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
func (s *Store) ProjectAgentTypeCatalog(ctx context.Context, projectID string) (domain.AgentTypeCatalog, error) {
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
// current version of its agent type, the values of its own the platform fills in, of which an
// empty Model and nil Hyperparameters are the type's, the coordinator_model of its platform
// experiment, and the platform's current rules.
type InstructionsSource struct {
	Role             domain.AgentRole
	Version          domain.AgentTypeVersion
	Model            string
	CoordinatorModel string
	Hyperparameters  map[string]any
	RepositoryURL    string
	RulesVersion     int
	Rules            string
}

// ModelChoice is what the model of src resolves from.
func (src *InstructionsSource) ModelChoice() domain.ModelChoice {
	return domain.ModelChoice{Role: src.Role, Own: src.Model, Type: src.Version.Settings.Model, Coordinator: src.CoordinatorModel}
}

const currentRulesJoin = `
LEFT JOIN LATERAL (SELECT version, rules FROM platform_rules ORDER BY version DESC LIMIT 1) r ON true`

// AgentInstructions is what the instructions of agent agentID of platform experiment platformExpID
// are composed from, nil for an agent that is not in it.
func (s *Store) AgentInstructions(ctx context.Context, platformExpID, agentID string) (*InstructionsSource, error) {
	src, err := readInstructions(s.pool.pool.QueryRow(ctx, `
SELECT t.role, `+agentTypeVersionColumnsOnly+`, COALESCE(s.model, ''), COALESCE(pe.coordinator_model, ''),
       s.hyperparameters, pe.repository_url, r.version, r.rules
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
JOIN agent_types t ON t.id = s.agent_type
JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version`+currentRulesJoin+`
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2`, platformExpID, agentID))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.AgentInstructions: %w", err)
	}
	return src, nil
}

// CoordinatorInstructions is what the instructions of the coordinator of platform experiment
// platformExpID are composed from, nil for no such platform experiment.
func (s *Store) CoordinatorInstructions(ctx context.Context, platformExpID string) (*InstructionsSource, error) {
	src, err := readInstructions(s.pool.pool.QueryRow(ctx, `
SELECT t.role, `+agentTypeVersionColumnsOnly+`, COALESCE(pe.coordinator_model, ''), '', NULL::jsonb, pe.repository_url, r.version, r.rules
FROM platform_experiments pe
JOIN agent_types t ON t.id = pe.coordinator_agent_type
JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version`+currentRulesJoin+`
WHERE pe.id = $1`, platformExpID))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.CoordinatorInstructions: %w", err)
	}
	return src, nil
}

// ProxiedModel is what the model of agent agentID of platform experiment platformExpID, or with
// no agentID of its coordinator, resolves from, as its instructions resolve it: nil for one the
// platform does not run.
func (s *Store) ProxiedModel(ctx context.Context, platformExpID, agentID string) (*domain.ModelChoice, error) {
	var c domain.ModelChoice
	err := s.pool.pool.QueryRow(ctx, `
SELECT t.role, COALESCE(s.model, ''), COALESCE(v.settings->>'model', ''), COALESCE(pe.coordinator_model, '')
FROM experiment_signups s
JOIN platform_experiments pe ON pe.id = s.platform_experiment_id
JOIN agent_types t ON t.id = s.agent_type
JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version
WHERE $2 <> '' AND s.platform_experiment_id = $1 AND s.agent_id = $2 AND s.in_agentsandbox
UNION ALL
SELECT t.role, COALESCE(pe.coordinator_model, ''), COALESCE(v.settings->>'model', ''), ''
FROM platform_experiments pe
JOIN agent_types t ON t.id = pe.coordinator_agent_type
JOIN agent_type_versions v ON v.type_id = t.id AND v.version = t.current_version
WHERE $2 = '' AND pe.id = $1 AND pe.coordinator_in_agentsandbox`, platformExpID, agentID).Scan(&c.Role, &c.Own, &c.Type, &c.Coordinator)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ProxiedModel: %w", err)
	}
	return &c, nil
}

func readInstructions(row pgx.Row) (*InstructionsSource, error) {
	var src InstructionsSource
	var hyperparameters []byte
	var rulesVersion *int
	var rules *string
	scan := versionScanner(&src.Version)
	err := row.Scan(append(append([]any{&src.Role}, scan.dest...), &src.Model, &src.CoordinatorModel, &hyperparameters, &src.RepositoryURL, &rulesVersion, &rules)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rulesVersion == nil {
		return nil, errors.New("no platform rules are seeded")
	}
	src.RulesVersion, src.Rules = *rulesVersion, *rules
	if err := scan.done(); err != nil {
		return nil, err
	}
	if hyperparameters == nil {
		return &src, nil
	}
	return &src, json.Unmarshal(hyperparameters, &src.Hyperparameters)
}
