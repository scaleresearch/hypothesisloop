package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// GetLogTail returns the last n lines of an experiment's most recently reported log tail
// (n <= 0 returns everything reported). Reported by the owning cluster-agent, not the job
// itself — see clusteragentapi and agentloop.reportChangedStatuses. Empty, not an error, if
// nothing has been reported yet.
func (s *Service) GetLogTail(ctx context.Context, experimentID string, n int) ([]string, error) {
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.GetLogTail: get experiment: %w", err)
	}
	if exp == nil {
		return nil, fmt.Errorf("%w: %s", ErrExperimentNotFound, experimentID)
	}
	lines, err := metricsdb.GetLatestLogTail(ctx, s.metricsDBURL, experimentID, n)
	if err != nil {
		return nil, fmt.Errorf("registry.GetLogTail: %w", err)
	}
	return lines, nil
}
