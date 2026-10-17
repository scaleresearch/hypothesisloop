package platformexperiments

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// SharingStore is the persistence of the platform's decision to show a platform experiment to
// every account. *db.PlatformExperimentsStore satisfies it.
type SharingStore interface {
	SetPlatformExperimentShared(ctx context.Context, id string, shared bool) (found bool, err error)
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
}

// Sharing is how staff show a platform experiment to every account and take that back. What
// sharing lets a caller reach is auth.Access's.
type Sharing struct {
	store  SharingStore
	logger *zap.Logger
}

func NewSharing(store SharingStore, logger *zap.Logger) *Sharing {
	return &Sharing{store: store, logger: logger}
}

// SetShared shares the platform experiment id with every account, or takes that back, and answers
// it as it is stored then.
func (s *Sharing) SetShared(ctx context.Context, id string, shared bool) (*domain.PlatformExperiment, error) {
	missing := refusal.Refuse(http.StatusNotFound, "not_found", "platform experiment not found")
	if !domain.IdentifierShaped(id) {
		return nil, missing
	}
	found, err := s.store.SetPlatformExperimentShared(ctx, id, shared)
	if err != nil {
		return nil, s.failed(id, err)
	}
	if !found {
		return nil, missing
	}
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, s.failed(id, err)
	}
	if pe == nil {
		return nil, missing
	}
	return pe, nil
}

func (s *Sharing) failed(id string, err error) error {
	return auth.Internal(s.logger, "sharing a platform experiment failed", err, zap.String("platform_experiment_id", id))
}
