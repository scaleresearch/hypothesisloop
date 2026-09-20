package scheduler

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ClusterName returns the name of the registered cluster bound to clusterID, or "" when none is.
// Settings are keyed by the id a cluster reports and a cluster is owned by name, so whose the
// settings are is what the platform recorded when it bound the two, never what a runtime states
// on this request. The binding outlives the connection: settings are reachable while the
// cluster is away.
func (s *Service) ClusterName(ctx context.Context, clusterID string) (string, error) {
	cluster, err := s.store.ClusterByReportedID(ctx, clusterID)
	if err != nil || cluster == nil {
		return "", err
	}
	return cluster.Name, nil
}

// GetClusterSettings returns the operator overrides for clusterID, or nil if none were ever set.
func (s *Service) GetClusterSettings(ctx context.Context, clusterID string) (*domain.ClusterSettings, error) {
	return s.store.GetClusterSettings(ctx, clusterID)
}

// PutClusterSettings validates and upserts the operator overrides for one cluster.
func (s *Service) PutClusterSettings(ctx context.Context, cs *domain.ClusterSettings) error {
	if cs.ClusterID == "" {
		return fmt.Errorf("cluster_id is required")
	}
	if err := domain.ValidateClusterSettings(cs); err != nil {
		return err
	}
	return s.store.PutClusterSettings(ctx, cs)
}
