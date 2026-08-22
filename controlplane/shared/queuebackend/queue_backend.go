// Package queuebackend implements workload.Backend without ever calling into a target
// cluster. Every method reads or writes Postgres only (via db.ClusterQueueStore). There's no
// command outbox or "delete" call: the experiment's status (set by services/scheduler before
// Backend is called) is itself the desired state a cluster-agent reconciles against, like
// updating a Deployment's spec instead of issuing an imperative RPC. experiments.status is
// the only record of desired state. The control plane never holds cluster credentials.
package queuebackend

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Store is the persistence interface Backend needs. Satisfied by *db.Store.
type Store interface {
	SumDesiredFootprintByCluster(ctx context.Context) (map[string]domain.Footprint, error)
}

// Backend implements workload.Backend using PostgreSQL desired state and metrics observations.
type Backend struct {
	store Store
	// metricsDBURL is where cluster-agents' live per-flavor accelerator capacity lives (see
	// clusteragentapi.DesiredState / metricsdb.RecordClusterAcceleratorCapacity) — capacity is
	// metric data, read live, never duplicated into Postgres.
	metricsDBURL string
	// connectedWithin mirrors clusteragentapi.Handler's staleness window
	// (scheduler.cluster_unreachable_after_seconds): a cluster whose heartbeat is older than
	// this contributes zero capacity instead of a possibly-stale number. This also freezes new
	// admission to an Unreachable cluster, since its capacity just drops out of the sum below.
	connectedWithin time.Duration
}

// New constructs a Backend from the authoritative stores and cluster-liveness threshold.
func New(store Store, metricsDBURL string, connectedWithin time.Duration) (*Backend, error) {
	if store == nil {
		return nil, fmt.Errorf("queuebackend: store is required")
	}
	if metricsDBURL == "" {
		return nil, fmt.Errorf("queuebackend: metrics DB URL is required")
	}
	if connectedWithin <= 0 {
		return nil, fmt.Errorf("queuebackend: cluster liveness window must be positive")
	}
	return &Backend{store: store, metricsDBURL: metricsDBURL, connectedWithin: connectedWithin}, nil
}

var _ workload.Backend = (*Backend)(nil)

// SetupCluster is a no-op here: namespace and PriorityClass creation happens on the
// cluster-agent side (it has in-cluster credentials; the control plane does not).
func (b *Backend) SetupCluster(_ context.Context) error { return nil }

// CreateWorkload is a no-op: the caller already updated exp's status to SUBMITTED (or
// similar), and that status change is exactly what a cluster-agent's desired-state
// reconciliation reads. Deliberately not even a database write, to avoid a second place
// that could disagree with experiments.status.
func (b *Backend) CreateWorkload(_ context.Context, _ *domain.Experiment) error { return nil }

// WaitForJobDeletion waits until no fresh actual-state phase exists. Callers must first remove
// the experiment from desired state so the cluster agent deletes it.
func (b *Backend) WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		phase, found, err := metricsdb.LatestJobPhase(ctx, b.metricsDBURL, exp.ID, exp.ClusterName, b.connectedWithin)
		if err != nil {
			return err
		}
		if found && phase == workload.JobPhaseGone {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("queuebackend: timed out waiting for job deletion report: %s", exp.ID)
}

// PollJobPhase reads the latest fresh phase metric pushed by the cluster agent.
func (b *Backend) PollJobPhase(ctx context.Context, exp *domain.Experiment) (workload.JobPhase, error) {
	phase, found, err := metricsdb.LatestJobPhase(ctx, b.metricsDBURL, exp.ID, exp.ClusterName, b.connectedWithin)
	if err != nil {
		return workload.JobPhasePending, err
	}
	if !found {
		return workload.JobPhasePending, fmt.Errorf("queuebackend: no current job-status snapshot for experiment %s in cluster %s", exp.ID, exp.ClusterName)
	}
	return phase, nil
}

// PollPhaseDetail reads the runtime's latest reported explanation for why exp's container
// hasn't started (or has been restarting). found=false means nothing has been reported yet —
// distinct from an empty reason, which just means the runtime has nothing notable to say.
// Implements scheduler.PhaseDetailer.
func (b *Backend) PollPhaseDetail(ctx context.Context, exp *domain.Experiment) (reason, message string, restartCount int32, found bool, err error) {
	return metricsdb.GetLatestPhaseDetail(ctx, b.metricsDBURL, exp.ID)
}

