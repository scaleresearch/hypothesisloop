package db

import (
	"context"
	"fmt"
	"time"

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
// every experiment's Footprint() currently RUNNING or SUBMITTED there. This is the
// control plane's own desired state, updated immediately on each status transition, independent
// of how long the cluster takes to converge. Available capacity is nominal total minus this sum,
// never total minus a separately live-reported "actual usage" number.
func (s *ExperimentsStore) SumDesiredFootprintByCluster(ctx context.Context) (map[string]domain.Footprint, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('RUNNING', 'SUBMITTED') AND cluster_name != ''`

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

// ListCapacityClaimedExperiments returns every experiment in RUNNING or SUBMITTED
// status — the same three statuses SumDesiredFootprintByCluster sums, i.e. every experiment that
// has already claimed physical capacity whether or not the workload backend has confirmed it
// RUNNING yet. QUEUED is deliberately excluded: a queued job has not claimed anything. Used by the
// scheduler's tick-time node-attribution reconstruction (see loop_resolve.go's
// installedAcceleratorsByNode), which must count SUBMITTED holds too — ListRunningExperiments
// alone undercounts a node's already-claimed capacity whenever such jobs exist.
func (s *ExperimentsStore) ListCapacityClaimedExperiments(ctx context.Context) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE status IN ('RUNNING', 'SUBMITTED')
ORDER BY created_at ASC`

	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListCapacityClaimedExperiments: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListActiveByPlatformExperiment returns all non-terminal jobs (QUEUED, SUBMITTED, RUNNING) for
// a platform experiment. Used to evict jobs when an experiment is closed.
func (s *ExperimentsStore) ListActiveByPlatformExperiment(ctx context.Context, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE platform_experiment_id = $1
  AND status IN ('QUEUED', 'SUBMITTED', 'RUNNING')
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

// GetAgentQueuedExperiments returns QUEUED and SUBMITTED experiments for an agent in a platform
// experiment.
func (s *ExperimentsStore) GetAgentQueuedExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE agent_id = $1 AND platform_experiment_id = $2 AND status IN ('QUEUED', 'SUBMITTED')
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
// UnsummarizedCompletedPredicate is the admission summary gate written as SQL over the
// experiments table: a finished job whose write-up was never filed. Admission asks whether any
// row matches it; GET /experiments?needs_summary=true lists the rows that do. One definition, so
// the set an agent is told to clear can never disagree with the rule that blocked it.
const UnsummarizedCompletedPredicate = `status = 'COMPLETED' AND NOT EXISTS (
	SELECT 1 FROM hypothesis_findings f WHERE f.experiment_id = experiments.id)`

func (s *ExperimentsStore) HasUnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM experiments
		WHERE agent_id = $1
		  AND platform_experiment_id = $2
		  AND ` + UnsummarizedCompletedPredicate + `
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

// PlacementByID returns each named experiment's assigned cluster, current attempt and the slot it
// was admitted to, for the ids that exist. One query instead of a lookup per report: a cluster-agent pushes a complete snapshot
// every few seconds, and a per-report round trip made that cost scale with the number of jobs it
// runs. Both columns come from the same row read, so a report can never be judged against one
// experiment's cluster and another read's generation.
func (s *ExperimentsStore) PlacementByID(ctx context.Context, ids []string) (map[string]domain.Placement, error) {
	if len(ids) == 0 {
		return map[string]domain.Placement{}, nil
	}
	rows, err := s.pool.pool.Query(ctx, `SELECT id, cluster_name, attempt_count,
		COALESCE(resolved_job_spec -> 'node_selector' ->> $2, '')
		FROM experiments WHERE id = ANY($1)`, ids, domain.NodeSlotLabel)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.PlacementByID: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.Placement, len(ids))
	for rows.Next() {
		var id string
		var p domain.Placement
		if err := rows.Scan(&id, &p.ClusterName, &p.AttemptCount, &p.Slot); err != nil {
			return nil, fmt.Errorf("experiments_store.PlacementByID: scan: %w", err)
		}
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.PlacementByID: %w", err)
	}
	return out, nil
}

// RecentlyTriedClusters reports the set of cluster_ids ANY experiment failed a speculative
// scale-up attempt on within the last ttl — not just this job's own tried_clusters. Only those
// two reasons count, and an entry from before reasons were kept, which was one of them: a job's
// other infrastructure faults are in its tried list for its own next placement and say nothing
// about what the cluster can do for another job. autoscaler.md
// (line 109): "don't line up other jobs behind the same dead node group" needs no state of its
// own, since every failover is already recorded on the failing job's row; this reads across all
// of them rather than adding a second per-cluster ledger.
func (s *ExperimentsStore) RecentlyTriedClusters(ctx context.Context, ttl time.Duration) (map[string]bool, error) {
	if ttl <= 0 {
		return map[string]bool{}, nil
	}
	since := time.Now().UTC().Add(-ttl)
	rows, err := s.pool.pool.Query(ctx, `
		SELECT DISTINCT t.entry->>'cluster_id'
		FROM experiments, jsonb_array_elements(tried_clusters) AS t(entry)
		WHERE (t.entry->>'at')::timestamptz > $1
		  AND COALESCE(t.entry->>'reason', '') IN ('', $2, $3)`, since, string(domain.EvictionScaleUpTimeout), string(domain.EvictionFlavorMismatch))
	if err != nil {
		return nil, fmt.Errorf("experiments_store.RecentlyTriedClusters: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var clusterID string
		if err := rows.Scan(&clusterID); err != nil {
			return nil, fmt.Errorf("experiments_store.RecentlyTriedClusters: scan: %w", err)
		}
		out[clusterID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.RecentlyTriedClusters: %w", err)
	}
	return out, nil
}
