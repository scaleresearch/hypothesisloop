package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// maxLineageDepth bounds the ancestor walk GetLineage performs.
const maxLineageDepth = 200

// GetLineage returns the ancestor chain of an experiment (oldest first).
func (s *ExperimentsStore) GetLineage(ctx context.Context, id string) ([]*domain.Experiment, error) {
	// The depth column bounds the walk. parent_id is a plain self-reference with nothing
	// enforcing acyclicity, so an unbounded recursive CTE would spin forever on a cycle rather
	// than answering; it also caps how much this returns for a very long derivation chain.
	q := `
WITH RECURSIVE lineage AS (
	SELECT 1 AS depth,` + experimentColumns + `FROM experiments WHERE id = $1
	UNION ALL
	SELECT l.depth + 1,` + experimentColumnsQualified + `FROM experiments e
	INNER JOIN lineage l ON e.id = l.parent_id
	WHERE l.depth < $2
)
SELECT` + experimentColumns + `FROM lineage ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q, id, maxLineageDepth)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetLineage: %w", err)
	}
	defer rows.Close()

	return collectExperiments(rows)
}

// GetRunningAndQueued returns all experiments with status RUNNING, QUEUED, or SUBMITTED.
func (s *ExperimentsStore) GetRunningAndQueued(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('RUNNING', 'QUEUED', 'SUBMITTED')
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetRunningAndQueued: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// SumDesiredFootprintByCluster returns each cluster's total committed footprint — the sum of
// every experiment's Footprint() currently RUNNING, SUBMITTED, or ADMITTED there. This is the
// control plane's own desired state, updated immediately on each status transition, independent
// of how long the cluster takes to converge. Available capacity is nominal total minus this sum,
// never total minus a separately live-reported "actual usage" number.
func (s *ExperimentsStore) SumDesiredFootprintByCluster(ctx context.Context) (map[string]domain.Footprint, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('RUNNING', 'SUBMITTED', 'ADMITTED') AND cluster_name != ''`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.SumDesiredFootprintByCluster: %w", err)
	}
	defer rows.Close()
	exps, err := collectExperiments(rows)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.SumDesiredFootprintByCluster: %w", err)
	}
	out := make(map[string]domain.Footprint)
	for _, exp := range exps {
		if out[exp.ClusterName] == nil {
			out[exp.ClusterName] = domain.NewFootprint()
		}
		out[exp.ClusterName].AddFootprint(exp.Footprint())
	}
	return out, nil
}

// ListRunningExperiments returns all RUNNING experiments.
func (s *ExperimentsStore) ListRunningExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status = 'RUNNING'
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListRunningExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListExperimentsByPlatformExperiment returns all jobs for a given platform experiment.
func (s *ExperimentsStore) ListExperimentsByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE platform_experiment_id = $1
ORDER BY created_at DESC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListExperimentsByPlatformExperiment: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListActiveByPlatformExperiment returns all non-terminal jobs (QUEUED, SUBMITTED, ADMITTED,
// RUNNING) for a platform experiment. Used to evict jobs when an experiment is closed.
func (s *ExperimentsStore) ListActiveByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE platform_experiment_id = $1
  AND status IN ('QUEUED', 'SUBMITTED', 'ADMITTED', 'RUNNING')
ORDER BY created_at DESC`

	rows, err := s.pool.pool.Query(ctx, q, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListActiveByPlatformExperiment: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListSubmittedExperiments returns all experiments in SUBMITTED state.
func (s *ExperimentsStore) ListSubmittedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'SUBMITTED' ORDER BY submitted_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListSubmittedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListAdmittedExperiments returns all experiments in ADMITTED state (submitted to the workload
// backend, not yet observed RUNNING) — still holding physical capacity, same as SUBMITTED/RUNNING.
func (s *ExperimentsStore) ListAdmittedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'ADMITTED' ORDER BY submitted_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListAdmittedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListQueuedExperiments returns all QUEUED experiments ordered by queued_at ASC (age_score).
func (s *ExperimentsStore) ListQueuedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments WHERE status = 'QUEUED' ORDER BY queued_at ASC NULLS LAST, created_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListQueuedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// GetAgentRunningExperiments returns RUNNING experiments for an agent in a platform experiment.
func (s *ExperimentsStore) GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE agent_id = $1 AND platform_experiment_id = $2 AND status = 'RUNNING'
ORDER BY updated_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetAgentRunningExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// GetAgentQueuedExperiments returns QUEUED, SUBMITTED, and ADMITTED experiments for an agent in a
// platform experiment. Used by the quota-exhaustion path to cancel waiting jobs and return their
// reservations. ADMITTED is included because a workload is already being created for it — leaving
// it out lets a cut/exhausted agent's job start after the sweep believes it stopped everything.
func (s *ExperimentsStore) GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE agent_id = $1 AND platform_experiment_id = $2 AND status IN ('QUEUED', 'SUBMITTED', 'ADMITTED')
ORDER BY queued_at ASC`
	rows, err := s.pool.pool.Query(ctx, q, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.GetAgentQueuedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// HasUnsummarizedCompleted returns true when the agent has any COMPLETED experiment in the given
// platform experiment without a finding filed against its hypothesis (see hypothesis_findings,
// one row per job). Agents must document successful runs before submitting new jobs. FAILED and
// EVICTED runs are excluded since documenting infra failures adds little signal.
func (s *ExperimentsStore) HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM experiments e
		LEFT JOIN hypothesis_findings f ON f.experiment_id = e.id
		WHERE e.agent_id = $1
		  AND e.platform_experiment_id = $2
		  AND e.status = 'COMPLETED'
		  AND f.id IS NULL
	)`
	var exists bool
	err := s.pool.pool.QueryRow(ctx, q, agentID, platformExpID).Scan(&exists)
	return exists, err
}

// ListUnsettledTerminalExperiments returns terminal experiments whose final usage has not yet
// been durably written — candidates for the settlement reconciler to retry after a crash or
// metrics-DB outage.
func (s *ExperimentsStore) ListUnsettledTerminalExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('COMPLETED', 'FAILED', 'EVICTED', 'REJECTED') AND quota_settled_at IS NULL
ORDER BY updated_at ASC`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListUnsettledTerminalExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ClusterNameByID returns each named experiment's assigned cluster, for the ids that exist. One
// query instead of a lookup per report: a cluster-agent pushes a complete snapshot every few
// seconds, and a per-report round trip made that cost scale with the number of jobs it runs.
func (s *ExperimentsStore) ClusterNameByID(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.pool.pool.Query(ctx, `SELECT id, cluster_name FROM experiments WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ClusterNameByID: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, cluster string
		if err := rows.Scan(&id, &cluster); err != nil {
			return nil, fmt.Errorf("experiments_store.ClusterNameByID: scan: %w", err)
		}
		out[id] = cluster
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.ClusterNameByID: %w", err)
	}
	return out, nil
}
