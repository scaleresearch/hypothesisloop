package db

import (
	"context"
	"fmt"
)

// AskOrgRegistry records that orgID's repository is wanted, and says whether it is provisioned.
func (s *Store) AskOrgRegistry(ctx context.Context, orgID string) (bool, error) {
	var provisioned bool
	err := s.pool.pool.QueryRow(ctx, `
WITH asked AS (
  INSERT INTO org_registries (org_id) VALUES ($1)
  ON CONFLICT (org_id) DO NOTHING
  RETURNING false
)
SELECT provisioned_at IS NOT NULL FROM org_registries WHERE org_id = $1
UNION ALL SELECT * FROM asked`, orgID).Scan(&provisioned)
	if err != nil {
		return false, fmt.Errorf("org_registries_store.AskOrgRegistry: %w", err)
	}
	return provisioned, nil
}

// OrgRegistriesToProvision returns the orgs whose repository is wanted and not provisioned, the
// longest waiting first, and with every the provisioned ones too.
func (s *Store) OrgRegistriesToProvision(ctx context.Context, every bool) ([]string, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT org_id FROM org_registries WHERE provisioned_at IS NULL OR $1 ORDER BY requested_at`, every)
	if err != nil {
		return nil, fmt.Errorf("org_registries_store.OrgRegistriesToProvision: %w", err)
	}
	defer rows.Close()
	orgs := []string{}
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			return nil, fmt.Errorf("org_registries_store.OrgRegistriesToProvision: %w", err)
		}
		orgs = append(orgs, orgID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org_registries_store.OrgRegistriesToProvision: %w", err)
	}
	return orgs, nil
}

// RecordOrgRegistry records the provisioning of orgID's repository: done with an empty failure,
// and otherwise failed with it, to be tried again.
func (s *Store) RecordOrgRegistry(ctx context.Context, orgID, failure string) error {
	query := `UPDATE org_registries SET provisioned_at = now(), last_error = '' WHERE org_id = $1 AND $2 = ''`
	if failure != "" {
		query = `UPDATE org_registries SET last_error = $2 WHERE org_id = $1`
	}
	if _, err := s.pool.pool.Exec(ctx, query, orgID, failure); err != nil {
		return fmt.Errorf("org_registries_store.RecordOrgRegistry: %w", err)
	}
	return nil
}
