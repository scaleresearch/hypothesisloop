package scheduler

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

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
