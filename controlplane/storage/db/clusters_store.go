package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
// org has not turned it off, and it runs the image the job names: a cluster with one image a job
// that names it, any other cluster a job that names one.
var clusterHostsJob = `EXISTS (
	SELECT 1 FROM clusters c, (SELECT ` + orgOfSQL("e.platform_experiment_id") + ` AS org_id) o
	WHERE c.name = $2 AND (c.shared OR c.org_id = o.org_id)
	  AND NOT EXISTS (SELECT 1 FROM org_disabled_clusters d WHERE d.cluster_name = c.name AND d.org_id = o.org_id)
	  AND CASE WHEN c.job_image = '' THEN COALESCE(e.job_spec ->> 'image', '') <> ''
	           ELSE COALESCE(e.job_spec ->> 'image', '') = c.job_image END)`

// requeueWithdrawn returns to the queue every job placed on a cluster that no longer takes it:
// all of them, or with keepOrg set every one of another org. It is the requeue an infrastructure
// fault gets (RequeueInfrastructureFault): the attempt advances so the next placement is a new
// desired state, and it is counted as not the job's own, so no retry is spent. The allowance for
// such requeues is not checked. It bounds a broken environment, and this is one decision.
var requeueWithdrawn = `
UPDATE experiments e SET
	status = 'QUEUED',
	eviction_reason = '` + string(domain.EvictionClusterWithdrawn) + `',
	attempt_count = attempt_count + 1,
	infra_requeue_count = infra_requeue_count + 1,
	infra_requeue_reason = '` + string(domain.EvictionClusterWithdrawn) + `',
	submitted_at = NULL,
	cluster_name = '',
	resolved_job_spec = NULL,
	queued_at = COALESCE(queued_at, NOW()),
	not_admitted_reason = '` + domain.QueueReasonAwaitingAdmission + `',
	quota_settled_at = NULL,
	updated_at = NOW()
WHERE e.cluster_name = $1 AND e.status IN ` + placedSQL + `
  AND ($2 = '' OR $2 <> ` + orgOfSQL("e.platform_experiment_id") + `)` + jobReturning

const clusterColumns = `name, org_id, shared, priority, COALESCE(reported_id, ''), job_image, created_at,
	scale_up_timeout_seconds, max_speculative_accelerators, stuck_pending_timeout_seconds, status_silence_ceiling_seconds,
	last_error, last_error_at,
	ARRAY(SELECT d.org_id FROM org_disabled_clusters d WHERE d.cluster_name = clusters.name ORDER BY d.org_id)`

func scanCluster(row rowScanner) (*domain.Cluster, error) {
	c := &domain.Cluster{}
	if err := row.Scan(&c.Name, &c.OrgID, &c.Shared, &c.Priority, &c.ReportedID, &c.JobImage, &c.CreatedAt,
		&c.ScaleUpTimeoutSeconds, &c.MaxSpeculativeAccelerators, &c.StuckPendingTimeoutSeconds, &c.StatusSilenceCeilingSeconds,
		&c.LastError, &c.LastErrorAt, &c.DisabledOrgs); err != nil {
		return nil, err
	}
	return c, nil
}

// RegisterCluster records name as a cluster of orgID. Registering a name the org already owns
// changes nothing.
func (s *Store) RegisterCluster(ctx context.Context, name, orgID string) (*domain.Cluster, error) {
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
func (s *Store) GetCluster(ctx context.Context, name string) (*domain.Cluster, error) {
	return s.clusterWhere(ctx, "GetCluster", `name = $1`, name)
}

// ClusterByReportedID returns the cluster bound to an identity a runtime reported, or nil when
// none is.
func (s *Store) ClusterByReportedID(ctx context.Context, reportedID string) (*domain.Cluster, error) {
	return s.clusterWhere(ctx, "ClusterByReportedID", `reported_id = $1`, reportedID)
}

func (s *Store) clusterWhere(ctx context.Context, op, where, arg string) (*domain.Cluster, error) {
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
func (s *Store) ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error) {
	err = s.pool.pool.QueryRow(ctx, `SELECT org_id FROM clusters WHERE name = $1`, name).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("clusters_store.ClusterOrg: %w", err)
	}
	return orgID, true, nil
}

