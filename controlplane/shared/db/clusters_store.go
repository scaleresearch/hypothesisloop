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
	// ErrClusterIDChanged reports a runtime stating an identity other than the one its cluster
	// is bound to.
	ErrClusterIDChanged = errors.New("cluster is bound to another cluster id")
)

// clusterAdmissionLock is taken by everything that places a job on a cluster or withdraws the
// cluster from jobs, so the two never interleave.
const clusterAdmissionLock = `SELECT pg_advisory_xact_lock(hashtextextended('cluster-admission/' || $1, 0))`

// clusterHostsJob is the placement rule (domain.Cluster.HostsJob) as a condition on a job row e
// and a cluster named $2: the cluster is registered, is shared or belongs to the job's org, that
// org has not turned it off, and it runs the image the job names: a cluster with a fixed image
// the jobs that name none or exactly it, every other cluster the jobs that name one.
const clusterHostsJob = `EXISTS (
	SELECT 1 FROM clusters c, platform_experiments pe JOIN projects p ON p.id = pe.project_id
	WHERE c.name = $2 AND pe.id = e.platform_experiment_id AND (c.shared OR c.org_id = p.org_id)
	  AND NOT EXISTS (SELECT 1 FROM org_disabled_clusters d WHERE d.cluster_name = c.name AND d.org_id = p.org_id)
	  AND CASE WHEN c.job_image = '' THEN COALESCE(e.job_spec ->> 'image', '') <> ''
	           ELSE COALESCE(e.job_spec ->> 'image', '') IN ('', c.job_image) END)`

// requeueWithdrawn returns to the queue every job placed on a cluster that no longer takes it:
// all of them, or with keepOrg set every one of another org. It is the requeue an infrastructure
// fault gets (RequeueInfrastructureFault): the attempt advances so the next placement is a new
// desired state, and it is counted as not the job's own, so no retry is spent. The allowance for
// such requeues is not checked. It bounds a broken environment, and this is one decision.
const requeueWithdrawn = `
UPDATE experiments e SET
	status = 'QUEUED',
	eviction_reason = '` + string(domain.EvictionClusterWithdrawn) + `',
	attempt_count = attempt_count + 1,
	infra_requeue_count = infra_requeue_count + 1,
	submitted_at = NULL,
	cluster_name = '',
	resolved_job_spec = NULL,
	queued_at = COALESCE(queued_at, NOW()),
	not_admitted_reason = 'awaiting_admission',
	quota_settled_at = NULL,
	updated_at = NOW()
WHERE e.cluster_name = $1 AND e.status IN ('SUBMITTED', 'RUNNING')
  AND ($2 = '' OR $2 <> (
	SELECT p.org_id FROM platform_experiments pe JOIN projects p ON p.id = pe.project_id
	WHERE pe.id = e.platform_experiment_id))
RETURNING e.id`

// ClustersStore holds which cluster names exist, the org each belongs to, whether the platform
// shares it, the identity its runtime reported, and which orgs turned it off for their jobs.
type ClustersStore struct {
	pool *Pool
}

func NewClustersStore(pool *Pool) *ClustersStore {
	return &ClustersStore{pool: pool}
}

const clusterColumns = `name, org_id, shared, COALESCE(reported_id, ''), job_image, created_at,
	ARRAY(SELECT d.org_id FROM org_disabled_clusters d WHERE d.cluster_name = clusters.name ORDER BY d.org_id)`

