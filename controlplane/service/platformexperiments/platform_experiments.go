package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
	"go.uber.org/zap"
)

// platformExperimentReader reads one platform experiment.
type platformExperimentReader interface {
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
}

// Store is the persistence interface for platform experiments.
type Store interface {
	platformExperimentReader
	InstructionsStore
	// CreatePlatformExperiment stores pe with the agents it is created with signed up, or neither.
	CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error
	// AgentTypeCatalog and ProjectAgentTypeCatalog are the agent types the agents of a platform
	// experiment, or of one of a project, may run as.
	AgentTypeCatalog(ctx context.Context, platformExpID string) (domain.AgentTypeCatalog, error)
	ProjectAgentTypeCatalog(ctx context.Context, projectID string) (domain.AgentTypeCatalog, error)
	// PriorRuns is the ids of the prior runs of platform experiment id (domain.Tenancy.PriorRuns).
	PriorRuns(ctx context.Context, id string) ([]string, error)
	// RunsToRank reads platform experiments ids as their standings are read, in their order.
	RunsToRank(ctx context.Context, ids []string) ([]db.RunToRank, error)
	ListPlatformExperiments(ctx context.Context, filter db.PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error)
	// ListPlatformExperimentsByStatus is the unpaginated read the sweeps use. A sweep that
	// borrowed the paged list above silently stopped at its default page — see the store.
	ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error)
	CountPlatformExperiments(ctx context.Context, filter db.PlatformExperimentsFilter) (int, error)
	// ClosePlatformExperiment closes it and records why: by is whose credential asked, nil for
	// the platform at the end time.
	ClosePlatformExperiment(ctx context.Context, id string, by *domain.Actor, message string) error
	// RemovePlatformExperiment removes it for by, closing one not closed yet in the same write:
	// db.ErrPlatformExperimentNotFound.
	RemovePlatformExperiment(ctx context.Context, id string, by domain.Actor) error
	// ReopenPlatformExperiment sets a closed run that started running again until endsAt, its
	// sandboxed coordinator on coordinatorModel unless it is empty, and takes back what the close
	// wrote: db.ErrNotReopenable, db.ErrPlatformExperimentNotFound.
	ReopenPlatformExperiment(ctx context.Context, id string, endsAt time.Time, durationHours float64, coordinatorModel string) error
	// UpdatePlatformExperiment writes pe only if it is still in expectedStatus — see the db
	// implementation's comment for why the compare-and-swap matters here — and researchers when
	// not nil, with max_agents and them held to the researchers at work (db.PoolBelowAtWorkError).
	UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus, researchers domain.ResearcherPool) error
	// ResearchersAtWork counts the researchers at work of a platform experiment by agent type and model.
	ResearchersAtWork(ctx context.Context, platformExpID string) (map[domain.PoolKey]int, error)
	// ResearcherPoolState is what keeping the researchers of a running platform experiment is
	// decided from: its pool and max_agents, who is at work and whose place is free for good.
	ResearcherPoolState(ctx context.Context, platformExpID string) (db.PoolState, error)
	// HoldResearchers records what a running platform experiment says of the places of its
	// researchers the platform keeps empty, "" for none.
	HoldResearchers(ctx context.Context, id, line string) error
	// RaiseBaseline writes the running pe as patched with its baseline raised and records the
	// raise, or does neither.
	RaiseBaseline(ctx context.Context, pe *domain.PlatformExperiment, raise domain.BaselineRaise) error
	SetPlatformExperimentSummary(ctx context.Context, id, summary string) error
	// RecordClose writes the platform's record of the close beside the summary, once.
	RecordClose(ctx context.Context, id, record string) error
	// SetCloseAgents keeps how each agent's run stands just before the close, for its record, and
	// CloseAgents reads it back.
	SetCloseAgents(ctx context.Context, id, lines string) error
	CloseAgents(ctx context.Context, id string) (string, error)
	// SummaryWrittenAt is when the summary was last written, nil while it never was.
	SummaryWrittenAt(ctx context.Context, id string) (*time.Time, error)
	// HypothesesInFlight is each hypothesis with jobs queued or running or claimed by a checkout,
	// and who is on it.
	HypothesesInFlight(ctx context.Context, id string) ([]domain.HypothesisInFlight, error)
	// MessageAllowance is how many more messages each agent may be sent now.
	MessageAllowance(ctx context.Context, platformExpID string, sender domain.Actor) (map[string]domain.MessageAllowance, error)
	// JobsSettled says every job of a platform experiment ended and its cost was written.
	JobsSettled(ctx context.Context, id string) (bool, error)
	// ClosesToRecord is the platform experiments closed in the last within, after they were open,
	// with no record of the close and every job settled.
	ClosesToRecord(ctx context.Context, within time.Duration) ([]string, error)
	// TellCoordinator records the status_reason the experiment waits for its coordinator with,
	// "" for none, and announces one that changed.
	TellCoordinator(ctx context.Context, id, reason string) error
	// HoldBaselineClock moves baseline_due_at of an open platform experiment with no baseline out
	// by the time its baseline waits on the platform, at most most in all: held says it waits now.
	HoldBaselineClock(ctx context.Context, id string, held bool, most time.Duration) error
	// MarkBaselineLate tells the owner of a job not running yet, once, as job.held, why it is late.
	MarkBaselineLate(ctx context.Context, id, reason string) error
	// Signup inserts only while the experiment is still open; inserted=false means it is closed
	// or the agent was already signed up. A researcher past max_agents is db.ErrMaxAgentsReached,
	// decided inside the insert's own transaction.
	Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) (bool, error)
	// SetProposal stores a draft's proposal for goal goalVersion with what p says of it: until
	// when it waits, when the platform accepts it by itself, how long its planner's machine is
	// kept for a change of the goal and the baseline agent it signs up; db.ErrNotDraft for
	// anything but a draft, db.ErrGoalChanged once it has another goal.
	SetProposal(ctx context.Context, id string, goalVersion int, proposal []byte, p db.Proposed) error
	// HoldAccept stops the platform accepting a proposed draft by itself: db.ErrNotDraft,
	// db.ErrNotProposed.
	HoldAccept(ctx context.Context, id string) error
	// DraftsToAccept is the proposed drafts whose accepts_at has passed.
	DraftsToAccept(ctx context.Context) ([]string, error)
	// AcceptDraft makes a proposed draft the open platform experiment pe describes, with its
	// researchers signed up, or does neither: db.ErrNotDraft, db.ErrNothingProposed.
	AcceptDraft(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error
	// SetGoal changes the goal of a platform experiment and tells whoever acts on it, giving a
	// draft back to its planner until planUntil: db.ErrNotEditable.
	SetGoal(ctx context.Context, id, goal string, planUntil time.Time) error
	// NameActors sets on each actor the name it carries now.
	NameActors(ctx context.Context, actors []*domain.Actor) error
	// JobHasFinding says a finding was filed for the job.
	JobHasFinding(ctx context.Context, experimentID string) (bool, error)
	// Decline stores why a draft's planner proposes nothing for goal goalVersion, until when the
	// draft waits for a person and how long the planner's machine is kept; the errors of
	// SetProposal.
	Decline(ctx context.Context, id string, goalVersion int, reason string, waitUntil time.Time, keepPlanner time.Duration) error
	// CreateDraft creates a draft with its planner signed up, or neither.
	CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup) error
	// RepositoriesToCreate, GiveRepository and FailRepository are the repository pass's: the
	// platform experiments whose repository is to be created, and what became of each.
	RepositoriesToCreate(ctx context.Context) ([]db.RepositoryToCreate, error)
	GiveRepository(ctx context.Context, id, repository, url string) error
	FailRepository(ctx context.Context, id, answer string) error
	// SpawnAgent creates an agent in the experiment's org and signs it up, or does neither.
	SpawnAgent(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup) error
	// JoinRunning gives a researcher a place in a running platform experiment and its quota,
	// creating agent first when it is not nil, or does neither.
	JoinRunning(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup, late db.LateJoin) error
	RemoveAgent(ctx context.Context, platformExpID, agentID string, deleteDataKey func(ctx context.Context, accessKeyID string) error) error
	// FinishAgent records that an agent has nothing left to do: domain.ErrNotSignedUp,
	// db.ErrSignupClosed where the agent has no work to finish, db.ErrWorkUnfinished.
	FinishAgent(ctx context.Context, platformExpID, agentID, reason string) error
	// SendAgentMessages stores a message to each agent named, or with none to every researcher at
	// work, or stores none (see the db implementation for its refusals).
	SendAgentMessages(ctx context.Context, platformExpID string, to []string, text string, sender domain.Actor) ([]domain.AgentMessage, []db.MessagesRateLimited, error)
	// SendCoordinatorMessage stores a message of a person or a key to the sandboxed coordinator, or
	// none (see the db implementation for its refusals).
	SendCoordinatorMessage(ctx context.Context, platformExpID, text string, sender domain.Actor) (*domain.AgentMessage, error)
	// ListAgentMessages is the messages sent in a platform experiment, to agentID alone unless it
	// is empty, oldest first.
	ListAgentMessages(ctx context.Context, platformExpID, agentID string) ([]domain.AgentMessage, error)
	ListAgentSignups(ctx context.Context, platformExpID string) ([]domain.AgentSignup, error)
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// StartPlatformExperimentTx flips open->running and writes every agent quota atomically.
	StartPlatformExperimentTx(ctx context.Context, id string, agentsDue time.Duration, quotasFor func(participants []db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error)
	ListSignups(ctx context.Context, platformExpID string) ([]string, error)
	// ListSignupsByRole is the ranking-side read: standings are about competitors, never about
	// the whole roster.
	ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error)
	// ListParticipants is the signups still taking part: all but an accepted draft's planner.
	ListParticipants(ctx context.Context, platformExpID string) ([]string, error)
	// FinishedBaselineJobs is the COMPLETED jobs on the objective that the baseline agents of a
	// platform experiment that finished left, oldest first.
	FinishedBaselineJobs(ctx context.Context, platformExpID string) ([]*domain.Experiment, error)
	// CompletedJobs is every COMPLETED job of a platform experiment, oldest first.
	CompletedJobs(ctx context.Context, platformExpID string) ([]*domain.Experiment, error)
	// ListExperimentsTesting is every job that tests one of the hypotheses, oldest first: of a
	// platform experiment's own id, the jobs that measure its objective.
	ListExperimentsTesting(ctx context.Context, hypothesisIDs []string) ([]*domain.Experiment, error)
	// CompletedJobIDs is the jobs that ran to a successful end: of them, the ones that hold every
	// constraint rank.
	CompletedJobIDs(ctx context.Context, platformExpID string) (map[string]bool, error)
	// AgentJobsByAgent is, for every agent that submitted a job, how many it submitted.
	AgentJobsByAgent(ctx context.Context, platformExpID string) (map[string]db.AgentJobs, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// GetAgentQuota/ListAgentQuotas cover allocation only (guaranteed/burst
	// capacity settings) — consumption (used_*) is never stored here; Service
	// merges it in from the metrics DB on every read via metricsdb.PopulateUsage(One).
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error)
	AddDesiredQuotaUsage(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error
	AddDesiredQuotaUsageOne(ctx context.Context, quota *domain.AgentQuota) error
	// GetAgentRunningExperiments backs the correction in running_cost.go: AddDesiredQuotaUsage
	// only knows each RUNNING job's static admission-time estimate, which goes stale the moment
	// a job runs longer (or shorter) than that estimate — see running_cost.go for why that matters.
	GetAgentRunningExperiments(ctx context.Context, agentID, platformExpID string) ([]*domain.Experiment, error)
	// GetExperiment resolves a winning sample's job_id to the row carrying its code_ref — see
	// rank, where a standing without it is a number nobody can reproduce.
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observed func(context.Context) (*domain.AgentQuota, error), rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (db.AdmitDecision, error)
	ReserveAdmittedAcceleratorTypeTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, acceleratorCount, defaultMaxConcurrent int, observed func(ctx context.Context, agentID, platformExpID string, platformExpStart time.Time) (*domain.AgentQuota, error)) (rejectionReason string, err error)
	RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error
	ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error)
	ListStageAdvances(ctx context.Context, platformExpID string) ([]domain.StageAdvance, error)
	AgentDataKey(ctx context.Context, platformExperimentID, agentID string, create func(context.Context) (domain.DataKey, error)) (domain.DataKey, error)
}

