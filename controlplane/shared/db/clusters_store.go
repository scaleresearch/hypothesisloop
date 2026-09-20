package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ErrClusterNameTaken reports that another org already registered the cluster name.
var ErrClusterNameTaken = errors.New("cluster name is registered to another org")

// ClustersStore holds which cluster names exist and the org each belongs to.
type ClustersStore struct {
	pool *Pool
}

func NewClustersStore(pool *Pool) *ClustersStore {
	return &ClustersStore{pool: pool}
}

// RegisterCluster records name as a cluster of orgID. Registering a name the org already owns
// changes nothing.
func (s *ClustersStore) RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error) {
	if err := domain.ValidateIdentifier("cluster name", name); err != nil {
		return nil, err
	}
	c := &domain.Cluster{Name: name}
	err := s.pool.pool.QueryRow(ctx, `
INSERT INTO clusters (name, org_id) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING org_id, created_at`, name, orgID).Scan(&c.OrgID, &c.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.RegisterCluster: %w", err)
	}
	if c.OrgID != orgID {
		return nil, ErrClusterNameTaken
	}
	return c, nil
}

// ClusterOrg returns the org a cluster is registered to; found is false for an unregistered name.
func (s *ClustersStore) ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `SELECT org_id FROM clusters WHERE name = $1`, name).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("clusters_store.ClusterOrg: %w", err)
	}
	return orgID, true, nil
}

// ClusterOrgs maps every registered cluster name to its org.
func (s *ClustersStore) ClusterOrgs(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT name, org_id FROM clusters`)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ClusterOrgs: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, orgID string
		if err := rows.Scan(&name, &orgID); err != nil {
			return nil, fmt.Errorf("clusters_store.ClusterOrgs: scan: %w", err)
		}
		out[name] = orgID
	}
	return out, rows.Err()
}

// ListClusters returns the clusters of the given orgs by name.
func (s *ClustersStore) ListClusters(ctx context.Context, orgIDs []string) ([]*domain.Cluster, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT name, org_id, created_at FROM clusters WHERE org_id = ANY($1) ORDER BY name`, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ListClusters: %w", err)
	}
	defer rows.Close()
	out := []*domain.Cluster{}
	for rows.Next() {
		c := &domain.Cluster{}
		if err := rows.Scan(&c.Name, &c.OrgID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("clusters_store.ListClusters: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
