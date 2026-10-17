package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const environmentColumns = ` key, org_id, project_id, git_repository, base, tools, dockerfile, image, loaded_image, loaded_node, loaded_cluster, state, fault, log_tail, failed_log, cluster_name, attempt, assigned_at, progress, progress_at, created_at, updated_at `

func scanEnvironment(row rowScanner) (*domain.Environment, error) {
	env := &domain.Environment{}
	var state, fault string
	if err := row.Scan(&env.Key, &env.OrgID, &env.ProjectID, &env.GitRepository, &env.Base, &env.Tools, &env.Dockerfile, &env.Image, &env.LoadedImage, &env.LoadedNode, &env.LoadedCluster, &state, &fault, &env.LogTail, &env.FailedLog,
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
func (s *Store) OrgHasEnvironmentBuilder(ctx context.Context, orgID string) (bool, error) {
	var has bool
	if err := s.pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clusters c WHERE `+clusterBuildsFor(`$1`)+`)`, orgID).Scan(&has); err != nil {
		return false, fmt.Errorf("environments_store.OrgHasEnvironmentBuilder: %w", err)
	}
	return has, nil
}

// EnsureEnvironment returns the environment of env's key as it is stored, writing env as building
// when there is none and making a failed one building again from its first attempt. One the file
// failed keeps the end of that build's output as FailedLog, which a fetch that failed by chance
// lets the agent tell from an error of the file.
func (s *Store) EnsureEnvironment(ctx context.Context, env *domain.Environment) (*domain.Environment, error) {
	const q = `WITH made AS (
	INSERT INTO environments (key, org_id, project_id, git_repository, base, tools, dockerfile, image, state) VALUES ($1, $2, $6, $7, $3, $5, $4, '', 'building')
	ON CONFLICT (key) DO UPDATE SET state = 'building', fault = '', log_tail = '', image = '', cluster_name = '', attempt = 0, assigned_at = NULL, updated_at = now(),
		loaded_image = '', loaded_node = '', loaded_cluster = '',
		failed_log = CASE WHEN environments.fault = 'file' THEN environments.log_tail ELSE environments.failed_log END
	WHERE environments.state = 'failed'
	RETURNING` + environmentColumns + `)
SELECT` + environmentColumns + `FROM made
UNION ALL
SELECT` + environmentColumns + `FROM environments WHERE key = $1 AND NOT EXISTS (SELECT 1 FROM made)`
	stored, err := scanEnvironment(s.pool.pool.QueryRow(ctx, q, env.Key, env.OrgID, env.Base, env.Dockerfile, env.Tools, env.ProjectID, env.GitRepository))
	if err != nil {
		return nil, fmt.Errorf("environments_store.Ensure: %w", err)
	}
	return stored, nil
}

// GetEnvironment returns the environment of key, nil when there is none.
func (s *Store) GetEnvironment(ctx context.Context, key string) (*domain.Environment, error) {
	env, err := scanEnvironment(s.pool.pool.QueryRow(ctx, `SELECT`+environmentColumns+`FROM environments WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.Get: %w", err)
	}
	return env, nil
}

// AwaitEnvironmentBuild has agentID of platform experiment peID told environment.status once the
// build of key is loaded on a cluster or ends, when it is building now; codeRef and image are what the agent first asked
// with while it builds, so asking again writes nothing until the build ended. The
// environment's row is held while it is read, so a build that ends at once tells the agent or was
// over before it asked.
func (s *Store) AwaitEnvironmentBuild(ctx context.Context, key, peID, agentID, codeRef, image string) error {
	if _, err := s.pool.pool.Exec(ctx, `INSERT INTO environment_waiters (environment_key, platform_experiment_id, agent_id, code_ref, image)
SELECT key, $2, $3, $4, $5 FROM environments WHERE key = $1 AND state = 'building' FOR SHARE
ON CONFLICT (environment_key, platform_experiment_id, agent_id) DO NOTHING`, key, peID, agentID, codeRef, image); err != nil {
		return fmt.Errorf("environments_store.AwaitBuild: %w", err)
	}
	return nil
}

