package clusterexchange

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

type occupancy struct{ busy, total int64 }

// occupancyByKind sums one cluster's reported accelerator types into devices and cores. The CPU type counts
// cores and an accelerator type devices, so the two are never one sum.
func occupancyByKind(available, total map[string]int64) (devices, cores occupancy) {
	for acceleratorType, installed := range total {
		sum := &devices
		if domain.AcceleratorType(acceleratorType).Kind() == domain.TypeKindCPU {
			sum = &cores
		}
		sum.total += installed
		sum.busy += installed - available[acceleratorType]
	}
	return devices, cores
}

// ClusterInfo is one row of GET /internal/clusters.
type ClusterInfo struct {
	ClusterName string `json:"cluster_name"`
	// ClusterID is the runtime-derived stable fingerprint (kube-system namespace UID /
	// machine-id), empty for a cluster whose agent has not reported it (older runtime build, or
	// no heartbeat within the freshness window).
	ClusterID  string    `json:"cluster_id"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Connected  bool      `json:"connected"`
	// AcceleratorBusy/AcceleratorTotal are the cluster's most recently reported occupancy,
	// summed across every accelerator type of kind accelerator — actual busy-vs-idle chip counts from the
	// last reconcile snapshot, not a budget-consumption ratio. Zero/absent when no live snapshot
	// is within the connectedWithin freshness window (e.g. a disconnected cluster).
	AcceleratorBusy  int64 `json:"accelerator_busy"`
	AcceleratorTotal int64 `json:"accelerator_total"`
	// CoreBusy/CoreTotal are the same for the cores the cluster offers to jobs on CPU. Kept
	// apart: 16 cores and 4 chips are not 20 of anything.
	CoreBusy  int64 `json:"core_busy"`
	CoreTotal int64 `json:"core_total"`
	// AutoscalerEnabled mirrors the cluster's own most recent clusterapi.ReconcileRequest — see
	// clusterapi.ReconcileRequest.AutoscalerEnabled. False (the fail-closed default) for a cluster whose agent
	// has never reported it or is not within the freshness window.
	AutoscalerEnabled bool `json:"autoscaler_enabled"`
}

// Clusters is every cluster that has ever polled, and whether its agent is connected now.
func (s *Service) Clusters(ctx context.Context) ([]ClusterInfo, error) {
	heartbeats, err := s.metrics.LiveClusterHeartbeats(ctx, s.connectedWithin)
	if err != nil {
		return nil, auth.Internal(s.logger, "list clusters", err)
	}
	available, total, err := s.metrics.LiveClusterAcceleratorAvailableAndTotal(ctx, s.connectedWithin)
	if err != nil {
		return nil, auth.Internal(s.logger, "list clusters", err)
	}
	clusterIDs, err := s.metrics.LiveClusterIDs(ctx, s.connectedWithin)
	if err != nil {
		return nil, auth.Internal(s.logger, "list clusters", err)
	}
	autoscalerEnabled, err := s.metrics.LiveClusterAutoscalerCapability(ctx, s.connectedWithin)
	if err != nil {
		return nil, auth.Internal(s.logger, "list clusters", err)
	}
	names := make([]string, 0, len(heartbeats))
	for name := range heartbeats {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ClusterInfo, len(names))
	now := time.Now()
	for i, name := range names {
		connected := heartbeats[name]
		var lastSeen time.Time
		if connected {
			lastSeen = now
		}
		devices, cores := occupancyByKind(available[name], total[name])
		out[i] = ClusterInfo{
			ClusterName:       name,
			ClusterID:         clusterIDs[name],
			LastSeenAt:        lastSeen,
			Connected:         connected,
			AcceleratorBusy:   devices.busy,
			AcceleratorTotal:  devices.total,
			CoreBusy:          cores.busy,
			CoreTotal:         cores.total,
			AutoscalerEnabled: autoscalerEnabled[name],
		}
	}
	return out, nil
}

// Detach is the cluster's own runtime ending it: its jobs go back to the queue, and Detach
// answers which.
func (s *Service) Detach(ctx context.Context, cluster string) ([]string, error) {
	requeued, err := s.store.RemoveCluster(ctx, cluster)
	switch {
	case errors.Is(err, db.ErrClusterNotFound):
		return nil, auth.NotFound("cluster")
	case err != nil:
		return nil, auth.Internal(s.logger, "detach cluster", err, zap.String("cluster", cluster))
	}
	s.logger.Info("cluster detached by its runtime", zap.String("cluster", cluster), zap.Int("requeued", len(requeued)))
	return append([]string{}, requeued...), nil
}
