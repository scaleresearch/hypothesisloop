package clusterexchange

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Reconcile records one complete actual-capacity snapshot of clusterName, its runtime running
// agentVersion, and answers the cluster's complete desired state. A refusal is kept as the
// cluster's last error until a reconcile is accepted.
func (s *Service) Reconcile(ctx context.Context, clusterName, agentVersion string, report clusterapi.ReconcileRequest) (answer clusterapi.ReconcileAnswer, err error) {
	defer func() { s.recordContact(ctx, clusterName, err) }()
	if err := checkAgentVersion(agentVersion); err != nil {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid("unsupported_version", err.Error()).On(clusterapi.AgentVersionHeader)
	}

	if report.AcceleratorAvailableByType == nil || report.AcceleratorTotalByType == nil || report.AcceleratorAvailableByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "complete accelerator capacity report is required").On("accelerator_total_by_type")
	}
	s.keepPricedAccelerators(&report)
	if report.NodeResourcesByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node resource capacity report is required").On("node_resources_by_node")
	}
	if report.NodeResourcesTotalByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node total resource capacity report is required").On("node_resources_total_by_node")
	}
	for node, byResource := range report.NodeResourcesByNode {
		if node == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node resource report has empty node identity").On("node_resources_by_node")
		}
		for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
			if _, present := byResource[key]; !present {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node resource report for "+node+" is missing "+key).On("node_resources_by_node")
			}
		}
		for resource, available := range byResource {
			if resource == "" || available < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "invalid per-node resource capacity value").On("node_resources_by_node")
			}
		}
		total, present := report.NodeResourcesTotalByNode[node]
		if !present {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node total resource report for "+node+" is missing").On("node_resources_total_by_node")
		}
		for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
			totalValue, present := total[key]
			if !present || totalValue < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node total resource report for "+node+" is missing or invalid "+key).On("node_resources_total_by_node")
			}
			if byResource[key] > totalValue {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node available resource exceeds total for "+node).On("node_resources_by_node")
			}
		}
	}
	if report.CPUAvailableCores < 0 || report.CPUTotalCores < 0 || report.CPUAvailableCores > report.CPUTotalCores {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "invalid CPU capacity values").On("cpu_available_cores")
	}
	for acceleratorType, total := range report.AcceleratorTotalByType {
		if acceleratorType == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "accelerator capacity contains empty accelerator type").On("accelerator_total_by_type")
		}
		available, present := report.AcceleratorAvailableByType[acceleratorType]
		if !present || total < 0 || available < 0 || available > total {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "invalid accelerator capacity values").On("accelerator_available_by_type")
		}
	}
	if len(report.AcceleratorAvailableByType) != len(report.AcceleratorTotalByType) {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "accelerator available and total accelerator type sets must match").On("accelerator_available_by_type")
	}
	availableByNode := make(map[string]int64, len(report.AcceleratorAvailableByType))
	for node, acceleratorTypes := range report.AcceleratorAvailableByNode {
		if node == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node accelerator report has empty node identity").On("accelerator_available_by_node")
		}
		for acceleratorType, available := range acceleratorTypes {
			if acceleratorType == "" || available < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "invalid per-node accelerator capacity value").On("accelerator_available_by_node")
			}
			if _, present := report.AcceleratorTotalByType[acceleratorType]; !present {
				return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node accelerator type is absent from cluster totals").On("accelerator_available_by_node")
			}
			availableByNode[acceleratorType] += available
		}
	}
	for acceleratorType, byNode := range availableByNode {
		if byNode > report.AcceleratorAvailableByType[acceleratorType] {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "per-node accelerator capacity exceeds cluster availability").On("accelerator_available_by_node")
		}
	}

	if report.RAMAvailableBytes < 0 || report.RAMTotalBytes < 0 || report.RAMAvailableBytes > report.RAMTotalBytes ||
		report.StorageAvailableBytes < 0 || report.StorageTotalBytes < 0 || report.StorageAvailableBytes > report.StorageTotalBytes {
		return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, "invalid memory or storage capacity values").On("ram_available_bytes")
	}

	// The scheduler knows a connected cluster by the id its runtime reports, so an id is one cluster's and
	// a cluster has one. A runtime stating another is refused before anything it reports is
	// recorded.
	if report.ClusterID != "" {
		if err := domain.ValidateClusterID(report.ClusterID); err != nil {
			return clusterapi.ReconcileAnswer{}, refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("cluster_id")
		}
		switch err := s.store.BindReportedID(ctx, clusterName, report.ClusterID); {
		case errors.Is(err, db.ErrClusterNotFound):
			return clusterapi.ReconcileAnswer{}, auth.NotFound("cluster")
		case errors.Is(err, db.ErrClusterIDBound):
			return clusterapi.ReconcileAnswer{}, refusal.Refuse(http.StatusConflict, "cluster_identity_conflict", "cluster_id "+report.ClusterID+" is the identity of another registered cluster")
		case errors.Is(err, db.ErrClusterIDChanged):
			return clusterapi.ReconcileAnswer{}, refusal.Refuse(http.StatusConflict, "cluster_identity_conflict", "cluster "+clusterName+" is bound to another cluster_id than the "+report.ClusterID+
				" this runtime reports: if its machines were rebuilt, its owner forgets the old identity (hl cluster forget-identity "+clusterName+")")
		case err != nil:
			return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
	}

	if err := s.metrics.RecordClusterCapacitySnapshot(ctx, metricsdb.ClusterCapacitySnapshot{
		ClusterName: clusterName, At: time.Now().UTC(),
		CPUAvailable: report.CPUAvailableCores, CPUTotal: report.CPUTotalCores,
		AcceleratorAvailable: report.AcceleratorAvailableByType, AcceleratorTotal: report.AcceleratorTotalByType,
		AcceleratorAvailableByNode: report.AcceleratorAvailableByNode,
		NodeResourcesByNode:        report.NodeResourcesByNode,
		NodeResourcesTotalByNode:   report.NodeResourcesTotalByNode,
		NodeLabelsByNode:           report.NodeLabelsByNode,
		MultiNodeCapable:           report.MultiNodeCapable,
		AutoscalerEnabled:          report.AutoscalerEnabled,
		ClusterID:                  report.ClusterID,
		AgentVersion:               agentVersion,
		RAMAvailable:               report.RAMAvailableBytes, RAMTotal: report.RAMTotalBytes,
		StorageAvailable: report.StorageAvailableBytes, StorageTotal: report.StorageTotalBytes,
	}); err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}

	exps, err := s.store.ListDesiredWorkloads(ctx, clusterName)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}
	// The other half of the same view: what should NO LONGER exist, and for how much longer
	// it may. A job the platform itself decided to stop -- preempted, cut, out of quota, out
	// of time -- is fine and its work is worth saving, so it is told termination is coming
	// and gets the window it declared before it is killed. A job the environment or its own
	// code ended gets no window; it simply never appears here.
	//
	// Derived from the termination that is already recorded on the experiment, and expiring
	// with maxCheckpointGrace, so there is no terminating state to write, advance or clear.
	undesired, err := s.store.ListRecentlyUndesiredWorkloads(ctx, clusterName, s.maxCheckpointGrace)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}
	answer.Jobs = make([]*clusterapi.DesiredJob, len(exps))
	for i, exp := range exps {
		if answer.Jobs[i], err = s.desiredJob(ctx, exp); err != nil {
			return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
	}
	answer.CheckpointWindow = domain.CheckpointWindowGrants(undesired)
	return answer, nil
}

// MaxReconcileBytes bounds one capacity report. Its size grows with nodes times the labels each
// carries: a thousand nodes with a hundred labels apiece is about 10 MiB.
const MaxReconcileBytes = 32 << 20

// recordContact keeps what a reconcile of the cluster was refused with, or clears it once one is
// accepted. A failure of the platform's own is not the cluster's to fix, and an unregistered
// name has nowhere to keep it.
func (s *Service) recordContact(ctx context.Context, cluster string, err error) {
	refusal := ""
	if err != nil {
		var status interface {
			error
			GetStatus() int
		}
		if !errors.As(err, &status) || status.GetStatus() >= http.StatusInternalServerError || status.GetStatus() == http.StatusNotFound {
			return
		}
		refusal = status.Error()
	}
	if err := s.store.RecordContactRefusal(ctx, cluster, refusal); err != nil {
		s.logger.Warn("could not keep the last refusal of a cluster's reconcile", zap.String("cluster", cluster), zap.Error(err))
	}
}