// Service manages the Platform Experiment lifecycle.
type Service struct {
	store   Store
	metrics *metricsdb.Store
	cfg     domain.QuotaConfig
	logger  *zap.Logger
	// observedGapCap is the deployment-wide observation cadence, identical to the
	// controller's and the settler's. Every observed-usage query in a deployment must agree on
	// what "how long did this run" means, or the same job's cost changes depending on which code
	// path is asked — visibly jumping the moment it settles.
	observedGapCap time.Duration
	dataStore      *objectstore.Client
	// maxDataBytesPerAgent is the durable-data ceiling admission refuses a submit on, 0 for none.
	maxDataBytesPerAgent int64
	// baseline reads what a declared baseline job did; readBaseline unless WithBaselineRead sets another.
	baseline baselineReader
	// reachable reads the accelerator types jobs of a project's org can be placed on now, each
	// with the clusters that have it. WithLiveCapacity or WithReachable sets it, and until then it
	// answers ErrLiveCapacityUnconfigured.
	reachable func(ctx context.Context, projectID string) (map[string][]domain.Cluster, error)
	// repos is the git host the repository pass creates a draft's repository on, and
	// repositoryWake wakes the pass for a draft just created.
	repos          githost.Provider
	repositoryWake chan struct{}
	// environments reads what the baseline branch declares and the environment a job ran in.
	environments Environments
	// models is the platform's model for an agent and a coordinator whose agent type names none
	// (WithModelDefaults).
	models domain.ModelDefaults
	// idle tells researchers with no job for idleAfter that they are idle, and whoever steers a
	// run that its capacity has held places free for idleAfter (WithIdleNotice).
	idle      IdleNotices
	idleAfter time.Duration
	// personIdeas counts the open ideas of people (WithPersonIdeas).
	personIdeas PersonIdeas
	// placement reads the live capacity jobs of a project's org can be placed on, node by node.
	// WithLiveCapacity sets it, and until then it answers ErrLiveCapacityUnconfigured.
	placement func(ctx context.Context, projectID string) (*Placement, error)
	// jobImages is the images the deployment offers a job to run in (WithJobImages).
	jobImages []domain.JobImage
	// capacityFreshness, clusters and podResources are what the live capacity is
	// read from (WithLiveCapacity).
	capacityFreshness time.Duration
	clusters          ClusterRegistry
	podResources      domain.PodResources
	// launch is what a sandboxed agent is launched with (WithAgentLaunch).
	launch AgentLaunch
	// planning is what bounds a draft (WithPlanning).
	planning Planning
	// proxyURL is where the control plane's proxy serves repositories (WithGitProxy).
	proxyURL string
}