// GetAdmittedAcceleratorType reads the latest observed type metric for exp's current admission.
//
// The lookback is clamped to the time since submitted_at, which Postgres rewrites on every
// ClaimSubmitted and clears on requeue. A job re-admitted onto a different flavor after
// preemption would otherwise be readable through the tail of its previous pod's samples, and
// comparing this against the newly reserved flavor would report a placement mismatch that
// happened one admission ago.
func (b *Backend) GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, error) {
	if exp.SubmittedAt == nil {
		return "", fmt.Errorf("queuebackend: experiment %s has no submission time to read its accelerator type against", exp.ID)
	}
	now := time.Now().UTC()
	window := b.connectedWithin
	if sinceSubmit := now.Sub(*exp.SubmittedAt); sinceSubmit < window {
		window = sinceSubmit
	}
	if window <= 0 {
		return "", fmt.Errorf("queuebackend: accelerator type for %s is not observed yet", exp.ID)
	}
	t, found, err := metricsdb.LatestAcceleratorType(ctx, b.metricsDBURL, exp.ID, now, window)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("queuebackend: accelerator type for %s is not observed yet", exp.ID)
	}
	return t, nil
}

// GetFlavorCapacity returns available capacity per cluster per flavor: guaranteed[cluster][flavor]
// and burst[cluster][flavor]. Every configured cluster always gets an entry.
//
// Available is the dimension-wise minimum of two views:
//   - desired-free = latest reported total minus every PostgreSQL SUBMITTED/ADMITTED/RUNNING footprint;
//   - actual-free = the cluster agent's fresh allocatable-minus-requested observation.
//
// Desired-free closes the creation-delay race right after MarkSubmitted; actual-free closes
// the deletion-delay race after preemption/eviction. Taking the minimum prevents overcommit.
// Missing/stale metrics abort the pass instead of being replaced with zero.
func (b *Backend) GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error) {
	heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: live clusters: %w", err)
	}
	cpuTotal, err := metricsdb.LiveClusterCPUTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: total CPU capacity: %w", err)
	}
	cpuAvailable, err := metricsdb.LiveClusterCPUCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: available CPU capacity: %w", err)
	}
	acceleratorAvailable, acceleratorTotal, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: accelerator capacity: %w", err)
	}
	// RAM/ephemeral-storage are hard physical-fit dimensions (not billing dimensions).
	ramTotal, err := metricsdb.LiveClusterRAMTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: total RAM capacity: %w", err)
	}
	ramAvailable, err := metricsdb.LiveClusterRAMCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: available RAM capacity: %w", err)
	}
	storageTotal, err := metricsdb.LiveClusterStorageTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: total storage capacity: %w", err)
	}
	storageAvailable, err := metricsdb.LiveClusterStorageCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: available storage capacity: %w", err)
	}
	desiredByCluster, err := b.store.SumDesiredFootprintByCluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: desired footprint: %w", err)
	}
	if err := b.validateCapacitySnapshot(heartbeats, cpuAvailable, cpuTotal, acceleratorAvailable, acceleratorTotal, ramAvailable, ramTotal, storageAvailable, storageTotal); err != nil {
		return nil, nil, err
	}

	guaranteed = make(map[string]domain.Footprint, len(heartbeats))
	burst = make(map[string]domain.Footprint, len(heartbeats))
	for cluster := range heartbeats {
		totalAccelerators := acceleratorTotal[cluster]
		availableAccelerators := acceleratorAvailable[cluster]
		total := domain.CapacityFootprint(cpuTotal[cluster], totalAccelerators, ramTotal[cluster], storageTotal[cluster])
		desiredFree := total.Sub(desiredByCluster[cluster])
		actualFree := domain.CapacityFootprint(cpuAvailable[cluster], availableAccelerators, ramAvailable[cluster], storageAvailable[cluster])
		fp := minimumFootprint(desiredFree, actualFree)
		guaranteed[cluster] = fp
		// Guaranteed and burst share the same physical pool per cluster; preemption enforces
		// the tier boundary, not a capacity split. Copy so tick()'s mutations don't alias.
		burstCopy := make(domain.Footprint, len(fp))
		for k, v := range fp {
			burstCopy[k] = v
		}
		burst[cluster] = burstCopy
	}
	return guaranteed, burst, nil
}

