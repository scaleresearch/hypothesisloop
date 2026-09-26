package registry

import (
	"context"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// Store is the persistence interface required by the registry service.
type Store interface {
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	ListExperiments(ctx context.Context, filter domain.ExperimentFilter) ([]*domain.Experiment, error)
	CountExperiments(ctx context.Context, filter domain.ExperimentFilter) (int, error)
	ListExperimentsTesting(ctx context.Context, hypothesisIDs []string) ([]*domain.Experiment, error)
	MarkUnderperforming(ctx context.Context, id, metricName string) error
	GetLineage(ctx context.Context, experimentID string) ([]*domain.Experiment, error)
	// GetAgent resolves an agent id, nil when no such agent ever signed up.
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// GetPlatformExperimentSubmitPolicies is a narrow read of a platform experiment's
	// HypothesisSubmitPolicy/JobSubmitPolicy, used to gate RegisterHypothesis without pulling
	// the full row through this interface. found=false means id does not exist.
	GetPlatformExperimentSubmitPolicies(ctx context.Context, platformExperimentID string) (hypothesisPolicy, jobPolicy domain.SubmitterPolicy, found bool, err error)
	// GetPlatformExperimentMetrics is a narrow read of a platform experiment's declared
	// MetricDefinitions, used by the underperformance check to find the ranking metric's
	// direction without pulling the full row through this interface.
	GetPlatformExperimentMetrics(ctx context.Context, platformExperimentID string) (metrics []domain.MetricDefinition, found bool, err error)
	ListFindingsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisFinding, error)
	CountFindingsByHypothesis(ctx context.Context, hypothesisID string) (int, error)
	NewestFindings(ctx context.Context, ids []string) (map[string]*domain.HypothesisFinding, error)
	FindingSummaries(ctx context.Context, experimentIDs []string) (map[string]string, error)
	CreateHypothesisComment(ctx context.Context, hypothesisID, platformExperimentID, agentID string, actor domain.Actor, text string) (*domain.HypothesisComment, error)
	// NameActors sets on each actor the name it carries now.
	NameActors(ctx context.Context, actors []*domain.Actor) error
	ListCommentsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error)
	CountCommentsByHypothesis(ctx context.Context, hypothesisID string) (int, error)
	CountHypothesisActivity(ctx context.Context, ids []string) (map[string]db.HypothesisActivity, error)
	Evidence(ctx context.Context, platformExperimentID string) (map[string]any, error)
}

// Graph is the hypothesis graph: every hypothesis and link, and the native read query over them.
// Implemented by graphdb.Store.
type Graph interface {
	Register(ctx context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, bool, error)
	AddLink(ctx context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec, stamp db.Stamp) (*graphdb.LinkResult, error)
	UpdateStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus, stamp db.Stamp) (*domain.Hypothesis, bool, error)
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	Links(ctx context.Context, id string) (domain.HypothesisLinks, error)
	ListHypotheses(ctx context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error)
	HypothesisStats(ctx context.Context, f graphdb.ListFilter) (*domain.HypothesisStats, error)
	ExperimentGraph(ctx context.Context, platformExperimentID string) ([]*domain.HypothesisListItem, []domain.GraphEdge, error)
	Query(ctx context.Context, platformExperimentID, cypher string, params map[string]any) (*graphdb.QueryResult, error)
}

// Service manages experiments and their metric timeseries.
type Service struct {
	store        Store
	graph        Graph
	logger       *zap.Logger
	metricsDBURL string
	// dataStore is the only way this service learns what a job wrote — listed live, never
	// mirrored into PostgreSQL.
	dataStore *objectstore.Client
	// events, when wired, carries to /watch subscribers the events no Postgres trigger emits: the
	// metric-arrival pointer and every hypothesis event (the graph store has no NOTIFY).
	events EventNotifier
}

// EventNotifier emits the events no Postgres trigger emits: the metric pointer, and the hypothesis
// events, emitted around the graph write they describe (see db.EventsStore.Emit).
type EventNotifier interface {
	NotifyEvent(ctx context.Context, e db.Event) error
	Emit(ctx context.Context, write func(stamp db.Stamp) ([]db.Event, error)) error
}

// WithEvents wires the change stream this service announces metric arrivals and hypothesis
// changes on.
func (s *Service) WithEvents(events EventNotifier) *Service {
	s.events = events
	return s
}

// New returns a new registry Service.
func New(store Store, graph Graph, logger *zap.Logger, metricsDBURL string, dataStore *objectstore.Client) *Service {
	return &Service{
		store:        store,
		graph:        graph,
		logger:       logger,
		metricsDBURL: metricsDBURL,
		dataStore:    dataStore,
	}
}
