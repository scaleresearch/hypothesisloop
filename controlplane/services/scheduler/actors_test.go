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

func (*hostingStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	return namesByID(actors)
}

func (*admitStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	return namesByID(actors)
}

func (*concurrentAdmissionStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	return namesByID(actors)
}

func (poolOnlyStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	return namesByID(actors)
}