func (b *Backend) validateCapacitySnapshot(
	heartbeats map[string]bool,
	cpuAvailable, cpuTotal map[string]float64,
	acceleratorAvailable, acceleratorTotal map[string]map[string]int64,
	ramAvailable, ramTotal, storageAvailable, storageTotal map[string]int64,
) error {
	for cluster, connected := range heartbeats {
		if !connected {
			continue
		}
		cpuAvail, cpuAvailOK := cpuAvailable[cluster]
		cpuTot, cpuTotalOK := cpuTotal[cluster]
		ramAvail, ramAvailOK := ramAvailable[cluster]
		ramTot, ramTotalOK := ramTotal[cluster]
		storageAvail, storageAvailOK := storageAvailable[cluster]
		storageTot, storageTotalOK := storageTotal[cluster]
		accelAvail, accelAvailOK := acceleratorAvailable[cluster]
		accelTotal, accelTotalOK := acceleratorTotal[cluster]
		if !cpuAvailOK || !cpuTotalOK || !ramAvailOK || !ramTotalOK || !storageAvailOK || !storageTotalOK {
			return fmt.Errorf("queuebackend: incomplete live capacity snapshot for active cluster %q", cluster)
		}
		if accelAvailOK != accelTotalOK {
			return fmt.Errorf("queuebackend: incomplete live accelerator capacity for cluster %q", cluster)
		}
		if !accelAvailOK {
			accelAvail, accelTotal = map[string]int64{}, map[string]int64{}
		}
		if cpuAvail > cpuTot || ramAvail > ramTot || storageAvail > storageTot {
			return fmt.Errorf("queuebackend: available capacity exceeds total for active cluster %q", cluster)
		}
		if len(accelAvail) != len(accelTotal) {
			return fmt.Errorf("queuebackend: mismatched live accelerator capacity sets for cluster %q", cluster)
		}
		for flavor, total := range accelTotal {
			available, availableOK := accelAvail[flavor]
			if !availableOK {
				return fmt.Errorf("queuebackend: incomplete live accelerator capacity for cluster %q flavor %q", cluster, flavor)
			}
			if available > total {
				return fmt.Errorf("queuebackend: available accelerator capacity exceeds total for cluster %q flavor %q", cluster, flavor)
			}
		}
	}
	return nil
}

// GetAcceleratorCapacityByNode returns fresh actual per-node capacity for topology-aware
// distributed admission. Missing/stale nodes are absent and can't satisfy a hard spread requirement.
func (b *Backend) GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error) {
	return metricsdb.LiveClusterNodeAcceleratorCapacity(ctx, b.metricsDBURL, b.connectedWithin)
}

// GetTotalCapacity returns each connected cluster's installed capacity — the same `total`
// GetFlavorCapacity derives desired-free from, exposed on its own for consumers that need the
// cluster's shape rather than its free space. Only clusters with a complete fresh snapshot are
// returned; a cluster whose totals are missing or stale is omitted entirely, never zero-filled.
func (b *Backend) GetTotalCapacity(ctx context.Context) (map[string]domain.Footprint, error) {
	heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("queuebackend: live clusters: %w", err)
	}
	cpuTotal, err := metricsdb.LiveClusterCPUTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("queuebackend: total CPU capacity: %w", err)
	}
	acceleratorTotal, err := metricsdb.LiveClusterAcceleratorTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("queuebackend: total accelerator capacity: %w", err)
	}
	ramTotal, err := metricsdb.LiveClusterRAMTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("queuebackend: total RAM capacity: %w", err)
	}
	storageTotal, err := metricsdb.LiveClusterStorageTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("queuebackend: total storage capacity: %w", err)
	}
	out := make(map[string]domain.Footprint, len(heartbeats))
	for cluster, connected := range heartbeats {
		if !connected {
			continue
		}
		cpu, cpuOK := cpuTotal[cluster]
		ram, ramOK := ramTotal[cluster]
		storage, storageOK := storageTotal[cluster]
		if !cpuOK || !ramOK || !storageOK {
			continue
		}
		out[cluster] = domain.CapacityFootprint(cpu, acceleratorTotal[cluster], ram, storage)
	}
	return out, nil
}

func (b *Backend) GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error) {
	return metricsdb.LiveClusterNodeLabels(ctx, b.metricsDBURL, b.connectedWithin)
}

func minimumFootprint(a, b domain.Footprint) domain.Footprint {
	out := domain.NewFootprint()
	for resource, av := range a {
		if bv := b[resource]; bv < av {
			out[resource] = bv
		} else {
			out[resource] = av
		}
	}
	return out
}

// ProvisionAgent is a no-op: the native scheduling model has no per-agent k8s object to create.
func (b *Backend) ProvisionAgent(_ context.Context, _ string) error { return nil }