// IdleNotices reads the researchers of running platform experiments due an idle notice, and
// tells each (db.Store.IdleResearchers, NoticeIdle). It records whether a
// run's capacity holds places free and tells whoever steers it once that lasted
// (MarkCapacityFree, NoticeCapacityIdle).
type IdleNotices interface {
	IdleResearchers(ctx context.Context, after time.Duration) ([]db.IdleResearcher, error)
	NoticeIdle(ctx context.Context, r db.IdleResearcher, detail string) error
	MarkCapacityFree(ctx context.Context, id string, free bool, after time.Duration) (*db.IdleCapacity, error)
	NoticeCapacityIdle(ctx context.Context, r db.IdleCapacity, detail string) error
}

// WithIdleNotice wires the idle notices the expiry sweep sends (agent.idle, capacity.idle).
func (s *Service) WithIdleNotice(notices IdleNotices, after time.Duration) *Service {
	s.idle, s.idleAfter = notices, after
	return s
}

// Environments is what the baseline gate asks of the environments (environments.Service).
type Environments interface {
	Read(ctx context.Context, projectID, repository, ref string) (*string, error)
	Get(ctx context.Context, key string) (*domain.Environment, error)
}

// WithPersonIdeas wires the graph the situation counts the open ideas of people in.
func (s *Service) WithPersonIdeas(ideas PersonIdeas) *Service {
	s.personIdeas = ideas
	return s
}

