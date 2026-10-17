package clusterexchange

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
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
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, err.Error())
	}

	if report.AcceleratorAvailableByFlavor == nil || report.AcceleratorTotalByFlavor == nil || report.AcceleratorAvailableByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "complete accelerator capacity report is required")
	}
	s.keepPricedAccelerators(&report)
	if report.NodeResourcesByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node resource capacity report is required")
	}
	if report.NodeResourcesTotalByNode == nil {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node total resource capacity report is required")
	}
	for node, byResource := range report.NodeResourcesByNode {
		if node == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node resource report has empty node identity")
		}
		for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
			if _, present := byResource[key]; !present {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node resource report for "+node+" is missing "+key)
			}
		}
		for resource, available := range byResource {
			if resource == "" || available < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "invalid per-node resource capacity value")
			}
		}
		total, present := report.NodeResourcesTotalByNode[node]
		if !present {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node total resource report for "+node+" is missing")
		}
		for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
			totalValue, present := total[key]
			if !present || totalValue < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node total resource report for "+node+" is missing or invalid "+key)
			}
			if byResource[key] > totalValue {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node available resource exceeds total for "+node)
			}
		}
	}
	if report.CPUAvailableCores < 0 || report.CPUTotalCores < 0 || report.CPUAvailableCores > report.CPUTotalCores {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "invalid CPU capacity values")
	}
	for flavor, total := range report.AcceleratorTotalByFlavor {
		if flavor == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "accelerator capacity contains empty flavor")
		}
		available, present := report.AcceleratorAvailableByFlavor[flavor]
		if !present || total < 0 || available < 0 || available > total {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "invalid accelerator capacity values")
		}
	}
	if len(report.AcceleratorAvailableByFlavor) != len(report.AcceleratorTotalByFlavor) {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "accelerator available and total flavor sets must match")
	}
	availableByNode := make(map[string]int64, len(report.AcceleratorAvailableByFlavor))
	for node, flavors := range report.AcceleratorAvailableByNode {
		if node == "" {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node accelerator report has empty node identity")
		}
		for flavor, available := range flavors {
			if flavor == "" || available < 0 {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "invalid per-node accelerator capacity value")
			}
			if _, present := report.AcceleratorTotalByFlavor[flavor]; !present {
				return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node accelerator flavor is absent from cluster totals")
			}
			availableByNode[flavor] += available
		}
	}
	for flavor, byNode := range availableByNode {
		if byNode > report.AcceleratorAvailableByFlavor[flavor] {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "per-node accelerator capacity exceeds cluster availability")
		}
	}

	if report.RAMAvailableBytes < 0 || report.RAMTotalBytes < 0 || report.RAMAvailableBytes > report.RAMTotalBytes ||
		report.StorageAvailableBytes < 0 || report.StorageTotalBytes < 0 || report.StorageAvailableBytes > report.StorageTotalBytes {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "invalid memory or storage capacity values")
	}

	// The scheduler knows a connected cluster by the id its runtime reports, so an id is one cluster's and
	// a cluster has one. A runtime stating another is refused before anything it reports is
	// recorded.
	if report.ClusterID != "" {
		if err := domain.ValidateClusterID(report.ClusterID); err != nil {
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, err.Error())
		}
		switch err := s.store.BindReportedID(ctx, clusterName, report.ClusterID); {
		case errors.Is(err, db.ErrClusterNotFound):
			return clusterapi.ReconcileAnswer{}, auth.NotFound("cluster")
		case errors.Is(err, db.ErrClusterIDBound):
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusConflict, "cluster_id "+report.ClusterID+" is the identity of another registered cluster")
		case errors.Is(err, db.ErrClusterIDChanged):
			return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusConflict, "cluster "+clusterName+" is bound to another cluster_id than the "+report.ClusterID+
				" this runtime reports: if its machines were rebuilt, its owner forgets the old identity (hl cluster forget-identity "+clusterName+")")
		case err != nil:
			return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
	}

	// Placement reads the image, so it is kept before the capacity that makes the cluster a
	// candidate.
	if len(report.JobImage) > domain.MaxJobImageLength {
		return clusterapi.ReconcileAnswer{}, refusal.Reject(http.StatusBadRequest, "job_image is longer than "+strconv.Itoa(domain.MaxJobImageLength)+" characters")
	}
	onlyImage := report.JobImage
	if report.JobImageDefault {
		onlyImage = ""
	}
	if err := s.store.RecordJobImage(ctx, clusterName, onlyImage); err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}
	if err := s.store.RecordBuilds(ctx, clusterName, report.Builds && s.environments != nil); err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}

	if err := s.metrics.RecordClusterCapacitySnapshot(ctx, metricsdb.ClusterCapacitySnapshot{
		ClusterName: clusterName, At: time.Now().UTC(),
		CPUAvailable: report.CPUAvailableCores, CPUTotal: report.CPUTotalCores,
		AcceleratorAvailable: report.AcceleratorAvailableByFlavor, AcceleratorTotal: report.AcceleratorTotalByFlavor,
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
		if answer.Jobs[i], err = s.desiredJob(exp); err != nil {
			return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
	}
	answer.CheckpointWindow = domain.CheckpointWindowGrants(undesired)
	if answer.Builds, answer.RegistryPull, err = s.environmentWork(ctx, clusterName, exps); err != nil {
		return clusterapi.ReconcileAnswer{}, auth.Internal(s.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
	}
	return answer, nil
}

// environmentWork is the builds assigned to cluster, and the pull login when one of exps, the
// workloads desired on it, runs in an environment.
func (s *Service) environmentWork(ctx context.Context, cluster string, exps []*domain.Experiment) ([]domain.EnvironmentBuild, *domain.RegistryLogin, error) {
	builds := []domain.EnvironmentBuild{}
	if s.environments == nil {
		return builds, nil, nil
	}
	assigned, err := s.store.AssignedEnvironmentBuilds(ctx, cluster)
	if err != nil {
		return nil, nil, err
	}
	for _, env := range assigned {
		builds = append(builds, domain.EnvironmentBuild{Key: env.Key, Attempt: env.Attempt, Base: env.Base, Tools: env.Tools, Dockerfile: env.Dockerfile, Repository: s.environments.Repository,
			CacheScope: domain.EnvironmentCacheScope(env.OrgID, env.GitRepository)})
	}
	if !slices.ContainsFunc(exps, func(exp *domain.Experiment) bool { return exp.EnvironmentKey != "" }) {
		return builds, nil, nil
	}
	// Without a login only a pull fails, and a desired state that is refused instead has the
	// runtime act on none of it.
	login, err := s.environments.Logins.Pull(ctx)
	if err != nil {
		s.logger.Error("cluster reconcile: mint registry pull login", zap.String("cluster", cluster), zap.Error(err))
		return builds, nil, nil
	}
	return builds, &login, nil
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