// RecordJobImage keeps the one image the cluster's runtime says it runs, empty for a runtime that
// runs whatever image a job names. A report that changes nothing writes nothing.
func (s *Store) RecordJobImage(ctx context.Context, name, image string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE clusters SET job_image = $2 WHERE name = $1 AND job_image <> $2`, name, image); err != nil {
		return fmt.Errorf("clusters_store.RecordJobImage: %w", err)
	}
	return nil
}

// RecordContactRefusal keeps what the control plane refused a reconcile of the cluster's agent
// with, and when. An empty refusal is a reconcile it accepted, which clears what was kept, and
// writes nothing when nothing was.
func (s *Store) RecordContactRefusal(ctx context.Context, name, refusal string) error {
	query := `UPDATE clusters SET last_error = $2, last_error_at = now() WHERE name = $1`
	if refusal == "" {
		query = `UPDATE clusters SET last_error = '', last_error_at = NULL WHERE name = $1 AND last_error <> $2`
	}
	if _, err := s.pool.pool.Exec(ctx, query, name, refusal); err != nil {
		return fmt.Errorf("clusters_store.RecordContactRefusal: %w", err)
	}
	return nil
}

// RecordBuilds keeps whether the cluster's runtime says it builds environments. A report that
// changes nothing writes nothing.
func (s *Store) RecordBuilds(ctx context.Context, name string, builds bool) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE clusters SET builds = $2 WHERE name = $1 AND builds <> $2`, name, builds); err != nil {
		return fmt.Errorf("clusters_store.RecordBuilds: %w", err)
	}
	return nil
}

