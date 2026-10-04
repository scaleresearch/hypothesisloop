package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// EnvironmentsStore keeps the environments the platform builds (domain.Environment) and decides
// which cluster builds each.
type EnvironmentsStore struct {
	pool *Pool
}

func NewEnvironmentsStore(pool *Pool) *EnvironmentsStore {
	return &EnvironmentsStore{pool: pool}
}

const environmentColumns = ` key, org_id, base, dockerfile, image, state, fault, log_tail, cluster_name, attempt, assigned_at, progress, progress_at, created_at, updated_at `

func scanEnvironment(row rowScanner) (*domain.Environment, error) {
	env := &domain.Environment{}
	var state, fault string
	if err := row.Scan(&env.Key, &env.OrgID, &env.Base, &env.Dockerfile, &env.Image, &state, &fault, &env.LogTail,
		&env.ClusterName, &env.Attempt, &env.AssignedAt, &env.Progress, &env.ProgressAt, &env.CreatedAt, &env.UpdatedAt); err != nil {
		return nil, err
	}
	env.State, env.Fault = domain.EnvironmentState(state), domain.EnvironmentFault(fault)
	return env, nil
}

// clusterBuildsFor is the rule of which cluster builds for an org, as a condition on a cluster c
// and an org named by orgID: c builds, and it is shared or the org's own and the org has not
// turned it off.
func clusterBuildsFor(orgID string) string {
	return `c.builds AND (c.shared OR c.org_id = ` + orgID + `)
	AND NOT EXISTS (SELECT 1 FROM org_disabled_clusters d WHERE d.cluster_name = c.name AND d.org_id = ` + orgID + `)`
}

// OrgHasEnvironmentBuilder says whether a registered cluster builds environments for orgID.
func (s *EnvironmentsStore) OrgHasEnvironmentBuilder(ctx context.Context, orgID string) (bool, error) {
	var has bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clusters c WHERE `+clusterBuildsFor(`$1`)+`)`, orgID).Scan(&has); err != nil {
		return false, fmt.Errorf("environments_store.OrgHasEnvironmentBuilder: %w", err)
	}
	return has, nil
}

// EnsureEnvironment returns the environment of env's key as it is stored, writing env as building
// when there is none and making one the platform failed building again from its first attempt.
// One the file failed stays failed: the key names the file, which fails the same way again.
func (s *EnvironmentsStore) EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error) {
	const q = `WITH made AS (
	INSERT INTO environments (key, org_id, base, dockerfile, image, state) VALUES ($1, $2, $3, $4, '', 'building')
	ON CONFLICT (key) DO UPDATE SET state = 'building', fault = '', log_tail = '', image = '', cluster_name = '', attempt = 0, assigned_at = NULL, updated_at = now()
	WHERE environments.state = 'failed' AND environments.fault = 'platform'
	RETURNING` + environmentColumns + `)
SELECT` + environmentColumns + `FROM made
UNION ALL
SELECT` + environmentColumns + `FROM environments WHERE key = $1 AND NOT EXISTS (SELECT 1 FROM made)`
	stored, err := scanEnvironment(s.pool.pool.QueryRow(ctx, q, env.Key, env.OrgID, env.Base, env.Dockerfile))
	if err != nil {
		return nil, fmt.Errorf("environments_store.Ensure: %w", err)
	}
	return stored, nil
}

// GetEnvironment returns the environment of key, nil when there is none.
func (s *EnvironmentsStore) GetEnvironment(ctx context.Context, key string) (*domain.Environment, error) {
	env, err := scanEnvironment(s.pool.pool.QueryRow(ctx, `SELECT`+environmentColumns+`FROM environments WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.Get: %w", err)
	}
	return env, nil
}