func scanCluster(row rowScanner) (*domain.Cluster, error) {
	c := &domain.Cluster{}
	if err := row.Scan(&c.Name, &c.OrgID, &c.Shared, &c.ReportedID, &c.JobImage, &c.CreatedAt, &c.DisabledOrgs); err != nil {
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

// RecordJobImage keeps the one image the cluster's runtime says every job there runs in, empty
// for a runtime that runs each job in its own. A report that changes nothing writes nothing.
func (s *ClustersStore) RecordJobImage(ctx context.Context, name, image string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE clusters SET job_image = $2 WHERE name = $1 AND job_image <> $2`, name, image); err != nil {
		return fmt.Errorf("clusters_store.RecordJobImage: %w", err)
	}
	return nil
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

// ClustersHosting returns the clusters orgID can use, its own and the shared ones, by name. The
// ones it turned off are among them and say so.
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

// SetClusterShared records whether the platform shares a cluster with every org. Ending the
// sharing is a decision about the jobs of other orgs placed there too, so it is written with
// them: in the same transaction they return to the queue, which is what makes the cluster's
// runtime stop them and admission place them elsewhere. requeued names those jobs.
func (s *ClustersStore) SetClusterShared(ctx context.Context, name string, shared bool) (cluster *domain.Cluster, requeued []string, err error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("clusters_store.SetClusterShared: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
		return nil, nil, fmt.Errorf("clusters_store.SetClusterShared: lock: %w", err)
	}
	cluster, err = scanCluster(tx.QueryRow(ctx, `UPDATE clusters SET shared = $2 WHERE name = $1 RETURNING `+clusterColumns, name, shared))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("clusters_store.SetClusterShared: %w", err)
	}
	if !shared {
		if requeued, err = withdraw(ctx, tx, name, cluster.OrgID); err != nil {
			return nil, nil, fmt.Errorf("clusters_store.SetClusterShared: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("clusters_store.SetClusterShared: commit: %w", err)
	}
	return cluster, requeued, nil
}

// SetClusterEnabled records whether orgID uses a cluster for its jobs, and by whom it was turned
// off. It decides placements from here on only: the org's jobs already on the cluster stay and
// finish. It takes the admission lock, so a claim either committed before the cluster was turned
// off or reads it as off. Repeating a decision changes nothing. A cluster that is not open to the
// org is ErrClusterNotFound.
func (s *ClustersStore) SetClusterEnabled(ctx context.Context, name, orgID, by string, enabled bool) (*domain.Cluster, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterEnabled: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterEnabled: lock: %w", err)
	}
	const open = `FROM clusters WHERE name = $1 AND (shared OR org_id = $2)`
	if enabled {
		_, err = tx.Exec(ctx, `DELETE FROM org_disabled_clusters WHERE cluster_name = $1 AND org_id = $2`, name, orgID)
	} else {
		_, err = tx.Exec(ctx, `
INSERT INTO org_disabled_clusters (cluster_name, org_id, disabled_by)
SELECT name, $2, $3 `+open+`
ON CONFLICT (cluster_name, org_id) DO NOTHING`, name, orgID, by)
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterEnabled: %w", err)
	}
	cluster, err := scanCluster(tx.QueryRow(ctx, `SELECT `+clusterColumns+` `+open, name, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterEnabled: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("clusters_store.SetClusterEnabled: commit: %w", err)
	}
	return cluster, nil
}

func withdraw(ctx context.Context, tx pgx.Tx, name, keepOrg string) ([]string, error) {
	rows, err := tx.Query(ctx, requeueWithdrawn, name, keepOrg)
	if err != nil {
		return nil, fmt.Errorf("requeue the jobs placed on %s: %w", name, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("requeue the jobs placed on %s: %w", name, err)
	}
	return ids, nil
}

// RemoveCluster ends a cluster: every job placed on it returns to the queue, its settings and
// the identity bound to it go, and the name is registered to nobody, so its runtime token is
// refused from here on. requeued names the jobs.
func (s *ClustersStore) RemoveCluster(ctx context.Context, name string) (requeued []string, err error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.RemoveCluster: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
		return nil, fmt.Errorf("clusters_store.RemoveCluster: lock: %w", err)
	}
	if requeued, err = withdraw(ctx, tx, name, ""); err != nil {
		return nil, fmt.Errorf("clusters_store.RemoveCluster: %w", err)
	}
	tag, err := tx.Exec(ctx, `
WITH removed AS (DELETE FROM clusters WHERE name = $1 RETURNING reported_id),
     settings AS (DELETE FROM cluster_settings WHERE cluster_id IN (SELECT reported_id FROM removed))
SELECT 1 FROM removed`, name)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.RemoveCluster: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrClusterNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("clusters_store.RemoveCluster: commit: %w", err)
	}
	return requeued, nil
}

// BindReportedID records the identity a registered cluster's runtime states about itself, once.
// The settings of a cluster are kept under that identity, so it is one cluster's and stays its:
// an identity another cluster holds is refused with ErrClusterIDBound, and a cluster stating
// another identity than its own with ErrClusterIDChanged until its owner clears the binding
// (ClearReportedID).
func (s *ClustersStore) BindReportedID(ctx context.Context, name, reportedID string) error {
	if err := domain.ValidateClusterID(reportedID); err != nil {
		return err
	}
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
	if bound != "" {
		return ErrClusterIDChanged
	}
	tag, err := s.pool.pool.Exec(ctx, `UPDATE clusters SET reported_id = $2 WHERE name = $1 AND reported_id IS NULL`, name, reportedID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ErrClusterIDBound
		}
		return fmt.Errorf("clusters_store.BindReportedID: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClusterIDChanged
	}
	return nil
}

// ClearReportedID forgets the identity bound to a cluster and deletes the settings kept under
// it, so nobody who later states that identity inherits them. The cluster binds again on its
// runtime's next report.
func (s *ClustersStore) ClearReportedID(ctx context.Context, name string) (*domain.Cluster, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ClearReportedID: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
DELETE FROM cluster_settings WHERE cluster_id = (SELECT reported_id FROM clusters WHERE name = $1 FOR UPDATE)`, name); err != nil {
		return nil, fmt.Errorf("clusters_store.ClearReportedID: settings: %w", err)
	}
	c, err := scanCluster(tx.QueryRow(ctx, `UPDATE clusters SET reported_id = NULL WHERE name = $1 RETURNING `+clusterColumns, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ClearReportedID: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("clusters_store.ClearReportedID: commit: %w", err)
	}
	return c, nil
}
