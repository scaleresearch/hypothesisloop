package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// moveToTeamRoles brings a database stored before executors and researchers were apart onto
// them: every agent type of role researcher but the built-in researcher runs jobs and is an
// executor, and a proposal's experiment that states its pool as researchers states it as agents.
// It changes nothing on a database that holds neither. A database copied onto this schema copies
// platform_experiments.researchers into agents and researchers_held into agents_held.
func moveToTeamRoles(ctx context.Context, tx pgx.Tx) error {
	for _, update := range []string{
		`UPDATE agent_types SET role = '` + string(domain.AgentRoleExecutor) + `'
WHERE role = '` + string(domain.AgentRoleResearcher) + `' AND id <> '` + domain.ResearcherAgentType + `'`,
		`UPDATE platform_experiments
SET proposal = jsonb_set(proposal #- '{experiment,researchers}', '{experiment,agents}', proposal->'experiment'->'researchers')
WHERE jsonb_typeof(proposal->'experiment') = 'object' AND proposal->'experiment' ? 'researchers'`,
	} {
		if _, err := tx.Exec(ctx, update); err != nil {
			return fmt.Errorf("move to team roles: %w", err)
		}
	}
	return nil
}
