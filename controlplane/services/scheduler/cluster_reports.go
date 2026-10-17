package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// DesiredFootprints is the desired state ClusterReports subtracts from installed capacity.
// Satisfied by *db.Store.
type DesiredFootprints interface {
	SumDesiredFootprintByCluster(ctx context.Context) (map[string]domain.Footprint, error)
}

// ClusterReports answers the scheduler's reads of what clusters report: capacity, phases and
// output from the metrics store, net of the desired footprint PostgreSQL holds. It never
// connects to a cluster; a job's status row is the desired state its cluster reconciles against.
type ClusterReports struct {
	store DesiredFootprints
	// metricsDBURL is where cluster-agents' live capacity and job reports live: metric data,
	// read live, never duplicated into Postgres.
	metricsDBURL string
	// connectedWithin mirrors clusteragentapi.Handler's staleness window
	// (scheduler.cluster_unreachable_after_seconds): a cluster whose heartbeat is older than
	// this contributes zero capacity instead of a possibly-stale number. This also freezes new
	// admission to an Unreachable cluster, since its capacity just drops out of the sum below.
	connectedWithin time.Duration
}

// NewClusterReports reads desired footprint from store and reports from the metrics store at
// metricsDBURL, counting a cluster live while it reported within connectedWithin.
func NewClusterReports(store DesiredFootprints, metricsDBURL string, connectedWithin time.Duration) (*ClusterReports, error) {
	if store == nil {
		return nil, fmt.Errorf("scheduler: desired footprint store is required")
	}
	if metricsDBURL == "" {
		return nil, fmt.Errorf("scheduler: metrics DB URL is required")
	}
	if connectedWithin <= 0 {
		return nil, fmt.Errorf("scheduler: cluster liveness window must be positive")
	}
	return &ClusterReports{store: store, metricsDBURL: metricsDBURL, connectedWithin: connectedWithin}, nil
}

var (
	_ ClusterCapacity   = (*ClusterReports)(nil)
	_ JobReports        = (*ClusterReports)(nil)
	_ InstalledCapacity = (*ClusterReports)(nil)
)

// LatestJobPhase reads the latest fresh phase metric pushed by the cluster agent. found=false means
// the cluster has no status snapshot inside the liveness window, or none since exp was admitted.
//
// A snapshot taken before submitted_at is of the admission before this one: a job requeued and
// claimed again between two snapshots was read as ended by the report that requeued it. A stored
// phase names no attempt, and one could not tell the two apart anyway, since a preempted job
// keeps its attempt. The window is cut to whole seconds, so it never reaches back past the claim.
func (b *ClusterReports) LatestJobPhase(ctx context.Context, exp *domain.Experiment) (domain.JobPhase, bool, error) {
	if exp.SubmittedAt == nil {
		return domain.JobPhasePending, false, fmt.Errorf("scheduler: experiment %s has no submission time to read its phase against", exp.ID)
	}
	window := min(b.connectedWithin, time.Since(*exp.SubmittedAt).Truncate(time.Second))
	if window <= 0 {
		return domain.JobPhasePending, false, nil
	}
	return metricsdb.LatestJobPhase(ctx, b.metricsDBURL, exp.ID, exp.ClusterName, window)
}

// LatestPhaseDetail reads the runtime's latest reported explanation for why exp's container
// hasn't started (or has been restarting). found=false means nothing has been reported yet —
// distinct from an empty reason, which just means the runtime has nothing notable to say.
func (b *ClusterReports) LatestPhaseDetail(ctx context.Context, exp *domain.Experiment) (reason, message string, restartCount int32, scheduledNodes int32, schedulingReason string, found bool, err error) {
	row, found, err := metricsdb.GetLatestPhaseDetailFull(ctx, b.metricsDBURL, exp.ID, exp.ClusterName, exp.AttemptCount)
	if err != nil || !found {
		return "", "", 0, 0, "", found, err
	}
	return row.Reason, row.Message, row.RestartCount, row.ScheduledNodes, row.SchedulingReason, true, nil
}

// PhaseReportedSince is when exp's current attempt on its cluster first reported reason.
func (b *ClusterReports) PhaseReportedSince(ctx context.Context, exp *domain.Experiment, reason string) (time.Time, bool, error) {
	return metricsdb.PhaseReportedSince(ctx, b.metricsDBURL, exp.ID, exp.ClusterName, exp.AttemptCount, reason)
}

// LogTail reads up to the last n lines of exp's most recently reported output.
func (b *ClusterReports) LogTail(ctx context.Context, exp *domain.Experiment, n int) ([]string, error) {
	return metricsdb.GetLatestLogTail(ctx, b.metricsDBURL, exp.ID, n)
}