// environmentEnded tells every agent that waits for the build of key how it ended, in state for
// fault, and lets them go. One told the build was loaded is not told it is ready.
func environmentEnded(ctx context.Context, w *Write, key string, state domain.EnvironmentState, fault domain.EnvironmentFault) error {
	rows, err := w.Query(ctx, `DELETE FROM environment_waiters WHERE environment_key = $1
RETURNING platform_experiment_id, agent_id, code_ref, image, loaded`, key)
	if err != nil {
		return fmt.Errorf("tell the end of %s: %w", key, err)
	}
	var peID, agentID, codeRef, image string
	var loaded bool
	_, err = pgx.ForEachRow(rows, []any{&peID, &agentID, &codeRef, &image, &loaded}, func() error {
		if state == domain.EnvironmentReady && loaded {
			return nil
		}
		w.Change(domain.Change{Kind: platformapi.EventEnvironmentStatus, Subject: key, Value: string(state),
			Detail: domain.EnvironmentEnded(codeRef, image, state, fault), PlatformExperimentID: peID, AgentID: agentID})
		return nil
	})
	if err != nil {
		return fmt.Errorf("tell the end of %s: %w", key, err)
	}
	return nil
}

// environmentLoaded tells every agent that waits for the build of key, and was not told so yet,
// that cluster runs it while it is pushed.
func environmentLoaded(ctx context.Context, w *Write, key, cluster string) error {
	rows, err := w.Query(ctx, `UPDATE environment_waiters SET loaded = true WHERE environment_key = $1 AND NOT loaded
RETURNING platform_experiment_id, agent_id, code_ref, image`, key)
	if err != nil {
		return fmt.Errorf("tell the load of %s: %w", key, err)
	}
	var peID, agentID, codeRef, image string
	_, err = pgx.ForEachRow(rows, []any{&peID, &agentID, &codeRef, &image}, func() error {
		w.Change(domain.Change{Kind: platformapi.EventEnvironmentStatus, Subject: key, Value: "loaded",
			Detail: domain.EnvironmentLoaded(codeRef, image, cluster), PlatformExperimentID: peID, AgentID: agentID})
		return nil
	})
	if err != nil {
		return fmt.Errorf("tell the load of %s: %w", key, err)
	}
	return nil
}

// AssignEnvironment decides which cluster builds the environment of key and returns it as it is
// then. A building environment is assigned when it is assigned to none, its lease has passed, or
// its cluster is not among holding: to the live cluster that builds for its org with the fewest
// builds, below domain.MaxEnvironmentBuildsPerCluster, as its next attempt. One assigned
// domain.MaxEnvironmentAttempts times already fails as the platform's fault instead, and one no
// cluster takes waits assigned to none. Anything else is returned unchanged. holding holds live
// and the clusters heard from within domain.EnvironmentBuildHold, which keep their builds. The
// image an earlier attempt loaded into a node is kept until the build ends: that node runs it.
func (s *Store) AssignEnvironment(ctx context.Context, key string, live, holding []string, lease time.Duration) (*domain.Environment, error) {
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
		loaded_image = CASE WHEN e.attempt >= $4 THEN '' ELSE e.loaded_image END,
		loaded_node = CASE WHEN e.attempt >= $4 THEN '' ELSE e.loaded_node END,
		loaded_cluster = CASE WHEN e.attempt >= $4 THEN '' ELSE e.loaded_cluster END,
		updated_at = now()
	FROM (SELECT 1) one LEFT JOIN pick ON true
	WHERE e.key = $1 AND e.state = 'building'
	  AND (e.cluster_name = '' OR e.assigned_at <= now() - make_interval(secs => $3) OR NOT e.cluster_name = ANY(COALESCE($6::text[], '{}')))
	  AND (e.cluster_name <> '' OR e.attempt >= $4 OR pick.name IS NOT NULL)
	RETURNING` + environmentColumns + `)
