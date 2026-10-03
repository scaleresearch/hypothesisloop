package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

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

// CompletedJobIDs returns the jobs of a platform experiment that ran to a successful end. Only
// their metrics count for an agent's best: a job that failed or was stopped proved nothing.
func (s *ExperimentsStore) CompletedJobIDs(ctx context.Context, platformExpID string) (map[string]bool, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT id FROM experiments WHERE platform_experiment_id = $1 AND status = 'COMPLETED'`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.CompletedJobIDs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("experiments_store.CompletedJobIDs: %w", err)
	}
	completed := make(map[string]bool, len(ids))
	for _, id := range ids {
		completed[id] = true
	}
	return completed, nil
}

// AgentJobs is what one agent's jobs in a platform experiment can still give a stage boundary to
// rank on.
type AgentJobs struct {
	// Completed is at least one COMPLETED job, the only kind whose metrics rank.
	Completed bool
	// InFlight is at least one admitted job that has not ended, SUBMITTED or RUNNING.
	InFlight bool
}

// AgentJobsByAgent returns, for every agent with a job in a platform experiment, whether it has a
// completed job and whether it has one in flight. One statement, so both answers describe the
// same moment.
func (s *ExperimentsStore) AgentJobsByAgent(ctx context.Context, platformExpID string) (map[string]AgentJobs, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT agent_id,
       bool_or(status = 'COMPLETED'),
       bool_or(status IN ('SUBMITTED', 'RUNNING'))
FROM experiments WHERE platform_experiment_id = $1 GROUP BY agent_id`, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.AgentJobsByAgent: %w", err)
	}
	defer rows.Close()
	agents := make(map[string]AgentJobs)
	for rows.Next() {
		var agentID string
		var jobs AgentJobs
		if err := rows.Scan(&agentID, &jobs.Completed, &jobs.InFlight); err != nil {
			return nil, fmt.Errorf("experiments_store.AgentJobsByAgent: %w", err)
		}
		agents[agentID] = jobs
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.AgentJobsByAgent: %w", err)
	}
	return agents, nil
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

// UnsummarizedCompletedPredicate is the admission summary gate written as SQL over the
// experiments table: a finished job whose write-up was never filed. Admission asks whether any
// row matches it; GET /experiments?needs_summary=true lists the rows that do. One definition, so
// the set an agent is told to clear can never disagree with the rule that blocked it.
const UnsummarizedCompletedPredicate = `status = 'COMPLETED' AND NOT EXISTS (
	SELECT 1 FROM hypothesis_findings f WHERE f.experiment_id = experiments.id)`

// UnsummarizedCompleted returns the ids of the agent's COMPLETED jobs in the platform experiment
// that have no summary, oldest first: the jobs that hold its queued ones back. FAILED and EVICTED
// jobs owe none.
func (s *ExperimentsStore) UnsummarizedCompleted(ctx context.Context, agentID, platformExpID string) ([]string, error) {
	const q = `SELECT id FROM experiments
		WHERE agent_id = $1
		  AND platform_experiment_id = $2
		  AND ` + UnsummarizedCompletedPredicate + `
		ORDER BY created_at, id`
	rows, err := s.pool.pool.Query(ctx, q, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.UnsummarizedCompleted: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DescribeJobs sets on each job what an answer says of it beyond its own row: its agent's name,
// the agent's id when it has none, and whether its summary is due. It is one read however many
// jobs are given. Every job an operation answers with passes here, so Status becomes the
// answered one (domain.AnsweredStatus) and a described job is good for answering only.
func (s *ExperimentsStore) DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error {
	if len(jobs) == 0 {
		return nil
	}
	byID := make(map[string]*domain.Experiment, len(jobs))
	ids := make([]string, len(jobs))
	for i, job := range jobs {
		byID[job.ID], ids[i] = job, job.ID
		job.AgentName = job.AgentID
		job.Status = domain.AnsweredStatus(job.Status, job.EvictionReason)
	}
	rows, err := s.pool.pool.Query(ctx, `SELECT id,
		COALESCE((SELECT NULLIF(a.name, '') FROM agents a WHERE a.id = experiments.agent_id), agent_id),
		`+UnsummarizedCompletedPredicate+`
		FROM experiments WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("experiments_store.DescribeJobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, agentName string
		var summaryDue bool
		if err := rows.Scan(&id, &agentName, &summaryDue); err != nil {
			return fmt.Errorf("experiments_store.DescribeJobs: scan: %w", err)
		}
		byID[id].AgentName, byID[id].SummaryDue = agentName, summaryDue
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("experiments_store.DescribeJobs: %w", err)
	}
	return nil
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
		COALESCE(resolved_job_spec -> 'node_selector' ->> $2, ''), submitted_at
		FROM experiments WHERE id = ANY($1)`, ids, domain.NodeSlotLabel)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.PlacementByID: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.Placement, len(ids))
	for rows.Next() {
		var id string
		var p domain.Placement
		var admittedAt *time.Time
		if err := rows.Scan(&id, &p.ClusterName, &p.AttemptCount, &p.Slot, &admittedAt); err != nil {
			return nil, fmt.Errorf("experiments_store.PlacementByID: scan: %w", err)
		}
		if admittedAt != nil {
			p.AdmittedAt = *admittedAt
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

// BaselineCodeRef returns the commit the declared baseline job of a platform experiment ran,
// empty while none is declared.
func (s *ExperimentsStore) BaselineCodeRef(ctx context.Context, platformExpID string) (string, error) {
	var codeRef string
	err := s.pool.pool.QueryRow(ctx,
		`SELECT e.code_ref FROM platform_experiments pe JOIN experiments e ON e.id = pe.baseline_job_id WHERE pe.id = $1`, platformExpID).Scan(&codeRef)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("experiments_store.BaselineCodeRef: %w", err)
	}
	return codeRef, nil
}

// LastCompletedCodeRef returns the commit the newest completed job testing a hypothesis ran,
// whichever agent's, empty when none completed.
func (s *ExperimentsStore) LastCompletedCodeRef(ctx context.Context, hypothesisID string) (string, error) {
	var codeRef string
	err := s.pool.pool.QueryRow(ctx,
		`SELECT code_ref FROM experiments WHERE hypothesis_id = $1 AND status = 'COMPLETED' ORDER BY created_at DESC, id DESC LIMIT 1`, hypothesisID).Scan(&codeRef)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("experiments_store.LastCompletedCodeRef: %w", err)
	}
	return codeRef, nil
}

// Implementations returns the branches the jobs testing a hypothesis ran on, each with its
// agent and the commit its newest job ran, oldest branch first. It is read off the jobs: a
// branch no job ran on is unknown here, and so is a job from before branches were kept.
func (s *ExperimentsStore) Implementations(ctx context.Context, hypothesisID string) ([]domain.Implementation, error) {
	const q = `
SELECT agent_id, branch, code_ref FROM (
	SELECT DISTINCT ON (agent_id, branch) agent_id, branch, code_ref, MIN(created_at) OVER (PARTITION BY agent_id, branch) AS started_at
	FROM experiments WHERE hypothesis_id = $1 AND branch <> ''
	ORDER BY agent_id, branch, created_at DESC, id DESC
) newest ORDER BY started_at, agent_id, branch`
	rows, err := s.pool.pool.Query(ctx, q, hypothesisID)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.Implementations: %w", err)
	}
	defer rows.Close()
	out := []domain.Implementation{}
	for rows.Next() {
		var i domain.Implementation
		if err := rows.Scan(&i.AgentID, &i.Branch, &i.CodeRef); err != nil {
			return nil, fmt.Errorf("experiments_store.Implementations: scan: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
