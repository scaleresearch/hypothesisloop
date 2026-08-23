package db

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ClusterQueueStore backs the desired-state feed and job-status reports cluster-agents
// push to. The control plane never writes an explicit "create"/"delete" instruction — the
// experiment's own status (already updated by services/scheduler before Backend is called)
// *is* the instruction, and it is the single source of truth: no parallel "desired" table.
// A cluster-agent asks "what's the desired set of Jobs for my cluster?" and reconciles its
// local state to match, the same way a kubelet reconciles pods to the spec it's given — no
// outbox, no claim/lease/ack bookkeeping needed.
//
// Callers that need a Job gone immediately (preemption, eviction) must update the
// experiment's status away from the desired set (SUBMITTED/ADMITTED/RUNNING) *before*
// waiting for deletion to be confirmed — see services/scheduler/loop.go's preempt() and
// services/controller's eviction paths, all of which do the status update first specifically
// so this table stays the only source of truth for "should a Job exist."
type ClusterQueueStore struct {
	pool *Pool
}

// NewClusterQueueStore creates a ClusterQueueStore backed by pool.
func NewClusterQueueStore(pool *Pool) *ClusterQueueStore {
	return &ClusterQueueStore{pool: pool}
}

// desiredStatuses are the experiment statuses that should have a running Job. Anything not
// in this set (QUEUED, COMPLETED, FAILED, EVICTED, REJECTED) should not.
var desiredStatuses = []string{
	string(domain.StatusSubmitted),
	string(domain.StatusAdmitted),
	string(domain.StatusRunning),
}

// ListDesiredWorkloads returns every experiment in clusterName that should currently have a
// Job running — the cluster-agent's whole "what should exist" view. Read-only; needs no
// locking, since it's not claiming anything, just reporting current truth.
func (s *ClusterQueueStore) ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE cluster_name = $1 AND status = ANY($2)
ORDER BY id`
	rows, err := s.pool.pool.Query(ctx, q, clusterName, desiredStatuses)
	if err != nil {
		return nil, fmt.Errorf("db.ListDesiredWorkloads: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}

// ListRecentlyUndesiredWorkloads returns every experiment on clusterName that has left the
// desired set within the last `within` — the rows a termination decision was just written to.
// It exists so the reconcile feed can say which of the workloads about to be deleted earned a
// checkpoint window, without any new table, column or lifecycle state to hold that fact: the
// termination is already recorded on the experiment (its status and its eviction reason), and
// `within` is what makes the answer expire on its own instead of needing to be cleared.
//
// Bounded rather than unbounded deliberately. Every experiment this cluster has ever finished is
// "not desired"; only the ones that stopped being desired moments ago are still being torn down.
func (s *ClusterQueueStore) ListRecentlyUndesiredWorkloads(ctx context.Context, clusterName string, within time.Duration) ([]*domain.Experiment, error) {
	q := `SELECT` + experimentColumns + `FROM experiments
WHERE cluster_name = $1 AND status <> ALL($2) AND updated_at > now() - $3::interval
ORDER BY id`
	rows, err := s.pool.pool.Query(ctx, q, clusterName, desiredStatuses, within.String())
	if err != nil {
		return nil, fmt.Errorf("db.ListRecentlyUndesiredWorkloads: %w", err)
	}
	defer rows.Close()
	return collectExperiments(rows)
}
