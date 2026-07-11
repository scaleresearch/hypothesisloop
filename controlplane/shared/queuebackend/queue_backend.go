// Package queuebackend implements workload.Backend without ever calling into a target
// cluster. Every method reads or writes Postgres only (via db.ClusterQueueStore). There is no
// command outbox and no separate "delete" call: the experiment's status (already updated by
// services/scheduler before Backend is called) is itself the desired state a cluster-agent
// reconciles against — the same way updating a Deployment's spec, not issuing an imperative
// RPC, is what tells a kubelet what to run. experiments.status is the *only* record of
// desired state; nothing here duplicates it. The control plane never holds cluster
// credentials and never opens a connection to a cluster's API server.
package queuebackend

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/workload"
)

// Store is the persistence interface Backend needs. Satisfied by *db.ClusterQueueStore.
type Store interface {
	GetJobReport(ctx context.Context, experimentID string) (*db.JobReport, error)
	DeleteJobReport(ctx context.Context, experimentID string) error
	GetLiveCPUCapacity(ctx context.Context, connectedWithin time.Duration) (float64, error)
}

// Backend implements workload.Backend purely against Postgres. It is symmetric with
// workload.ClusterSet's method set so it's a drop-in replacement at the cmd/*/main.go wiring
// point; the difference is entirely in how each method is implemented.
type Backend struct {
	store         Store
	clusterNames  []string
	staticFlavors *workload.OpenResearchConfig // for GetFlavorCapacity fallback until live capacity reporting exists
	// connectedWithin mirrors clusteragentapi.Handler's own staleness window (config-driven,
	// scheduler.cluster_unreachable_after_seconds — one threshold, not two hardcoded ones): a
	// cluster whose heartbeat (and therefore its live CPU report) is older than this is treated
	// as contributing zero capacity rather than a possibly-stale number. This is also what
	// "freezes new admission" to an Unreachable cluster (item #6) — no separate freeze logic
	// needed, since its capacity simply drops out of the sum below.
	connectedWithin time.Duration
}

// New constructs a Backend. clusterNames are the configured target clusters (used for
// ClusterNames()); flavorCfg feeds the static GPU-flavor GetFlavorCapacity fallback (no GPU
// clusters wired up yet). connectedWithin is the cluster-liveness threshold (config-driven).
func New(store Store, clusterNames []string, flavorCfg *workload.OpenResearchConfig, connectedWithin time.Duration) *Backend {
	return &Backend{store: store, clusterNames: clusterNames, staticFlavors: flavorCfg, connectedWithin: connectedWithin}
}

var _ workload.Backend = (*Backend)(nil)

// SetupCluster is a no-op here: namespace and PriorityClass creation happens on the
// cluster-agent side (it has in-cluster credentials; the control plane does not).
func (b *Backend) SetupCluster(_ context.Context) error { return nil }

// CreateWorkload is a no-op: the caller has already updated exp's status to SUBMITTED (or
// similar) in the database before calling this, and that status change is exactly what a
// cluster-agent's desired-state reconciliation reads to know a Job should exist. There is
// nothing further to do — deliberately not even a database write, to avoid a second place
// that could disagree with experiments.status.
func (b *Backend) CreateWorkload(_ context.Context, _ *domain.Experiment) error { return nil }

// WaitForJobDeletion polls the local job-report row (populated by cluster-agent pushes)
// until it disappears or reports Gone, instead of asking the cluster. Callers must have
// already updated exp's status away from SUBMITTED/ADMITTED/RUNNING before calling this —
// otherwise the cluster-agent still considers the job desired and will never delete it.
func (b *Backend) WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		report, err := b.store.GetJobReport(ctx, exp.ID)
		if err != nil {
			return err
		}
		if report == nil || workload.ParseJobPhase(report.Phase) == workload.JobPhaseGone {
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

// PollJobPhase reads the last phase a cluster-agent pushed for exp — never asks the cluster.
func (b *Backend) PollJobPhase(ctx context.Context, exp *domain.Experiment) (workload.JobPhase, error) {
	report, err := b.store.GetJobReport(ctx, exp.ID)
	if err != nil {
		return workload.JobPhasePending, err
	}
	if report == nil {
		return workload.JobPhasePending, nil
	}
	return workload.ParseJobPhase(report.Phase), nil
}

// GetAdmittedGPUType reads the flavor a cluster-agent reported back, if any; falls back to
// the requested type if no report has arrived yet.
func (b *Backend) GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType {
	report, err := b.store.GetJobReport(ctx, exp.ID)
	if err != nil || report == nil || report.AdmittedGPUType == "" {
		return exp.GPUType
	}
	return report.AdmittedGPUType
}

// cpuFlavor is the pseudo-flavor key used for CPU-only jobs (no GPU request) in the same
// guaranteed/burst maps GPU flavors use — see scheduler/loop.go's admissionUnit.
const cpuFlavor = "cpu"

// GetFlavorCapacity returns GPU-flavor capacity from static config (no GPU clusters are wired
// up yet — see design doc's own production-migration item for that) plus real, live CPU-core
// capacity summed from cluster-agents' own self-reported allocatable-minus-requested numbers
// (GetLiveCPUCapacity), since CPU jobs are what's actually submitted today. Guaranteed and
// burst share the same physical pool, same as the GPU flavors below — preemption is what
// enforces the tier boundary, not a capacity split.
func (b *Backend) GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]int64, err error) {
	nominal := gpuNominalCapacity(b.staticFlavors)
	guaranteed = make(map[string]int64, len(nominal)+1)
	burst = make(map[string]int64, len(nominal)+1)
	for flavor, n := range nominal {
		guaranteed[flavor] = n
		burst[flavor] = n
	}

	cpuAvail, err := b.store.GetLiveCPUCapacity(ctx, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("queuebackend: live CPU capacity: %w", err)
	}
	cpuCores := int64(cpuAvail) // truncates down: never round available capacity up in the admission loop's favor
	guaranteed[cpuFlavor] = cpuCores
	burst[cpuFlavor] = cpuCores
	return guaranteed, burst, nil
}

// ClusterNames returns the configured target cluster names.
func (b *Backend) ClusterNames() []string {
	return b.clusterNames
}

// ProvisionAgent is a no-op: the native scheduling model has no per-agent k8s object to
// create (see workload.Backend.ProvisionAgent doc).
func (b *Backend) ProvisionAgent(_ context.Context, _ string) error { return nil }

var defaultFlavorOrder = []string{"flavor-t4", "flavor-l40", "flavor-a100", "flavor-h100", "flavor-h200"}
var defaultGPUsByFlavor = map[string]int{
	"flavor-t4": 64, "flavor-l40": 16, "flavor-a100": 8, "flavor-h100": 4, "flavor-h200": 2,
}

// gpuNominalCapacity returns the nominal GPU slot count per flavor from config (or the PoC
// defaults). GPUs are the only resource dimension the scheduler accounts for.
func gpuNominalCapacity(cfg *workload.OpenResearchConfig) map[string]int64 {
	order, gpus := defaultFlavorOrder, defaultGPUsByFlavor
	if cfg != nil {
		if len(cfg.FlavorOrder) > 0 {
			order = cfg.FlavorOrder
		}
		if len(cfg.GPUsByFlavor) > 0 {
			gpus = cfg.GPUsByFlavor
		}
	}
	out := make(map[string]int64, len(order))
	for _, flavor := range order {
		out[flavor] = int64(gpus[flavor])
	}
	return out
}