// GetAdmittedAcceleratorType reads the latest observed type metric for exp's current admission.
//
// The lookback is clamped to the time since submitted_at, which Postgres rewrites on every
// ClaimSubmitted and clears on requeue. A job re-admitted onto a different flavor after
// preemption would otherwise be readable through the tail of its previous pod's samples, and
// comparing this against the newly reserved flavor would report a placement mismatch that
// happened one admission ago.
//
// found=false means no sample of this admission names a type yet.
func (b *ClusterReports) GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, bool, error) {
	if exp.SubmittedAt == nil {
		return "", false, fmt.Errorf("scheduler: experiment %s has no submission time to read its accelerator type against", exp.ID)
	}
	now := time.Now().UTC()
	window := b.connectedWithin
	if sinceSubmit := now.Sub(*exp.SubmittedAt); sinceSubmit < window {
		window = sinceSubmit
	}
	if window <= 0 {
		return "", false, nil
	}
	return metricsdb.LatestAcceleratorType(ctx, b.metricsDBURL, exp.ID, now, window)
}

// GetFlavorCapacity returns two views of each connected cluster's free capacity:
//   - desiredFree = capacity left to platform jobs minus every PostgreSQL SUBMITTED/RUNNING
//     footprint: what is free once the cluster has converged on desired state;
//   - available = the dimension-wise minimum of desiredFree and the cluster agent's fresh
//     allocatable-minus-requested observation: what can be claimed now.
//
// Desired-free closes the creation-delay race right after MarkSubmitted; actual-free closes
// the deletion-delay race after preemption/eviction. Taking the minimum prevents overcommit.
// A cluster whose snapshot is missing a metric or contradicts itself is left out of both maps,
// like a disconnected one, so the other clusters are still admitted to. The maps are then valid
// and err is an InconsistentSnapshots naming each cluster left out and why.
//
// CPU, memory and storage totals are the per-node figures net of non-platform pods, so a
// desiredFree above available there is always a platform job still terminating. Accelerator
// totals are raw allocatable: the runtime does not report them net of non-platform pods.
func (b *ClusterReports) GetFlavorCapacity(ctx context.Context) (available, desiredFree map[string]domain.Footprint, err error) {
	heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: live clusters: %w", err)
	}
	nodeTotals, err := metricsdb.LiveClusterNodeTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: total node capacity: %w", err)
	}
	cpuAvailable, err := metricsdb.LiveClusterCPUCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: available CPU capacity: %w", err)
	}
	acceleratorAvailable, acceleratorTotal, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: accelerator capacity: %w", err)
	}
	ramAvailable, err := metricsdb.LiveClusterRAMCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: available RAM capacity: %w", err)
	}
	storageAvailable, err := metricsdb.LiveClusterStorageCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: available storage capacity: %w", err)
	}
	desiredByCluster, err := b.store.SumDesiredFootprintByCluster(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: desired footprint: %w", err)
	}
	cpuTotal, ramTotal, storageTotal := sumNodeTotals(nodeTotals)
	inconsistent := b.validateCapacitySnapshot(heartbeats, cpuAvailable, cpuTotal, acceleratorAvailable, acceleratorTotal, ramAvailable, ramTotal, storageAvailable, storageTotal)

	available = make(map[string]domain.Footprint, len(heartbeats))
	desiredFree = make(map[string]domain.Footprint, len(heartbeats))
	for cluster, connected := range heartbeats {
		if !connected || inconsistent[cluster] != nil {
			continue
		}
		total := domain.CapacityFootprint(0, acceleratorTotal[cluster], ramTotal[cluster], storageTotal[cluster])
		total[domain.ResourceKey{Kind: domain.ResourceKindCPU}] = cpuTotal[cluster]
		actualFree := domain.CapacityFootprint(cpuAvailable[cluster], acceleratorAvailable[cluster], ramAvailable[cluster], storageAvailable[cluster])
		desiredFree[cluster] = total.Sub(desiredByCluster[cluster])
		available[cluster] = minimumFootprint(desiredFree[cluster], actualFree)
	}
	if len(inconsistent) > 0 {
		return available, desiredFree, InconsistentSnapshots(inconsistent)
	}
	return available, desiredFree, nil
}

// InconsistentSnapshots is why each connected cluster was left out of a capacity read.
type InconsistentSnapshots map[string]error

func (e InconsistentSnapshots) Error() string {
	reasons := make([]string, 0, len(e))
	for _, err := range e {
		reasons = append(reasons, err.Error())
	}
	sort.Strings(reasons)
	return strings.Join(reasons, "; ")
}

