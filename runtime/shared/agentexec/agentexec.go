// Package agentexec names the method set every cluster-side execution client implements —
// k8sexec.JobWorkloadClient (Kubernetes) and podexec.Executor (bare node, no k8s) — so the
// reconcile/status loop shape (fetch desired state, diff against actual, converge, report
// phase) is written once and shared: see runtime/shared/agentloop, which both
// runtime/k8s/cmd/cluster-agent and runtime/bare-metal/cmd/bare-agent drive their loop through.
package agentexec

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Executor is the cluster-local half of the desired-state reconcile loop: given exp's compiled
// desired state, create/delete/compare it against whatever is actually running, and report
// phase back. This is deliberately not controlplane/shared/workload.Backend — Backend is the
// control-plane-facing, multi-cluster contract (queuebackend); Executor is the single-cluster,
// agent-facing contract each cluster-agent/bare-agent binary drives its local reconcile/status
// loop with.
type Executor interface {
	SetupCluster(ctx context.Context) error

	CreateWorkload(ctx context.Context, exp *domain.Experiment) error
	DeleteWorkload(ctx context.Context, experimentID string) error
	WaitForJobDeletion(ctx context.Context, experimentID string, timeout time.Duration) error

	ListManagedJobs(ctx context.Context) ([]string, error)
	ListManagedAuxiliaryWorkloads(ctx context.Context) ([]string, error)
	WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error)

	PollJobPhase(ctx context.Context, experimentID string) (workload.JobPhase, error)
	PollJobPhaseAndUID(ctx context.Context, experimentID string) (workload.JobPhase, string, error)
	ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (acceleratorType domain.AcceleratorType, node string, consistent bool, err error)

	GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error)
	GetLiveRAMCapacity(ctx context.Context) (available, total int64, err error)
	GetLiveStorageCapacity(ctx context.Context) (available, total int64, err error)
	// GetLiveNodeResourceCapacity reports free CPU/memory/storage per node, keyed by
	// domain.NodeResource*. The cluster-wide totals above cannot answer "does this job fit a
	// node", and a job runs on one node — admission needs both views.
	GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error)
	GetLiveAcceleratorCapacitySnapshot(ctx context.Context) (available, total map[string]int64, byNode map[string]map[string]int64, nodeLabels map[string]map[string]string, err error)

	ProvisionAgent(ctx context.Context, agentID string) error
}
