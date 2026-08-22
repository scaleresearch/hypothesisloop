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
	// ListManagedJobsForStatus is the set a status push reports on, which is not the set
	// reconcile acts on: a job being deleted and recreated is still real and must keep being
	// reported, or the control plane reads its absence from a complete snapshot as gone and
	// evicts live work.
	ListManagedJobsForStatus(ctx context.Context) ([]string, error)
	ListManagedAuxiliaryWorkloads(ctx context.Context) ([]string, error)
	WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error)

	// ReapTerminal prunes whatever terminal records the ecosystem retains for jobs no longer
	// desired. Nothing to prune is a valid implementation, not a missing one — k8s deletes the
	// Job object itself, so its executor has no separate record to remove.
	ReapTerminal(ctx context.Context, desired map[string]*domain.Experiment) error

	// FetchLogTail and PollPhaseDetail are how a job explains itself: the runtime watching the
	// job relays its output and its non-start reason, never the job process (important.md #16).
	FetchLogTail(ctx context.Context, experimentID string, maxLines int) ([]string, error)
	PollPhaseDetail(ctx context.Context, experimentID string) (reason, message string, restartCount int32, err error)

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

	// SupportsMultiNodeJobs reports whether this runtime can execute a job spanning more than one
	// node — a fact about the runtime, reported to the control plane alongside capacity so
	// admission never places a distributed job somewhere it cannot run. It is deliberately a
	// reported FACT and not a rule handed over for the runtime to evaluate: the control plane
	// still makes the placement decision, it just gets to know this one thing about the cluster.
	//
	// Before this existed, a distributed job placed on a single-node runtime was admitted, held a
	// reservation, and failed at workload creation — an admission-time question answered at
	// execution time.
	SupportsMultiNodeJobs() bool

	ProvisionAgent(ctx context.Context, agentID string) error
}
