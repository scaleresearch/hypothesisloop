package workload

import (
	"context"

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

	// There is deliberately no DeleteWorkload: the source of truth for "should exp's job exist"
	// is exp's own status. A caller that needs a job gone (preemption, eviction) moves it out of
	// SUBMITTED/ADMITTED/RUNNING and the cluster agent reconciles it away.
	//
	// CreateWorkload places exp's job onto its target cluster. Called only after the control
	// plane's own admission decision (services/scheduler) — implementations are not expected
	// to queue or suspend; if the backend needs to do its own gating, do it here.
	CreateWorkload(ctx context.Context, exp *domain.Experiment) error

	// PollJobPhase reports the current lifecycle phase of exp's job.
	PollJobPhase(ctx context.Context, exp *domain.Experiment) (JobPhase, error)

	// PollPhaseDetail is the runtime's latest explanation for why a job has not started (see
	// domain.PhaseDetail). found=false means no runtime has reported one yet.
	PollPhaseDetail(ctx context.Context, exp *domain.Experiment) (reason, message string, restartCount int32, found bool, err error)

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
	// GetNodeTotalCapacity reports each node's capacity available to PLATFORM-scheduled jobs —
	// allocatable minus non-platform-pod requests (DaemonSets, CNI, monitoring, anything else
	// permanently resident; platform job pods themselves are not subtracted) — as
	// cluster -> node -> domain.NodeResource* -> amount — the same shape GetNodeResourceCapacity
	// returns, but total rather than free. Free capacity moves every time something is scheduled
	// or freed; this is the stable per-node denominator fair-share math needs instead.
	GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error)
	GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error)

	// GetMultiNodeCapability reports, per cluster, whether its runtime can execute a job spanning
	// more than one node — a capability each cluster reports about itself alongside its capacity,
	// not a rule the control plane hands out. Admission filters on it, so a distributed job is
	// never placed on a single-node runtime. A cluster absent from the map has no fresh report and
	// is treated as incapable.
	GetMultiNodeCapability(ctx context.Context) (map[string]bool, error)

	// GetAutoscalerCapability reports, per cluster, whether it sits behind a native autoscaler
	// (cluster-autoscaler / Karpenter) that reacts to Pending pods. Operator-set, fail-closed: a
	// cluster absent from the map has no fresh report and is treated as not-autoscaled.
	GetAutoscalerCapability(ctx context.Context) (map[string]bool, error)

	// GetClusterIDs reports each connected cluster's runtime-derived stable identity
	// (kube-system namespace UID / machine-id), keyed by cluster_name. Speculative admission's
	// tried-cluster bookkeeping keys on cluster_id so a rename can't split or merge history; a
	// cluster whose agent has not yet started sending cluster_id is absent from the map.
	GetClusterIDs(ctx context.Context) (map[string]string, error)

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
