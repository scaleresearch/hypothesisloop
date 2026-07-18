package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// UpsertPendingReservation durably records exp's admission footprint against clusterName —
// see pending_capacity_reservations' schema comment. Called once, at the moment tick() (or the
// operator admit endpoint) marks an experiment SUBMITTED, so a second tick before its pod
// actually exists sees this capacity as already claimed. Upsert (not insert) so a job that gets
// resubmitted onto a different cluster after a rollback overwrites its own prior row rather than
// conflicting with it.
func (s *ExperimentsStore) UpsertPendingReservation(ctx context.Context, experimentID, clusterName string, fp domain.Footprint) error {
	cpuKey := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	var accelFlavor string
	var accelCount int64
	for k, v := range fp {
		if k.Kind == domain.ResourceKindAccelerator && v > 0 {
			accelFlavor = k.Flavor
			accelCount = v
			break // Experiment.Footprint() carries at most one accelerator flavor per job today.
		}
	}
	const q = `
INSERT INTO pending_capacity_reservations (experiment_id, cluster_name, cpu_millicores, accelerator_flavor, accelerator_count, created_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (experiment_id) DO UPDATE SET
    cluster_name = EXCLUDED.cluster_name,
    cpu_millicores = EXCLUDED.cpu_millicores,
    accelerator_flavor = EXCLUDED.accelerator_flavor,
    accelerator_count = EXCLUDED.accelerator_count,
    created_at = NOW()`
	_, err := s.pool.pool.Exec(ctx, q, experimentID, clusterName, fp[cpuKey], accelFlavor, accelCount)
	if err != nil {
		return fmt.Errorf("experiments_store.UpsertPendingReservation: %w", err)
	}
	return nil
}

// DeletePendingReservation removes experimentID's reservation, if any — called once its fate is
// resolved: onRunning (pod confirmed to exist, so live capacity already reflects it),
// onStuckPending/onFinished (evicted/completed without ever needing to hold a pending claim), or
// a rolled-back submission (workload creation failed, job returned to QUEUED). Safe to call even
// when no row exists (no-op).
func (s *ExperimentsStore) DeletePendingReservation(ctx context.Context, experimentID string) error {
	const q = `DELETE FROM pending_capacity_reservations WHERE experiment_id = $1`
	_, err := s.pool.pool.Exec(ctx, q, experimentID)
	if err != nil {
		return fmt.Errorf("experiments_store.DeletePendingReservation: %w", err)
	}
	return nil
}

// ListPendingReservationsByCluster sums every outstanding reservation into one domain.Footprint
// per cluster_name — what tick() subtracts from live capacity on top of the existing
// SUBMITTED/ADMITTED/RUNNING accounting to close the pending-pod race (see schema comment).
func (s *ExperimentsStore) ListPendingReservationsByCluster(ctx context.Context) (map[string]domain.Footprint, error) {
	const q = `SELECT cluster_name, cpu_millicores, accelerator_flavor, accelerator_count FROM pending_capacity_reservations`
	rows, err := s.pool.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("experiments_store.ListPendingReservationsByCluster: %w", err)
	}
	defer rows.Close()

	out := make(map[string]domain.Footprint)
	cpuKey := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	for rows.Next() {
		var cluster, accelFlavor string
		var cpuMillicores, accelCount int64
		if err := rows.Scan(&cluster, &cpuMillicores, &accelFlavor, &accelCount); err != nil {
			return nil, fmt.Errorf("experiments_store.ListPendingReservationsByCluster: scan: %w", err)
		}
		fp, ok := out[cluster]
		if !ok {
			fp = domain.NewFootprint()
			out[cluster] = fp
		}
		fp.Add(cpuKey, cpuMillicores)
		if accelFlavor != "" && accelCount > 0 {
			fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: accelFlavor}, accelCount)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("experiments_store.ListPendingReservationsByCluster: %w", err)
	}
	return out, nil
}