// AssignEnvironment decides which cluster builds the environment of key and returns it as it is
// then. A building environment is assigned when it is assigned to none, its lease has passed, or
// its cluster is not among live: to the live cluster that builds for its org with the fewest
// builds, below domain.MaxEnvironmentBuildsPerCluster, as its next attempt. One assigned
// domain.MaxEnvironmentAttempts times already fails as the platform's fault instead, and one no
// cluster takes waits assigned to none. Anything else is returned unchanged.
func (s *EnvironmentsStore) AssignEnvironment(ctx context.Context, key string, live []string, lease time.Duration) (*domain.Environment, error) {
	q := `WITH pick AS (
	SELECT c.name FROM clusters c, environments v
	WHERE v.key = $1 AND c.name = ANY(COALESCE($2::text[], '{}')) AND ` + clusterBuildsFor(`v.org_id`) + `
	  AND (SELECT count(*) FROM environments b WHERE b.state = 'building' AND b.cluster_name = c.name AND b.key <> v.key) < $5
	ORDER BY (SELECT count(*) FROM environments b WHERE b.state = 'building' AND b.cluster_name = c.name AND b.key <> v.key), c.name
	LIMIT 1
), made AS (
	UPDATE environments e SET
		state = CASE WHEN e.attempt >= $4 THEN 'failed' ELSE 'building' END,
		fault = CASE WHEN e.attempt >= $4 THEN 'platform' ELSE '' END,
		cluster_name = CASE WHEN e.attempt >= $4 THEN '' ELSE COALESCE(pick.name, '') END,
		attempt = CASE WHEN e.attempt < $4 AND pick.name IS NOT NULL THEN e.attempt + 1 ELSE e.attempt END,
		assigned_at = CASE WHEN e.attempt < $4 AND pick.name IS NOT NULL THEN now() END,
		progress = '', progress_at = NULL,
		updated_at = now()
	FROM (SELECT 1) one LEFT JOIN pick ON true
	WHERE e.key = $1 AND e.state = 'building'
	  AND (e.cluster_name = '' OR e.assigned_at <= now() - make_interval(secs => $3) OR NOT e.cluster_name = ANY(COALESCE($2::text[], '{}')))
	  AND (e.cluster_name <> '' OR e.attempt >= $4 OR pick.name IS NOT NULL)
	RETURNING` + environmentColumns + `)
SELECT` + environmentColumns + `FROM made
UNION ALL
SELECT` + environmentColumns + `FROM environments WHERE key = $1 AND NOT EXISTS (SELECT 1 FROM made)`
	env, err := scanEnvironment(s.pool.pool.QueryRow(ctx, q, key, live, lease.Seconds(), domain.MaxEnvironmentAttempts, domain.MaxEnvironmentBuildsPerCluster))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.Assign: %w", err)
	}
	return env, nil
}

// AssignedEnvironmentBuilds returns the builds assigned to cluster now, oldest first.
func (s *EnvironmentsStore) AssignedEnvironmentBuilds(ctx context.Context, cluster string) ([]*domain.Environment, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT`+environmentColumns+`FROM environments WHERE state = 'building' AND cluster_name = $1 ORDER BY created_at, key`, cluster)
	if err != nil {
		return nil, fmt.Errorf("environments_store.AssignedBuilds: %w", err)
	}
	defer rows.Close()
	var out []*domain.Environment
	for rows.Next() {
		env, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("environments_store.AssignedBuilds: %w", err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// AssignedEnvironmentBuild returns the environment of key while its build is assigned to cluster
// at attempt and is building, nil otherwise.
func (s *EnvironmentsStore) AssignedEnvironmentBuild(ctx context.Context, cluster, key string, attempt int) (*domain.Environment, error) {
	env, err := scanEnvironment(s.pool.pool.QueryRow(ctx, `SELECT`+environmentColumns+`FROM environments
WHERE key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`, key, attempt, cluster))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.AssignedBuild: %w", err)
	}
	return env, nil
}

// FinishEnvironmentBuild records what cluster reports of the build of key it was assigned at
// attempt, and says whether it was still that build's to report. Built, the environment is ready
// in outcome.Image; the file's fault fails it with the end of its output; the platform's frees it,
// so the next assignment builds it again.
func (s *EnvironmentsStore) FinishEnvironmentBuild(ctx context.Context, cluster, key string, attempt int, outcome domain.EnvironmentBuildOutcome) (bool, error) {
	var set string
	args := []any{key, attempt, cluster}
	switch outcome.Fault {
	case "":
		if outcome.Image == "" {
			return false, errors.New("environments_store.FinishBuild: a built environment names its image")
		}
		set, args = `state = 'ready', image = $4`, append(args, outcome.Image)
	case domain.EnvironmentFaultFile:
		set, args = `state = 'failed', fault = 'file', log_tail = $4`, append(args, outcome.LogTail)
	case domain.EnvironmentFaultPlatform:
		set, args = `cluster_name = '', assigned_at = NULL, log_tail = $4`, append(args, outcome.LogTail)
	default:
		return false, fmt.Errorf("environments_store.FinishBuild: %q is no fault of a build", outcome.Fault)
	}
	tag, err := s.pool.pool.Exec(ctx, `UPDATE environments SET `+set+`, updated_at = now()
WHERE key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`, args...)
	if err != nil {
		return false, fmt.Errorf("environments_store.FinishBuild: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordEnvironmentProgress keeps step as the progress of the build of key that cluster runs at
// attempt, and says whether it was still that build's to report.
func (s *EnvironmentsStore) RecordEnvironmentProgress(ctx context.Context, cluster, key string, attempt int, step string) (bool, error) {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE environments SET progress = $4, progress_at = now()
WHERE key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`, key, attempt, cluster, step)
	if err != nil {
		return false, fmt.Errorf("environments_store.RecordProgress: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
