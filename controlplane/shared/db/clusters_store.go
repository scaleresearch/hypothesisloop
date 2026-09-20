package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	// ErrClusterNameTaken reports that another org already registered the cluster name.
	ErrClusterNameTaken = errors.New("cluster name is registered to another org")
	ErrClusterNotFound  = errors.New("cluster is not registered")
	// ErrClusterIDBound reports a runtime stating an identity another registered cluster holds.
	ErrClusterIDBound = errors.New("cluster id is bound to another registered cluster")
)

// ClustersStore holds which cluster names exist, the org each belongs to, whether the platform
// shares it, and the identity its runtime reported.
type ClustersStore struct {
	pool *Pool
}

func NewClustersStore(pool *Pool) *ClustersStore {
	return &ClustersStore{pool: pool}
}

const clusterColumns = `name, org_id, shared, COALESCE(reported_id, ''), created_at`

func scanCluster(row rowScanner) (*domain.Cluster, error) {
	c := &domain.Cluster{}
	if err := row.Scan(&c.Name, &c.OrgID, &c.Shared, &c.ReportedID, &c.CreatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// RegisterCluster records name as a cluster of orgID. Registering a name the org already owns
// changes nothing.
func (s *ClustersStore) RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error) {
	if err := domain.ValidateIdentifier("cluster name", name); err != nil {
		return nil, err
	}
	c, err := scanCluster(s.pool.pool.QueryRow(ctx, `
INSERT INTO clusters (name, org_id) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING `+clusterColumns, name, orgID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return nil, ErrOrgNotFound
		}
		return nil, fmt.Errorf("clusters_store.RegisterCluster: %w", err)
	}
	if c.OrgID != orgID {
		return nil, ErrClusterNameTaken
	}
	return c, nil
}

// GetCluster returns a registered cluster, or nil when the name is not registered.
func (s *ClustersStore) GetCluster(ctx context.Context, name string) (*domain.Cluster, error) {
	return s.clusterWhere(ctx, "GetCluster", `name = $1`, name)
}

// ClusterByReportedID returns the cluster bound to an identity a runtime reported, or nil when
// none is.
func (s *ClustersStore) ClusterByReportedID(ctx context.Context, reportedID string) (*domain.Cluster, error) {
	return s.clusterWhere(ctx, "ClusterByReportedID", `reported_id = $1`, reportedID)
}

func (s *ClustersStore) clusterWhere(ctx context.Context, op, where, arg string) (*domain.Cluster, error) {
	c, err := scanCluster(s.pool.pool.QueryRow(ctx, `SELECT `+clusterColumns+` FROM clusters WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.%s: %w", op, err)
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

// RegisteredClusters returns every registered cluster by name.
func (s *ClustersStore) RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT `+clusterColumns+` FROM clusters`)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.RegisteredClusters: %w", err)
	}
	defer rows.Close()
	out := map[string]domain.Cluster{}
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, fmt.Errorf("clusters_store.RegisteredClusters: scan: %w", err)
		}
		out[c.Name] = *c
	}
	return out, rows.Err()
}

// ListClusters returns the clusters of the given orgs by name.
func (s *ClustersStore) ClustersHosting(ctx context.Context, orgID string) ([]*domain.Cluster, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT `+clusterColumns+` FROM clusters WHERE org_id = $1 OR shared ORDER BY name`, orgID)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ClustersHosting: %w", err)
	}
	defer rows.Close()
	out := []*domain.Cluster{}
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, fmt.Errorf("clusters_store.ClustersHosting: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetClusterShared records whether the platform shares a cluster with every org.
func (s *ClustersStore) SetClusterShared(ctx context.Context, name string, shared bool) (*domain.Cluster, error) {
	c, err := scanCluster(s.pool.pool.QueryRow(ctx, `UPDATE clusters SET shared = $2 WHERE name = $1 RETURNING `+clusterColumns, name, shared))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterShared: %w", err)
	}
	return c, nil
}

// BindReportedID records the identity a registered cluster's runtime states about itself. A
// cluster rebuilt under its name reports a new identity and is bound to that one. An identity
// another registered cluster holds is refused with ErrClusterIDBound.
func (s *ClustersStore) BindReportedID(ctx context.Context, name, reportedID string) error {
	var bound string
	err := s.pool.pool.QueryRow(ctx, `SELECT COALESCE(reported_id, '') FROM clusters WHERE name = $1`, name).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrClusterNotFound
	}
	if err != nil {
		return fmt.Errorf("clusters_store.BindReportedID: %w", err)
	}
	if bound == reportedID {
		return nil
	}
	if _, err := s.pool.pool.Exec(ctx, `UPDATE clusters SET reported_id = $2 WHERE name = $1`, name, reportedID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ErrClusterIDBound
		}
		return fmt.Errorf("clusters_store.BindReportedID: %w", err)
	}
	return nil
}
