package registry

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// ListData returns what a job actually wrote to its durable-data prefix, read live from the
// object store. There is no PostgreSQL copy to read instead: jobs may push metrics and nothing
// else, so a job could never report its own file list, and a stored list beside the real bytes
// would only drift from them.
//
// A job that wrote nothing lists as an empty slice. That is the answer, not an error — most jobs
// keep nothing, and turning the ordinary case into a 404 makes every caller special-case it.
func (s *Service) ListData(ctx context.Context, experimentID string) ([]objectstore.Object, error) {
	exp, err := s.store.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("registry.ListData: %w", err)
	}
	if exp == nil {
		return nil, fmt.Errorf("%w: %s", ErrExperimentNotFound, experimentID)
	}
	objects, err := s.dataStore.List(ctx, objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID))
	if err != nil {
		return nil, fmt.Errorf("registry.ListData: %w", err)
	}
	return objects, nil
}
