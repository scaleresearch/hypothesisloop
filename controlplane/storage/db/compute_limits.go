package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// heldOnSharedSQL sums acch_per_hour over the placed jobs of org $1 on shared clusters, all of
// them and those of project $2, leaving out job $3.
var heldOnSharedSQL = `SELECT COALESCE(SUM(e.acch_per_hour), 0), COALESCE(SUM(e.acch_per_hour) FILTER (WHERE p.id = $2), 0)
FROM experiments e
JOIN clusters c ON c.name = e.cluster_name AND c.shared
JOIN platform_experiments pe ON pe.id = e.platform_experiment_id
JOIN projects p ON p.id = pe.project_id
WHERE p.org_id = $1 AND e.status IN ` + placedSQL + ` AND e.id <> $3`

// orgSharedLock is the advisory lock a claim on a shared cluster takes after the cluster's own,
// so two claims of one org on different shared clusters read each other's reservations.
func orgSharedLock(ctx context.Context, q queryer, orgID string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org-shared/"+orgID)
	return err
}

// SeedDefaultOrg creates the default org, with limit, and its default project where they do not
// exist. An existing default org keeps the limit it has.
func (s *Store) SeedDefaultOrg(ctx context.Context, limit domain.ComputeLimit) error {
	if err := limit.Validate(); err != nil {
		return fmt.Errorf("accounts_store.SeedDefaultOrg: %w", err)
	}
	return s.write(ctx, func(tx *Write) error {
		if _, err := tx.Exec(ctx, `INSERT INTO orgs (id, name, shared_acch_per_hour, project_shared_acch_per_hour) VALUES ('default', 'default', $1, $2)
ON CONFLICT (id) DO NOTHING`, limit.Org, limit.Project); err != nil {
			return fmt.Errorf("accounts_store.SeedDefaultOrg: org: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO projects (id, org_id, name) VALUES ('default', 'default', 'default') ON CONFLICT (id) DO NOTHING`); err != nil {
			return fmt.Errorf("accounts_store.SeedDefaultOrg: project: %w", err)
		}
		return nil
	})
}

// SetOrgComputeLimit replaces an org's platform compute limit. ErrOrgNotFound when there is no
// such org. Jobs placed already stay placed.
func (s *Store) SetOrgComputeLimit(ctx context.Context, orgID string, limit domain.ComputeLimit) error {
	if err := limit.Validate(); err != nil {
		return err
	}
	tag, err := s.pool.pool.Exec(ctx, `UPDATE orgs SET shared_acch_per_hour = $2, project_shared_acch_per_hour = $3 WHERE id = $1`, orgID, limit.Org, limit.Project)
	if err != nil {
		return fmt.Errorf("accounts_store.SetOrgComputeLimit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOrgNotFound
	}
	return nil
}

// OrgComputeLimits is every org's platform compute limit, by org.
func (s *Store) OrgComputeLimits(ctx context.Context) (map[string]domain.ComputeLimit, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT id, shared_acch_per_hour, project_shared_acch_per_hour FROM orgs`)
	if err != nil {
		return nil, fmt.Errorf("accounts_store.OrgComputeLimits: %w", err)
	}
	defer rows.Close()
	limits := map[string]domain.ComputeLimit{}
	for rows.Next() {
		var id string
		var limit domain.ComputeLimit
		if err := rows.Scan(&id, &limit.Org, &limit.Project); err != nil {
			return nil, fmt.Errorf("accounts_store.OrgComputeLimits: %w", err)
		}
		limits[id] = limit
	}
	return limits, rows.Err()
}

// PlatformComputeHeld is orgID's platform compute limit and what its placed jobs on shared
// clusters reserve, all of them and those of projectID, which may be empty. ErrOrgNotFound when
// there is no such org.
func (s *Store) PlatformComputeHeld(ctx context.Context, orgID, projectID string) (limit domain.ComputeLimit, orgHeld, projectHeld float64, err error) {
	err = s.pool.pool.QueryRow(ctx, `SELECT shared_acch_per_hour, project_shared_acch_per_hour FROM orgs WHERE id = $1`, orgID).Scan(&limit.Org, &limit.Project)
	if errors.Is(err, pgx.ErrNoRows) {
		return limit, 0, 0, ErrOrgNotFound
	}
	if err != nil {
		return limit, 0, 0, fmt.Errorf("accounts_store.PlatformComputeHeld: %w", err)
	}
	if err := s.pool.pool.QueryRow(ctx, heldOnSharedSQL, orgID, projectID, "").Scan(&orgHeld, &projectHeld); err != nil {
		return limit, 0, 0, fmt.Errorf("accounts_store.PlatformComputeHeld: held: %w", err)
	}
	return limit, orgHeld, projectHeld, nil
}
