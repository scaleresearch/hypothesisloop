package registry

import (
	"context"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// Store is the persistence interface required by the registry service.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	CountExperiments(ctx context.Context, filter domain.ExperimentFilter) (int, error)
	MarkStarted(ctx context.Context, id string) (bool, error)
	GetLineage(ctx context.Context, experimentID string) ([]*domain.Experiment, error)
	// FindOrCreateHypothesis registers a hypothesis within a platform experiment, or returns
	// the existing row (and true) if one with equivalent normalized text already exists in
	// that same platform experiment — the real uniqueness check.
	FindOrCreateHypothesis(ctx context.Context, source domain.HypothesisSource, agentID, author, platformExperimentID, text string) (h *domain.Hypothesis, alreadyExisted bool, err error)
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	ListHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus, limit, offset int) ([]*db.HypothesisListItem, error)
	CountHypotheses(ctx context.Context, platformExperimentID, agentID string, status domain.HypothesisStatus) (int, error)
	UpdateHypothesisStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus) (*domain.Hypothesis, error)
	ListFindingsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, error)
	CountFindingsByHypothesis(ctx context.Context, hypothesisID string) (int, error)
	CreateHypothesisComment(ctx context.Context, hypothesisID string, source domain.HypothesisSource, agentID, author, text string) (*domain.HypothesisComment, error)
	ListCommentsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error)
	CountCommentsByHypothesis(ctx context.Context, hypothesisID string) (int, error)
}

// Service manages experiments and their metric timeseries.
type Service struct {
	store        Store
	logger       *zap.Logger
	metricsDBURL string
	// dataStore is the only way this service learns what a job wrote — listed live, never
	// mirrored into PostgreSQL.
	dataStore *objectstore.Client
}

// New returns a new registry Service.
func New(store Store, logger *zap.Logger, metricsDBURL string, dataStore *objectstore.Client) *Service {
	return &Service{
		store:        store,
		logger:       logger,
		metricsDBURL: metricsDBURL,
		dataStore:    dataStore,
	}
}