// WithModelDefaults sets the platform's model for an agent and a coordinator whose agent type names
// none.
func (s *Service) WithModelDefaults(models domain.ModelDefaults) *Service {
	s.models = models
	return s
}

// WithEnvironments wires the environments.
func (s *Service) WithEnvironments(environments Environments) *Service {
	s.environments = environments
	return s
}

// WithRepositories wires the git host the repository pass creates drafts' repositories on.
func (s *Service) WithRepositories(repos githost.Provider) *Service {
	s.repos = repos
	return s
}

// WithReachable reads the accelerator types a project's org can place jobs on from reachable in
// place of the live capacity, for a caller that has no metrics DB to read it from.
func (s *Service) WithReachable(reachable func(ctx context.Context, projectID string) (map[string][]domain.Cluster, error)) *Service {
	s.reachable = reachable
	return s
}

// WithBaselineRead reads what a declared baseline job did from read in place of the metrics DB
// and the graph.
func (s *Service) WithBaselineRead(read func(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error)) *Service {
	s.baseline = read
	return s
}

// WithDataStore wires the store a removed agent's key is deleted from, and the durable-data
// ceiling an agent's quota is answered with.
func (s *Service) WithDataStore(client *objectstore.Client, maxBytesPerAgent int64) *Service {
	s.dataStore = client
	s.maxDataBytesPerAgent = maxBytesPerAgent
	return s
}

