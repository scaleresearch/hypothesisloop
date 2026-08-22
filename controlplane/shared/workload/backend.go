package workload

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Backend is the scheduler-facing contract for desired-state placement and fresh observed
// capacity. The production implementation persists desired state in PostgreSQL and reads
// actual capacity from the metrics store; it never connects to an execution cluster.
type Backend interface {
	// SetupCluster idempotently prepares a target cluster to receive workloads (namespace,
	// priority classes, or whatever the backend's admission mechanism requires). Called once
	// per cluster at control-plane startup.
	SetupCluster(ctx context.Context) error

	// CreateWorkload places exp's job onto its target cluster. Called only after the control
	// plane's own admission decision (services/scheduler) — implementations are not expected
	// to queue or suspend; if the backend needs to do its own gating, do it here.
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error

	// WaitForJobDeletion blocks until exp's job is confirmed gone or timeout elapses. There is
	// deliberately no separate DeleteWorkload: the source of truth for "should exp's job
	// exist" is exp's own status, set by the caller before this is invoked. Callers that need
	// a job gone (preemption, eviction) must update status away from
	// SUBMITTED/ADMITTED/RUNNING first, then call this to wait for confirmation.
	WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error

	// PollJobPhase reports the current lifecycle phase of exp's job.
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (JobPhase, error)

	// GetAdmittedAcceleratorType reports which accelerator type the job actually ran on. Backends that can
	// substitute a different flavor than requested should read that back here.
	GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, error)

	// GetFlavorCapacity reports available capacity per cluster as a canonical domain.Footprint
	// (CPU millicores + per-accelerator-flavor counts, see domain.CapacityFootprint) —
	// guaranteed[cluster] and burst[cluster] — used by the scheduler's admission math
	// (domain.Fits) to place each job on a cluster with room across every requested dimension
	// jointly, rather than a pooled total that hides which cluster is overloaded.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]domain.Footprint, err error)

	// GetAcceleratorCapacityByNode reports fresh actual free devices as
	// cluster -> node -> flavor -> count for hard distributed-placement checks.
	GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error)
	// GetNodeResourceCapacity reports fresh free CPU/memory/storage per node as
	// cluster -> node -> domain.NodeResource* -> amount. A job runs on one node and must fit that
	// node in every dimension, which a cluster-wide total cannot establish.
	GetNodeResourceCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)

	// GetTotalCapacity reports each cluster's installed (not free) capacity as a canonical
	// domain.Footprint — the denominator for "how much CPU/memory does one accelerator on this
	// cluster come with", which GetFlavorCapacity's availability numbers cannot answer. A
	// cluster with no fresh total report is absent from the map rather than reported as zero,
	// so callers can tell "no accelerators installed" from "no data".
	GetTotalCapacity(ctx context.Context) (map[string]domain.Footprint, error)

	// ProvisionAgent sets up any per-agent resources the backend needs at agent registration
	// time (called once, when an agent first registers with quota-service). The native
	// backend has nothing per-agent to create, so this is a no-op; a backend modeling quota
	// as native objects would do real work here.
	ProvisionAgent(ctx context.Context, agentID string) error
}