// sumNodeTotals adds up each cluster's per-node capacity left to platform jobs: CPU in
// millicores, memory and storage in bytes.
func sumNodeTotals(byNode map[string]map[string]map[string]int64) (cpuMillicores, ramBytes, storageBytes map[string]int64) {
	cpuMillicores, ramBytes, storageBytes = map[string]int64{}, map[string]int64{}, map[string]int64{}
	for cluster, nodes := range byNode {
		for _, total := range nodes {
			cpuMillicores[cluster] += total[domain.NodeResourceCPUMillicores]
			ramBytes[cluster] += total[domain.NodeResourceMemoryBytes]
			storageBytes[cluster] += total[domain.NodeResourceStorageBytes]
		}
	}
	return cpuMillicores, ramBytes, storageBytes
}

// validateCapacitySnapshot returns, per connected cluster whose snapshot cannot be trusted, why.
func (b *ClusterReports) validateCapacitySnapshot(
	heartbeats map[string]bool,
	cpuAvailable map[string]float64, cpuTotalMillicores map[string]int64,
	acceleratorAvailable, acceleratorTotal map[string]map[string]int64,
	ramAvailable, ramTotal, storageAvailable, storageTotal map[string]int64,
) map[string]error {
	inconsistent := map[string]error{}
	for cluster, connected := range heartbeats {
		if !connected {
			continue
		}
		if err := validateClusterSnapshot(cluster, cpuAvailable, cpuTotalMillicores, acceleratorAvailable, acceleratorTotal, ramAvailable, ramTotal, storageAvailable, storageTotal); err != nil {
			inconsistent[cluster] = err
		}
	}
	return inconsistent
}

func validateClusterSnapshot(
	cluster string,
	cpuAvailable map[string]float64, cpuTotalMillicores map[string]int64,
	acceleratorAvailable, acceleratorTotal map[string]map[string]int64,
	ramAvailable, ramTotal, storageAvailable, storageTotal map[string]int64,
) error {
	cpuAvail, cpuAvailOK := cpuAvailable[cluster]
	cpuTot, cpuTotalOK := cpuTotalMillicores[cluster]
	ramAvail, ramAvailOK := ramAvailable[cluster]
	ramTot, ramTotalOK := ramTotal[cluster]
	storageAvail, storageAvailOK := storageAvailable[cluster]
	storageTot, storageTotalOK := storageTotal[cluster]
	accelAvail, accelAvailOK := acceleratorAvailable[cluster]
	accelTotal, accelTotalOK := acceleratorTotal[cluster]
	if !cpuAvailOK || !cpuTotalOK || !ramAvailOK || !ramTotalOK || !storageAvailOK || !storageTotalOK {
		return fmt.Errorf("scheduler: incomplete live capacity snapshot for active cluster %q", cluster)
	}
	if accelAvailOK != accelTotalOK {
		return fmt.Errorf("scheduler: incomplete live accelerator capacity for cluster %q", cluster)
	}
	if !accelAvailOK {
		accelAvail, accelTotal = map[string]int64{}, map[string]int64{}
	}
	if int64(cpuAvail*1000) > cpuTot || ramAvail > ramTot || storageAvail > storageTot {
		return fmt.Errorf("scheduler: available capacity exceeds total for active cluster %q", cluster)
	}
	if len(accelAvail) != len(accelTotal) {
		return fmt.Errorf("scheduler: mismatched live accelerator capacity sets for cluster %q", cluster)
	}
	for flavor, total := range accelTotal {
		available, availableOK := accelAvail[flavor]
		if !availableOK {
			return fmt.Errorf("scheduler: incomplete live accelerator capacity for cluster %q flavor %q", cluster, flavor)
		}
		if available > total {
			return fmt.Errorf("scheduler: available accelerator capacity exceeds total for cluster %q flavor %q", cluster, flavor)
		}
	}
	return nil
}

// GetAcceleratorCapacityByNode returns fresh actual per-node capacity for topology-aware
// distributed admission. Missing/stale nodes are absent and can't satisfy a hard spread requirement.
// A type is keyed in lower case, the form a footprint names it in: a node reports it as its
// driver cases it, and an accelerator type is one type however it is cased
// (domain.AcceleratorType.Is), so every reader looks it up exactly.
func (b *ClusterReports) GetAcceleratorCapacityByNode(ctx context.Context) (map[string]map[string]map[string]int64, error) {
	reported, err := metricsdb.LiveClusterNodeAcceleratorCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, err
	}
	return lowerAcceleratorTypes(reported), nil
}