// RegisteredClusters returns every registered cluster by name.
func (s *Store) RegisteredClusters(ctx context.Context) (map[string]domain.Cluster, error) {
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
// ones it turned off are among them and say so. They come in the order placement tries them:
// highest priority first, then by name.
func (s *Store) ClustersHosting(ctx context.Context, orgID string) ([]*domain.Cluster, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT `+clusterColumns+` FROM clusters WHERE org_id = $1 OR shared ORDER BY priority DESC, name`, orgID)
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

// UpdateCluster changes a cluster in one transaction, so all of a change is written or none of
// it. shared, when stated, records whether the platform shares the cluster with every org.
// Ending the sharing is a decision about the jobs of other orgs placed there too, so it is
// written with them: they return to the queue, which is what makes the cluster's runtime stop
// them and admission place them elsewhere. requeued names those jobs. Of the settings, a value
// sets the field, 0 puts it back on the scheduler's default and nil leaves it as it is.
func (s *Store) UpdateCluster(ctx context.Context, name string, shared *bool, change domain.ClusterSettings) (cluster *domain.Cluster, requeued []string, err error) {
	if err := domain.ValidateClusterSettings(change); err != nil {
		return nil, nil, err
	}
	if err := s.write(ctx, func(tx *Write) error {
		if shared != nil {
			if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
				return fmt.Errorf("clusters_store.UpdateCluster: lock: %w", err)
			}
		}
		// A shared cluster holds the platform's priority, and one that is its org's alone again starts
		// from the priority a cluster is registered with.
		cluster, err = scanCluster(tx.QueryRow(ctx, `
UPDATE clusters SET shared = COALESCE($2::boolean, shared),
	priority = CASE WHEN $2::boolean THEN $3 WHEN NOT $2::boolean AND shared THEN $4 ELSE priority END,
	scale_up_timeout_seconds = CASE WHEN $5::int IS NULL THEN scale_up_timeout_seconds ELSE NULLIF($5::int, 0) END,
	max_speculative_accelerators = CASE WHEN $6::int IS NULL THEN max_speculative_accelerators ELSE NULLIF($6::int, 0) END,
	stuck_pending_timeout_seconds = CASE WHEN $7::int IS NULL THEN stuck_pending_timeout_seconds ELSE NULLIF($7::int, 0) END,
	status_silence_ceiling_seconds = CASE WHEN $8::int IS NULL THEN status_silence_ceiling_seconds ELSE NULLIF($8::int, 0) END
WHERE name = $1 RETURNING `+clusterColumns, name, shared, domain.SharedClusterPriority, domain.DefaultClusterPriority,
			change.ScaleUpTimeoutSeconds, change.MaxSpeculativeAccelerators, change.StuckPendingTimeoutSeconds, change.StatusSilenceCeilingSeconds))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClusterNotFound
		}
		if err != nil {
			return fmt.Errorf("clusters_store.UpdateCluster: %w", err)
		}
		if shared != nil && !*shared {
			if requeued, err = withdraw(ctx, tx, name, cluster.OrgID); err != nil {
				return fmt.Errorf("clusters_store.UpdateCluster: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return cluster, requeued, nil
}

// SetClusterEnabled records whether orgID uses a cluster for its jobs, and by whom it was turned
// off. It decides placements from here on only: the org's jobs already on the cluster stay and
// finish. It takes the admission lock, so a claim either committed before the cluster was turned
// off or reads it as off. Repeating a decision changes nothing. A cluster that is not open to the
// org is ErrClusterNotFound.
func (s *Store) SetClusterEnabled(ctx context.Context, name, orgID string, by domain.Actor, enabled bool) (*domain.Cluster, error) {
	var cluster *domain.Cluster
	if err := s.write(ctx, func(tx *Write) error {
		var err error
		if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
			return fmt.Errorf("clusters_store.SetClusterEnabled: lock: %w", err)
		}
		const open = `FROM clusters WHERE name = $1 AND (shared OR org_id = $2)`
		if enabled {
			_, err = tx.Exec(ctx, `DELETE FROM org_disabled_clusters WHERE cluster_name = $1 AND org_id = $2`, name, orgID)
		} else {
			_, err = tx.Exec(ctx, `
INSERT INTO org_disabled_clusters (cluster_name, org_id, disabled_by_kind, disabled_by_id)
SELECT name, $2, $3, $4 `+open+`
ON CONFLICT (cluster_name, org_id) DO NOTHING`, name, orgID, string(by.Kind), by.ID)
		}
		if err != nil {
			return fmt.Errorf("clusters_store.SetClusterEnabled: %w", err)
		}
		cluster, err = scanCluster(tx.QueryRow(ctx, `SELECT `+clusterColumns+` `+open, name, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClusterNotFound
		}
		if err != nil {
			return fmt.Errorf("clusters_store.SetClusterEnabled: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return cluster, nil
}

// ErrClusterOrder is an order that does not name every cluster its org ranks exactly once.
var ErrClusterOrder = errors.New("the order must name every cluster the org registered and does not share, each once")

// SetClusterOrder ranks the clusters orgID registered, first placed first, in one step: all of
// them take their new priority or none does. names must be exactly the clusters the org ranks,
// which leaves out one the platform shares. It orders placements from here on and moves no job.
// A cluster registered later starts at DefaultClusterPriority, behind an order of up to 900.
func (s *Store) SetClusterOrder(ctx context.Context, orgID string, names []string) error {
	if err := s.write(ctx, func(tx *Write) error {
		rows, err := tx.Query(ctx, `SELECT name FROM clusters WHERE org_id = $1 AND NOT shared FOR UPDATE`, orgID)
		if err != nil {
			return fmt.Errorf("clusters_store.SetClusterOrder: %w", err)
		}
		ranked, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("clusters_store.SetClusterOrder: %w", err)
		}
		if err := domain.ValidateClusterOrder(ranked, names); err != nil {
			return fmt.Errorf("%w: %w", ErrClusterOrder, err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE clusters c SET priority = $2 + 1 - o.place
FROM unnest($1::text[]) WITH ORDINALITY AS o(name, place) WHERE c.name = o.name`, names, domain.ClusterPriorityMax); err != nil {
			return fmt.Errorf("clusters_store.SetClusterOrder: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// GetClusterSettings reads the settings of the cluster bound to the id its runtime reports, which
// is how the scheduler knows a connected cluster. Nil for an id bound to no cluster and for a
// cluster with nothing set: the scheduler's defaults apply.
func (s *Store) GetClusterSettings(ctx context.Context, reportedID string) (*domain.ClusterSettings, error) {
	cs := &domain.ClusterSettings{}
	err := s.pool.pool.QueryRow(ctx, `
SELECT scale_up_timeout_seconds, max_speculative_accelerators, stuck_pending_timeout_seconds, status_silence_ceiling_seconds FROM clusters WHERE reported_id = $1`, reportedID).
		Scan(&cs.ScaleUpTimeoutSeconds, &cs.MaxSpeculativeAccelerators, &cs.StuckPendingTimeoutSeconds, &cs.StatusSilenceCeilingSeconds)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cs.Empty()) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.GetClusterSettings: %w", err)
	}
	return cs, nil
}

// StatusSilenceCeiling is the status_silence_ceiling_seconds the cluster named name sets, nil
// where it sets none or no cluster has the name. Read by name, since a cluster that stopped
// reporting has no fresh reported id to look it up by.
func (s *Store) StatusSilenceCeiling(ctx context.Context, name string) (*int, error) {
	var seconds *int
	err := s.pool.pool.QueryRow(ctx, `SELECT status_silence_ceiling_seconds FROM clusters WHERE name = $1`, name).Scan(&seconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.StatusSilenceCeiling: %w", err)
	}
	return seconds, nil
}

func withdraw(ctx context.Context, tx *Write, name, keepOrg string) ([]string, error) {
	rows, err := tx.Query(ctx, requeueWithdrawn, name, keepOrg)
	if err != nil {
		return nil, fmt.Errorf("requeue the jobs placed on %s: %w", name, err)
	}
	requeued, err := collectJobsWritten(rows)
	if err != nil {
		return nil, fmt.Errorf("requeue the jobs placed on %s: %w", name, err)
	}
	ids := make([]string, len(requeued))
	for i, j := range requeued {
		tx.jobStatus(j)
		ids[i] = j.id
	}
	return ids, nil
}

// RemoveCluster ends a cluster: every job placed on it returns to the queue, its settings and
// the identity bound to it go with its row, and the name is registered to nobody, so its runtime token is
// refused from here on. requeued names the jobs.
func (s *Store) RemoveCluster(ctx context.Context, name string) (requeued []string, err error) {
	if err := s.write(ctx, func(tx *Write) error {
		if _, err := tx.Exec(ctx, clusterAdmissionLock, name); err != nil {
			return fmt.Errorf("clusters_store.RemoveCluster: lock: %w", err)
		}
		if requeued, err = withdraw(ctx, tx, name, ""); err != nil {
			return fmt.Errorf("clusters_store.RemoveCluster: %w", err)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM clusters WHERE name = $1`, name)
		if err != nil {
			return fmt.Errorf("clusters_store.RemoveCluster: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrClusterNotFound
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return requeued, nil
}

// BindReportedID records the identity a registered cluster's runtime states about itself, once.
// The scheduler knows a connected cluster by that identity, so it is one cluster's and stays its:
// an identity another cluster holds is refused with ErrClusterIDBound, and a cluster stating
// another identity than its own with ErrClusterIDChanged until its owner clears the binding
// (ClearReportedID).
func (s *Store) BindReportedID(ctx context.Context, name, reportedID string) error {
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

// ClearReportedID forgets the identity bound to a cluster, which binds again on its runtime's
// next report. Its settings are the named cluster's and stay.
func (s *Store) ClearReportedID(ctx context.Context, name string) (*domain.Cluster, error) {
	c, err := scanCluster(s.pool.pool.QueryRow(ctx, `UPDATE clusters SET reported_id = NULL WHERE name = $1 RETURNING `+clusterColumns, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("clusters_store.ClearReportedID: %w", err)
	}
	return c, nil
}