// NewService constructs the service. metrics is the GreptimeDB instance
// backing observed agent quota consumption. PostgreSQL holds allocations and current desired
// experiment estimates.
func NewService(store Store, cfg domain.QuotaConfig, logger *zap.Logger, metrics *metricsdb.Store, observedGapCap time.Duration) *Service {
	if observedGapCap <= 0 {
		panic("quota: NewService requires a positive observation cadence — it must match the controller's and the settler's")
	}
	s := &Service{
		store:          store,
		metrics:        metrics,
		cfg:            cfg,
		logger:         logger,
		observedGapCap: observedGapCap,
		repositoryWake: make(chan struct{}, 1),
	}
	s.baseline = s.readBaseline
	s.reachable = func(context.Context, string) (map[string][]domain.Cluster, error) {
		return nil, ErrLiveCapacityUnconfigured
	}
	s.placement = func(context.Context, string) (*Placement, error) {
		return nil, ErrLiveCapacityUnconfigured
	}
	return s
}

// CreatePlatformExperimentRequest is the body of POST /platform-experiments, and what a
// platform experiment is created as by any way: the input of validate. It is the experiment's
// spec and where the experiment is placed.
type CreatePlatformExperimentRequest struct {
	experimentspec.Spec
	// ProjectID places the platform experiment. Required on create, and fixed from then on.
	ProjectID string `json:"project_id,omitempty"`
	// Repository is the code the platform experiment works on, owner/name on GitHub, reachable
	// through an installation its org connected. Fixed at creation.
	Repository string `json:"repository,omitempty" doc:"owner/name on GitHub. An installation the org connected must cover it. Left out, the platform creates a private repository on the GitHub account the org connected, and the answer names it."`
	// RepositoryAccount is the connected GitHub account a new repository is created on.
	RepositoryAccount string    `json:"repository_account,omitempty" doc:"The GitHub account, one the org connected, on which the platform creates the repository when repository is left out. Required then only when the org connected several."`
	Continues         string    `json:"continues,omitempty" doc:"The id of a closed platform experiment of the same project to carry on, which keeps its code in the repository this one names. Its baseline starts from the commit of that run's baseline job, so both are measured by the same harness. It keeps the metric keys and directions of that run: 422 invalid_metrics for others. Left out, it starts fresh. 422 invalid_continues for one that is no closed platform experiment of the project, keeps its code in another repository or declared no baseline."`
	StartsAt          time.Time `json:"starts_at" required:"false" doc:"When its baseline begins to be measured, for a later start than this create. Left out, or past, it is this create."`

	// coordinator is CoordinatorInAgentSandbox resolved by CreateExperiment: which model.
	coordinator *domain.AgentSandboxCoordinator
	// agents and signups are the agents created with it (sandboxedAgents), none without a sandboxed
	// coordinator.
	agents  []*domain.Agent
	signups []domain.AgentSignup
	// maxAgentSandboxes is how many machines the platform may run for the experiment, from config.
	maxAgentSandboxes int
	// createdBy is whose credential the request came with.
	createdBy *domain.Actor
	// repository is where it keeps its code, as CreateExperiment resolved the request's repository
	// and repository_account (chooseRepository).
	repository RepositoryChoice
}

