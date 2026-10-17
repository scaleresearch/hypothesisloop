package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// tenantClause restricts rows to the platform experiments inside scope, matched on peColumn, and
// not removed. It returns "" for an unrestricted scope, and otherwise a predicate numbered after
// args.
func tenantClause(scope domain.TenantScope, peColumn string, args []any) (string, []any) {
	if scope.Unrestricted() {
		return "", args
	}
	conds, args := tenantConds(scope, args)
	return peColumn + ` IN (SELECT tpe.id FROM platform_experiments tpe JOIN projects tp ON tp.id = tpe.project_id WHERE tpe.removed_at IS NULL AND ` +
		strings.Join(conds, " AND ") + `)`, args
}

// tenantConds are the conditions on a platform experiment tpe, joined to its project as tp, that
// place it inside scope, a restricted one, numbered after args.
func tenantConds(scope domain.TenantScope, args []any) ([]string, []any) {
	var conds []string
	if !scope.AllOrgs {
		args = append(args, scope.OrgIDs)
		conds = append(conds, fmt.Sprintf("tp.org_id = ANY($%d)", len(args)))
	}
	if scope.ProjectID != "" {
		args = append(args, scope.ProjectID)
		conds = append(conds, fmt.Sprintf("tpe.project_id = $%d", len(args)))
	}
	if scope.PlatformExperimentID != "" {
		args = append(args, scope.PlatformExperimentID)
		conds = append(conds, fmt.Sprintf("tpe.id = $%d", len(args)))
	}
	return conds, args
}

// PlatformExperimentTenancy places a platform experiment and reports when it closed, nil while
// it has not. A nil tenancy means there is no such platform experiment, or it was removed.
func (s *Store) PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (*domain.Tenancy, *time.Time, error) {
	t := &domain.Tenancy{PlatformExperimentID: platformExperimentID}
	var closedAt *time.Time
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.project_id, p.org_id, pe.shared, ARRAY(`+priorRuns+`), pe.closed_at
FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
WHERE pe.id = $1 AND pe.removed_at IS NULL`, platformExperimentID).Scan(&t.ProjectID, &t.OrgID, &t.Shared, &t.PriorRuns, &closedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("accounts_store.PlatformExperimentTenancy: %w", err)
	}
	return t, closedAt, nil
}

// ExperimentTenancy places a job through its platform experiment, names the agent it belongs to
// and reports whether the job has ended. nil when there is no such job, or its platform
// experiment was removed.
func (s *Store) ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error) {
	t := &domain.Tenancy{}
	var terminal bool
	err := s.pool.pool.QueryRow(ctx, `
SELECT pe.id, pe.project_id, p.org_id, pe.shared, e.agent_id, e.status IN `+endedSQL+`
FROM experiments e
JOIN platform_experiments pe ON pe.id = e.platform_experiment_id
JOIN projects p ON p.id = pe.project_id
WHERE e.id = $1 AND pe.removed_at IS NULL`, experimentID).Scan(&t.PlatformExperimentID, &t.ProjectID, &t.OrgID, &t.Shared, &t.AgentID, &terminal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("accounts_store.ExperimentTenancy: %w", err)
	}
	return t, terminal, nil
}

// AgentOrg returns the org an agent was registered in; found is false for an unknown agent.
func (s *Store) AgentOrg(ctx context.Context, agentID string) (orgID string, found bool, err error) {
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
func (s *Store) PlatformExperimentIDs(ctx context.Context, scope domain.TenantScope) ([]string, error) {
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
