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
	// CompletedJobIDs is the jobs of a platform experiment that ran to a successful end.
	CompletedJobIDs(ctx context.Context, platformExpID string) (map[string]bool, error)
	// GetAgentSignup is the agent's place in the platform experiment, nil when it has none.
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	// GetAgent resolves an agent id, nil when no such agent ever signed up.
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// PlatformExperimentExists says whether a hypothesis has a platform experiment to be in.
	PlatformExperimentExists(ctx context.Context, platformExperimentID string) (bool, error)
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
	// DescribeJobs sets on each job its agent's name and whether its summary is due.
	DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error
	ListCommentsByHypothesis(ctx context.Context, hypothesisID string, limit, offset int) ([]*domain.HypothesisComment, error)
	CountCommentsByHypothesis(ctx context.Context, hypothesisID string) (int, error)
	DeleteHypothesisComments(ctx context.Context, hypothesisID string) error
	CountHypothesisActivity(ctx context.Context, ids []string) (map[string]db.HypothesisActivity, error)
	Evidence(ctx context.Context, platformExperimentID string) (map[string]any, error)
	Implementations(ctx context.Context, hypothesisID string) ([]domain.Implementation, error)
	LastCompletedCodeRef(ctx context.Context, hypothesisID string) (string, error)
	BaselineCodeRef(ctx context.Context, platformExpID string) (string, error)
}

// Graph is the hypothesis graph: every hypothesis and link, and the native read query over them.
// Implemented by graphdb.Store.
type Graph interface {
	Register(ctx context.Context, n graphdb.NewHypothesis) (*domain.Hypothesis, []domain.Link, bool, bool, error)
	AddLink(ctx context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec, stamp db.Stamp) (*graphdb.LinkResult, error)
	UpdateStatus(ctx context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus, stamp db.Stamp) (*domain.Hypothesis, bool, error)
	EditText(ctx context.Context, id, text string) (*domain.Hypothesis, error)
	Remove(ctx context.Context, id string) error
	GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error)
	Links(ctx context.Context, id string) (domain.HypothesisLinks, error)
	FirstParent(ctx context.Context, id string) (string, error)
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
	// events carries to /watch subscribers the events no Postgres trigger emits: the
	// metric-arrival pointer and every hypothesis event (the graph store has no NOTIFY).
	events EventNotifier
}

// EventNotifier emits the events no Postgres trigger emits: the metric pointer, and the hypothesis
// events, emitted around the graph write they describe (see db.EventsStore.Emit).
type EventNotifier interface {
	NotifyEvent(ctx context.Context, e db.Event) error
	Emit(ctx context.Context, write func(stamp db.Stamp) ([]db.Event, error)) error
}

// New returns a new registry Service, which announces metric arrivals and hypothesis changes on
// events.
func New(store Store, graph Graph, events EventNotifier, logger *zap.Logger, metricsDBURL string, dataStore *objectstore.Client) *Service {
	return &Service{
		store:        store,
		graph:        graph,
		events:       events,
		logger:       logger,
		metricsDBURL: metricsDBURL,
		dataStore:    dataStore,
	}
}
