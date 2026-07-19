package registry

import (
	"context"

	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Store is the persistence interface required by the registry service.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	UpdateExperiment(ctx context.Context, exp *domain.Experiment) error
	MarkStarted(ctx context.Context, id string) (bool, error)
	GetLineage(ctx context.Context, experimentID string) ([]*domain.Experiment, error)
	// FindOrCreateHypothesis registers a hypothesis within a platform experiment, or returns
	// the existing row (and true) if one with equivalent normalized text already exists in
	// that same platform experiment — the real uniqueness check.
	FindOrCreateHypothesis(ctx context.Context, agentID, platformExperimentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error)
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	ListHypotheses(ctx context.Context, platformExperimentID string) ([]*domain.Hypothesis, error)
	ListFindingsByHypothesis(ctx context.Context, hypothesisID string) ([]*domain.HypothesisFinding, error)
}

// Service manages experiments and their metric timeseries.
type Service struct {
	store        Store
	logger       *zap.Logger
	metricsDBURL string
}

// New returns a new registry Service.
func New(store Store, logger *zap.Logger, metricsDBURL string) *Service {
	return &Service{
		store:        store,
		logger:       logger,
		metricsDBURL: metricsDBURL,
	}
}
