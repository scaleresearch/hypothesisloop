package workload

import (
	"context"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Backend is the full contract the control plane needs from whatever actually places and
// tracks training jobs on a target execution cluster. *ClusterSet (plain k8s Jobs + native
// PriorityClass, no external operator) is the only implementation today, but this interface
// is the seam a team would implement against to plug in a different scheduling mechanism
// instead — Kueue, Volcano, Slurm-on-k8s, or anything else — without changing any code in
// services/scheduler, services/controller, or services/quota. Those packages only ever
// depend on their own narrower local interfaces (e.g. scheduler.LoopWorkloadClient,
// controller.WorkloadDeleter); Backend exists so a single type can be constructed once in
// cmd/*/main.go and satisfy all of them at once.
//
// In production, cmd/control-service uses queuebackend.Backend (pure Postgres, never dials a
// cluster) and each cluster's own cluster-agent uses *workload.ClusterSet (the real k8s
// client) — see shared/queuebackend and cluster/cmd/cluster-agent. To plug in an alternative
// execution engine: implement Backend in a new package (e.g. controlplane/shared/slurmbackend)
// and swap the constructor call at whichever of those wiring points owns real cluster access.
// No other source changes needed — admission policy, quota, and preemption all live in
// services/scheduler and are backend-agnostic already; only physical job placement and status
// crosses this boundary.
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
	// deliberately no separate DeleteWorkload: the single source of truth for "should exp's
	// job exist" is exp's own status (set by the caller before this is invoked) — the backend
	// derives desired state from that, not from a separate imperative delete call. Callers
	// that need a job gone (preemption, eviction) must update status away from
	// SUBMITTED/ADMITTED/RUNNING first, then call this to wait for confirmation.
	WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error

	// PollJobPhase reports the current lifecycle phase of exp's job.
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (JobPhase, error)

	// GetAdmittedGPUType reports which GPU type the job actually ran on. Backends that can
	// substitute a different flavor than requested (e.g. Kueue's ResourceFlavor ordering)
	// should read that back here; backends that never substitute (like ClusterSet today) can
	// just return exp.GPUType.
	GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType

	// GetFlavorCapacity reports available GPU slots per GPU flavor for the guaranteed
	// and burst tiers, used by the scheduler loop's admission math. GPUs are the only
	// resource dimension modeled — CPU/memory/storage are not accounted for.
	GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]int64, err error)

	// ClusterNames returns the configured target cluster names in stable order, used by
	// admission to pick a cluster for a newly-admitted experiment.
	ClusterNames() []string

	// ProvisionAgent sets up any per-agent resources the backend needs at agent registration
	// time (called once, when an agent first registers with quota-service). The native
	// backend has nothing per-agent to create — shared queues and priority classes are
	// enough — so this is a no-op here. A backend that models quota as native objects would
	// do real work.
	ProvisionAgent(ctx context.Context, agentID string) error
}

// Compile-time check that ClusterSet satisfies Backend.
var _ Backend = (*ClusterSet)(nil)