func lowerAcceleratorTypes(reported map[string]map[string]map[string]int64) map[string]map[string]map[string]int64 {
	out := make(map[string]map[string]map[string]int64, len(reported))
	for cluster, byNode := range reported {
		out[cluster] = make(map[string]map[string]int64, len(byNode))
		for node, byType := range byNode {
			lowered := make(map[string]int64, len(byType))
			for acceleratorType, free := range byType {
				lowered[strings.ToLower(acceleratorType)] += free
			}
			out[cluster][node] = lowered
		}
	}
	return out
}

// GetNodeResourceCapacity reports fresh free CPU/memory/storage per node as
// cluster -> node -> domain.NodeResource* -> amount.
func (b *ClusterReports) GetNodeResourceCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error) {
	return metricsdb.LiveClusterNodeResourceCapacity(ctx, b.metricsDBURL, b.connectedWithin)
}

// GetNodeTotalCapacity reports each node's capacity available to PLATFORM-scheduled jobs —
// allocatable minus non-platform-pod requests (DaemonSets, CNI, monitoring, anything else
// permanently resident; platform job pods themselves are not subtracted), as
// cluster -> node -> domain.NodeResource* -> amount.
func (b *ClusterReports) GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]map[string]int64, error) {
	return metricsdb.LiveClusterNodeTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
}

// GetTotalCapacity returns each connected cluster's installed capacity, for consumers that need
// the cluster's shape rather than its free space. Only clusters with a complete fresh snapshot are
// returned; a cluster whose totals are missing or stale is omitted entirely, never zero-filled.
func (b *ClusterReports) GetTotalCapacity(ctx context.Context) (map[string]domain.Footprint, error) {
	heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("scheduler: live clusters: %w", err)
	}
	cpuTotal, err := metricsdb.LiveClusterCPUTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("scheduler: total CPU capacity: %w", err)
	}
	acceleratorTotal, err := metricsdb.LiveClusterAcceleratorTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("scheduler: total accelerator capacity: %w", err)
	}
	ramTotal, err := metricsdb.LiveClusterRAMTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("scheduler: total RAM capacity: %w", err)
	}
	storageTotal, err := metricsdb.LiveClusterStorageTotalCapacity(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil {
		return nil, fmt.Errorf("scheduler: total storage capacity: %w", err)
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

// GetMultiNodeCapability reports which clusters say their runtime can execute a job spanning more
// than one node. A cluster with no fresh report is absent, and admission reads absence as "cannot"
// — the same fail-closed rule every other capacity read here follows.
func (b *ClusterReports) GetMultiNodeCapability(ctx context.Context) (map[string]bool, error) {
	return metricsdb.LiveClusterMultiNodeCapability(ctx, b.metricsDBURL, b.connectedWithin)
}

// GetAutoscalerCapability reports which clusters say they sit behind a native autoscaler
// (cluster-autoscaler / Karpenter) that reacts to Pending pods. A cluster with no fresh report is
// absent, and speculative admission reads absence as "no autoscaler" — the same fail-closed rule
// GetMultiNodeCapability follows.
func (b *ClusterReports) GetAutoscalerCapability(ctx context.Context) (map[string]bool, error) {
	return metricsdb.LiveClusterAutoscalerCapability(ctx, b.metricsDBURL, b.connectedWithin)
}

func (b *ClusterReports) GetNodeLabels(ctx context.Context) (map[string]map[string]map[string]string, error) {
	return metricsdb.LiveClusterNodeLabels(ctx, b.metricsDBURL, b.connectedWithin)
}

// GetClusterIDs reports each connected cluster's runtime-derived stable identity (kube-system
// namespace UID / machine-id), keyed by cluster_name — the routing key everything else here uses.
// Speculative admission's tried-cluster bookkeeping keys on cluster_id (a rename must not split
// or merge it); a cluster whose agent has not yet started sending cluster_id is simply absent.
func (b *ClusterReports) GetClusterIDs(ctx context.Context) (map[string]string, error) {
	return metricsdb.LiveClusterIDs(ctx, b.metricsDBURL, b.connectedWithin)
}

func (b *ClusterReports) GetLiveClusters(ctx context.Context) (map[string]bool, error) {
	live, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, b.connectedWithin)
	if err != nil || domain.EnvironmentBuildHold <= b.connectedWithin {
		return live, err
	}
	heard, err := metricsdb.LiveClusterHeartbeats(ctx, b.metricsDBURL, domain.EnvironmentBuildHold)
	if err != nil {
		return nil, err
	}
	for name := range heard {
		live[name] = live[name]
	}
	return live, nil
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