// PlatformExperimentPatch is the body of PATCH /platform-experiments/{id}: the fields that
// change, and no others. A field left out keeps its value. The project, the repository, the agent image, the stages and the default
// quota tier are fixed at creation and are no fields of it.
type PlatformExperimentPatch struct {
	Goal                      *string                    `json:"goal,omitempty" doc:"What the owner asks, changed alone: a body that carries another field beside it is 422 goal_alone. A draft is planned again from it, and the coordinator of an open or running one is told to bring the brief in line. Not empty and at most 2000 bytes (422 invalid_goal), and not the goal it has (422 nothing_to_change)."`
	Name                      *string                    `json:"name,omitempty" doc:"Not empty."`
	Description               *string                    `json:"description,omitempty" doc:"The brief every agent works from, not empty and at most 32768 bytes."`
	BudgetAcceleratorHours    *float64                   `json:"budget_accelerator_hours,omitempty"`
	MaxAgents                 *int                       `json:"max_agents,omitempty" doc:"How many researchers may be at work at once, counted as a join counts them: the agents of role competitor that have not finished, were not cut, were not replaced and still have a machine. The coordinator, a draft's planner, agents of role baseline and reviewers never count. Lowered below the researchers at work of a running one, it cuts the newest of them: each winds down as a cut researcher does, told by agent.cut with detail max_agents_lowered, and nobody takes its place. Before the run starts, 409 max_agents_below_researchers says how many are signed up. Researchers the patch does not state are lowered to it, from the last entry. While it runs, a raised one is held to the hours its researchers left unspent: one trial of the baseline for each."`
	Researchers               *domain.ResearcherPool     `json:"researchers,omitempty" doc:"The researchers it is to have at work, the whole list as on create, at most max_agents in all. Each entry at least the researchers of its agent type and model at work: 409 researchers_below_at_work otherwise. Where the platform runs the researchers it adds the ones a raised count wants."`
	MaxConcurrentAccelerators *int                       `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   *[]domain.MetricDefinition `json:"metrics,omitempty"`
	ReportIntervalSeconds     *int                       `json:"report_interval_seconds,omitempty"`
	StartsAt                  *time.Time                 `json:"starts_at,omitempty" doc:"When its baseline begins to be measured, now when past. Stated, baseline_due_at moves with it."`
	DurationHours             *float64                   `json:"duration_hours,omitempty" doc:"How long the run lasts from the moment it starts."`
	BaselineJobID             *string                    `json:"baseline_job_id,omitempty" doc:"While the platform experiment is open, declares the baseline: the completed job of an agent of role baseline that measured it, with its finding filed. While it runs, raises the baseline to a stronger completed job of the experiment that ran the baseline's code_ref unchanged in a better configuration, with baseline_reason."`
	BaselineReason            *string                    `json:"baseline_reason,omitempty" doc:"Why the baseline of a running platform experiment is raised to baseline_job_id: the configuration of the control that job measured. Required with baseline_job_id while it runs, and taken then only."`

	// by is whose credential sent the patch, recorded with a raise of the baseline.
	by *domain.Actor
}

// carried names the fields the patch carries, without those in except.
func (p PlatformExperimentPatch) carried(except ...string) []string {
	var out []string
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"goal", p.Goal != nil},
		{"name", p.Name != nil},
		{"description", p.Description != nil},
		{"budget_accelerator_hours", p.BudgetAcceleratorHours != nil},
		{"max_agents", p.MaxAgents != nil},
		{"researchers", p.Researchers != nil},
		{"max_concurrent_accelerators", p.MaxConcurrentAccelerators != nil},
		{"metrics", p.Metrics != nil},
		{"report_interval_seconds", p.ReportIntervalSeconds != nil},
		{"starts_at", p.StartsAt != nil},
		{"duration_hours", p.DurationHours != nil},
		{"baseline_job_id", p.BaselineJobID != nil},
		{"baseline_reason", p.BaselineReason != nil},
	} {
		if field.set && !slices.Contains(except, field.name) {
			out = append(out, field.name)
		}
	}
	return out
}

// AgentResult is (agentID, finalMetric) used when closing an experiment.
type AgentResult struct {
	AgentID     string  `json:"agent_id"`
	FinalMetric float64 `json:"final_metric"`
}

// stageProgress is the read-only value served by GET /platform-experiments/{id}/stages. It omits
// the in-flight cost of running jobs that the controller's authoritative value includes, so it
// can trail the boundary the controller acts on.
func (s *Service) stageProgress(ctx context.Context, pe *domain.PlatformExperiment) (float64, error) {
	consumed, err := s.metrics.TotalObservedAccH(ctx, pe.CreatedAt, pe.ID)
	if err != nil {
		return 0, fmt.Errorf("stages: observed usage for %s: %w", pe.ID, err)
	}
	return domain.StageProgress(consumed, pe.BudgetAcceleratorHours, pe.StartedAt, pe.EndsAt, time.Now().UTC()), nil
}

// MaxListLimit is the largest page List answers.
const MaxListLimit = db.MaxPlatformExperimentListLimit

// ValidListSort says whether List sorts by sort.
func ValidListSort(sort string) bool {
	return domain.ValidSortField(sort, db.PlatformExperimentSortFields)
}

// ListQuery is what List reads: the platform experiments in Scope, and the shared ones with
// WithShared, of Status and holding Search, one page sorted by Sort. Shared, when set, keeps
// only the shared ones or only the others.
type ListQuery struct {
	Scope      domain.TenantScope
	WithShared bool
	Status     string
	Search     string
	Limit      int
	Offset     int
	Sort       string
	Shared     *bool
}

// List is one page of the platform experiments q reads, and how many it matches in all.
func (s *Service) List(ctx context.Context, q ListQuery) ([]*domain.PlatformExperiment, int, error) {
	filter := db.PlatformExperimentsFilter{
		Scope:      q.Scope,
		WithShared: q.WithShared,
		Status:     q.Status,
		Search:     q.Search,
		Limit:      q.Limit,
		Offset:     q.Offset,
		Sort:       q.Sort,
		Shared:     q.Shared,
	}
	pes, err := s.store.ListPlatformExperiments(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(s.logger, "list platform experiments", err)
	}
	total, err := s.store.CountPlatformExperiments(ctx, filter)
	if err != nil {
		return nil, 0, auth.Internal(s.logger, "list platform experiments", err)
	}
	if pes == nil {
		pes = []*domain.PlatformExperiment{}
	}
	return pes, total, nil
}

// Get reads the platform experiment id with its signed up agents, the prior runs of it reaches
// says its reader reaches, its spend and the names of who acted on it.
func (s *Service) Get(ctx context.Context, id string, reaches func(id string) bool) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	if pe == nil || pe.RemovedAt != nil {
		return nil, ErrPlatformExperimentNotFound
	}
	signups, err := s.store.ListSignups(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	pe.SignedUpAgents = signups
	pe.SignupCount = len(signups)
	prior, err := s.store.PriorRuns(ctx, id)
	if err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	reached := prior[:0]
	for _, id := range prior {
		if reaches(id) {
			reached = append(reached, id)
		}
	}
	if pe.PriorRuns, err = s.PriorRuns(ctx, reached); err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	if err := s.Spend(ctx, pe); err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	if err := s.nameActors(ctx, pe); err != nil {
		return nil, auth.Internal(s.logger, "get platform experiment", err, zap.String("id", id))
	}
	return pe, nil
}

// ErrNotSignedUp is an agent read on a platform experiment it is not signed up to.
var ErrNotSignedUp = errors.New("agent is not signed up to this platform experiment")

// AgentSignup reads the signup of agentID to the platform experiment id, with what reader may do
// to the agent when it writes the platform experiment.
func (s *Service) AgentSignup(ctx context.Context, id, agentID string, reader auth.Principal, writes bool) (*domain.AgentSignup, []domain.SignupAction, error) {
	signup, err := s.store.GetAgentSignup(ctx, id, agentID)
	if err != nil {
		return nil, nil, auth.Internal(s.logger, "get platform experiment signup", err, zap.String("id", id))
	}
	if signup == nil {
		return nil, nil, ErrNotSignedUp
	}
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return signup, SignupActions(pe, *signup, reader, writes), nil
}

// ResearchersAtWork counts the researchers at work of the platform experiment id by agent type
// and model.
func (s *Service) ResearchersAtWork(ctx context.Context, id string) (map[domain.PoolKey]int, error) {
	return s.store.ResearchersAtWork(ctx, id)
}

// GitURL is where the platform experiment id's repository is served by the control plane's proxy.
func (s *Service) GitURL(id string) (string, error) {
	if s.proxyURL == "" {
		return "", errors.New("no git proxy address to name its repository at")
	}
	return domain.GitProxyURL(s.proxyURL, id), nil
}
