package scheduler

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// namesByID is the naming of actors for a store a test stands in: each by its id.
func namesByID(actors []*domain.Actor) error {
	for _, a := range actors {
		a.Name = a.ID
	}
	return nil
}

func (poolOnlyStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	return namesByID(actors)
}

// describeNothing stands in for the store's read of a job's agent name and due summary.
func describeNothing(context.Context, []*domain.Experiment) error { return nil }

func (poolOnlyStore) DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error {
	return describeNothing(ctx, jobs)
}