SELECT true,` + environmentColumns + `FROM made
UNION ALL
SELECT false,` + environmentColumns + `FROM environments WHERE key = $1 AND NOT EXISTS (SELECT 1 FROM made)`
	var env *domain.Environment
	err := s.write(ctx, func(w *Write) error {
		var assigned bool
		var err error
		env, err = scanEnvironment(prefixScanner{w.QueryRow(ctx, q, key, live, lease.Seconds(), domain.MaxEnvironmentAttempts, domain.MaxEnvironmentBuildsPerCluster, holding), &assigned})
		if err != nil {
			return err
		}
		if assigned && env.State == domain.EnvironmentFailed {
			return environmentEnded(ctx, w, key, env.State, env.Fault)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("environments_store.Assign: %w", err)
	}
	return env, nil
}

// AssignedEnvironmentBuilds returns the builds assigned to cluster now, oldest first.
func (s *Store) AssignedEnvironmentBuilds(ctx context.Context, cluster string) ([]*domain.Environment, error) {
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
func (s *Store) AssignedEnvironmentBuild(ctx context.Context, cluster, key string, attempt int) (*domain.Environment, error) {
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
// so the next assignment builds it again, and keeps the image it loaded into a node, which runs
// it meanwhile: a push that failed leaves that node's image whole. The file is the key's, so its fault fails the build
// whatever attempt, on whatever cluster, it is at now: an attempt taken from a cluster that went
// silent and then reported the file failing ends the build there, and is never run again.
func (s *Store) FinishEnvironmentBuild(ctx context.Context, cluster, key string, attempt int, outcome domain.EnvironmentBuildOutcome) (bool, error) {
	var set string
	const held = `, loaded_image = '', loaded_node = '', loaded_cluster = ''`
	fence := `key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`
	args := []any{key, attempt, cluster}
	switch outcome.Fault {
	case "":
		if outcome.Image == "" {
			return false, errors.New("environments_store.FinishBuild: a built environment names its image")
		}
		set, args = `state = 'ready', image = $4, failed_log = ''`+held, append(args, outcome.Image)
	case domain.EnvironmentFaultFile:
		set, fence, args = `state = 'failed', fault = 'file', log_tail = $3`+held, `key = $1 AND attempt >= $2 AND state = 'building'`, []any{key, attempt, outcome.LogTail}
	case domain.EnvironmentFaultPlatform:
		set, args = `cluster_name = '', assigned_at = NULL, log_tail = $4`, append(args, outcome.LogTail)
	default:
		return false, fmt.Errorf("environments_store.FinishBuild: %q is no fault of a build", outcome.Fault)
	}
	finished := false
	err := s.write(ctx, func(w *Write) error {
		var state, fault string
		err := w.QueryRow(ctx, `UPDATE environments SET `+set+`, updated_at = now() WHERE `+fence+` RETURNING state, fault`, args...).Scan(&state, &fault)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		finished = true
		if domain.EnvironmentState(state) == domain.EnvironmentBuilding {
			return nil
		}
		return environmentEnded(ctx, w, key, domain.EnvironmentState(state), domain.EnvironmentFault(fault))
	})
	if err != nil {
		return false, fmt.Errorf("environments_store.FinishBuild: %w", err)
	}
	return finished, nil
}

// RecordEnvironmentLoaded keeps image, repository@digest, as the image of the build of key that
// cluster runs at attempt, held on its node node while it is pushed, in place of any an earlier
// attempt loaded, and says whether it was still that build's to report.
func (s *Store) RecordEnvironmentLoaded(ctx context.Context, cluster, key string, attempt int, node, image string) (bool, error) {
	if node == "" || image == "" {
		return false, errors.New("environments_store.RecordLoaded: a loaded build names its node and its image")
	}
	recorded := false
	err := s.write(ctx, func(w *Write) error {
		tag, err := w.Exec(ctx, `UPDATE environments SET loaded_image = $5, loaded_node = $4, loaded_cluster = $3, updated_at = now()
WHERE key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`, key, attempt, cluster, node, image)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		recorded = true
		return environmentLoaded(ctx, w, key, cluster)
	})
	if err != nil {
		return false, fmt.Errorf("environments_store.RecordLoaded: %w", err)
	}
	return recorded, nil
}

// RecordEnvironmentProgress keeps step as the progress of the build of key that cluster runs at
// attempt, and says whether it was still that build's to report.
func (s *Store) RecordEnvironmentProgress(ctx context.Context, cluster, key string, attempt int, step string) (bool, error) {
	tag, err := s.pool.pool.Exec(ctx, `UPDATE environments SET progress = $4, progress_at = now()
WHERE key = $1 AND attempt = $2 AND cluster_name = $3 AND state = 'building'`, key, attempt, cluster, step)
	if err != nil {
		return false, fmt.Errorf("environments_store.RecordProgress: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// prefixScanner scans the first column of row into first and the others as its reader asks.
type prefixScanner struct {
	row   rowScanner
	first any
}

func (p prefixScanner) Scan(dest ...any) error {
	return p.row.Scan(append([]any{p.first}, dest...)...)
}
