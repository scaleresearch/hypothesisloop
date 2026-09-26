package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"go.uber.org/zap"
)

// PlatformExperimentsStore is the persistence interface for platform experiments.
type PlatformExperimentsStore interface {
	CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	ListPlatformExperiments(ctx context.Context, filter db.PlatformExperimentsFilter) ([]*domain.PlatformExperiment, error)
	// ListPlatformExperimentsByStatus is the unpaginated read the sweeps use. A sweep that
	// borrowed the paged list above silently stopped at its default page — see the store.
	ListPlatformExperimentsByStatus(ctx context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error)
	CountPlatformExperiments(ctx context.Context, filter db.PlatformExperimentsFilter) (int, error)
	// ClosePlatformExperiment closes it and records why: by is whose credential asked, nil for
	// the platform at the end time.
	ClosePlatformExperiment(ctx context.Context, id string, by *domain.Actor) error
	// UpdatePlatformExperiment writes pe only if it is still in expectedStatus — see the db
	// implementation's comment for why the compare-and-swap matters here.
	UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus) error
	SetPlatformExperimentSummary(ctx context.Context, id, summary string) error
	// Signup inserts only while the experiment is still open; inserted=false means it is closed
	// or the agent was already signed up. A competitor past max_agents is db.ErrMaxAgentsReached,
	// decided inside the insert's own transaction.
	Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) (bool, error)
	LauncherKnown(ctx context.Context, name string) (bool, error)
	// SetProposal stores a draft's proposal and until when it waits for a person; db.ErrNotDraft
	// for anything but a draft.
	SetProposal(ctx context.Context, id string, proposal []byte, waitUntil time.Time) error
	// AcceptDraft makes a proposed draft the open platform experiment pe describes, with its
	// researchers signed up, or does neither: db.ErrNotDraft, db.ErrNothingProposed.
	AcceptDraft(ctx context.Context, pe *domain.PlatformExperiment, create []*domain.Agent, signups []domain.AgentSignup) error
	// Revise records a request to change a proposed draft's proposal and gives the draft back
	// to its planner until planUntil: db.ErrNotDraft, db.ErrNothingProposed.
	Revise(ctx context.Context, id, message string, by domain.Actor, planUntil time.Time) error
	// NameActors sets on each actor the name it carries now.
	NameActors(ctx context.Context, actors []*domain.Actor) error
	// DescribeJobs sets on each job its agent's name and whether its summary is due.
	DescribeJobs(ctx context.Context, jobs []*domain.Experiment) error
	// JobHasFinding says a finding was filed for the job.
	JobHasFinding(ctx context.Context, experimentID string) (bool, error)
	// Decline stores why a draft's planner proposes nothing and until when the draft waits for
	// a person; db.ErrNotDraft for anything but a draft.
	Decline(ctx context.Context, id, reason string, waitUntil time.Time) error
	// CreateDraft creates a draft with its planner signed up, or neither.
	CreateDraft(ctx context.Context, pe *domain.PlatformExperiment, planner *domain.Agent, signup domain.AgentSignup, limits db.DraftLimits, prepare func(context.Context, *domain.PlatformExperiment) error) error
	// SpawnAgent creates an agent in the experiment's org and signs it up, or does neither.
	SpawnAgent(ctx context.Context, platformExpID string, agent *domain.Agent, signup domain.AgentSignup) error
	RemoveAgent(ctx context.Context, platformExpID, agentID string) error
	// FinishAgent records that an agent has nothing left to do: db.ErrNotSignedUp,
	// db.ErrSignupClosed when the experiment is not running, db.ErrWorkUnfinished.
	FinishAgent(ctx context.Context, platformExpID, agentID, reason string) error
	ListAgentSignups(ctx context.Context, platformExpID string) ([]domain.AgentSignup, error)
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	GetAgent(ctx context.Context, id string) (*domain.Agent, error)
	// StartPlatformExperimentTx flips open->running and writes every agent quota atomically.
	StartPlatformExperimentTx(ctx context.Context, id string, quotasFor func(participants []db.StartParticipant) ([]*domain.AgentQuota, error)) (bool, []*domain.AgentQuota, error)
	ListSignups(ctx context.Context, platformExpID string) ([]string, error)
	// ListSignupsByRole is the ranking-side read: standings are about competitors, never about
	// the whole roster.
	ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error)
	// PlannerID is the agent that planned the platform experiment as a draft, or "".
	PlannerID(ctx context.Context, platformExpID string) (string, error)
	// ListParticipants is the signups still taking part: all but an accepted draft's planner.
	ListParticipants(ctx context.Context, platformExpID string) ([]string, error)
	// CompletedJobIDs is the jobs whose metrics rank: the ones that ran to a successful end.
	CompletedJobIDs(ctx context.Context, platformExpID string) (map[string]bool, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	// GetAgentQuota/ListAgentQuotas cover allocation only (guaranteed/burst
	// capacity settings) — consumption (used_*) is never stored here; PlatformExperimentsService
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
	// standingsOnMetric, where a standing without it is a number nobody can reproduce.
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
	AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observed func(context.Context) (*domain.AgentQuota, error), rateLimit db.SubmissionRateLimit, createDataKey func(context.Context) error) (decision db.AdmitDecision, rejectionReason string, err error)
	ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, acceleratorCount, defaultMaxConcurrent int, observed func(ctx context.Context, agentID, platformExpID string, platformExpStart time.Time) (*domain.AgentQuota, error)) (rejectionReason string, err error)
	RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error
	ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error)
	ListStageAdvances(ctx context.Context, platformExpID string) ([]domain.StageAdvance, error)
}

// PlatformExperimentsService manages the Platform Experiment lifecycle.
type PlatformExperimentsService struct {
	store        PlatformExperimentsStore
	usage        *metricsdb.UsageTracker
	metricsDBURL string
	cfg          domain.QuotaConfig
	logger       *zap.Logger
	// observedGapCap is the deployment-wide observation cadence, identical to the
	// controller's and the settler's. Every observed-usage query in a deployment must agree on
	// what "how long did this run" means, or the same job's cost changes depending on which code
	// path is asked — visibly jumping the moment it settles.
	observedGapCap time.Duration
}

// NewPlatformExperimentsService constructs the service. metricsDBURL is the GreptimeDB instance
// backing observed agent quota consumption. PostgreSQL holds allocations and current desired
// experiment estimates.
func NewPlatformExperimentsService(store PlatformExperimentsStore, cfg domain.QuotaConfig, logger *zap.Logger, metricsDBURL string, observedGapCap time.Duration) *PlatformExperimentsService {
	if observedGapCap <= 0 {
		panic("quota: NewPlatformExperimentsService requires a positive observation cadence — it must match the controller's and the settler's")
	}
	return &PlatformExperimentsService{
		store:          store,
		usage:          metricsdb.NewUsageTracker(metricsDBURL),
		metricsDBURL:   metricsDBURL,
		cfg:            cfg,
		logger:         logger,
		observedGapCap: observedGapCap,
	}
}

// CreatePlatformExperimentRequest is the body of POST /platform-experiments, and the input of
// Create and Update.
//
// name, description and metrics are not required by the schema, because with from_repo the
// repository's experiment file may state them. A create without from_repo must still state each
// of them, which the handler checks against the keys the body carried.
type CreatePlatformExperimentRequest struct {
	// ProjectID places the platform experiment. Required on create, and fixed from then on.
	ProjectID string `json:"project_id,omitempty"`
	// FromRepo takes every field the request leaves out from .hypothesisloop/experiment.yaml at
	// the head of the default branch of the repository the request names.
	FromRepo               bool    `json:"from_repo,omitempty"`
	Name                   string  `json:"name" required:"false"`
	Description            string  `json:"description" required:"false"`
	BudgetAcceleratorHours float64 `json:"budget_accelerator_hours"`
	MaxAgents              int     `json:"max_agents,omitempty" doc:"How many competitors may join. Left out, it is max_hosted: as many as the platform runs machines for one platform experiment."`
	// MaxConcurrentAccelerators bounds accelerators-in-flight for this experiment. Nil means
	// "leave unset" on create (defers to quota.default_max_concurrent_accelerators) and "no
	// change" on update; a present zero is rejected by domain validation, not treated as unset.
	MaxConcurrentAccelerators *int                      `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   []domain.MetricDefinition `json:"metrics" required:"false"` // metric keys jobs must emit
	ReportIntervalSeconds     int                       `json:"report_interval_seconds,omitempty" doc:"How often a job is expected to report. Left out, it is the platform's scheduler.default_report_interval_seconds."`
	StartsAt                  time.Time                 `json:"starts_at" required:"false" doc:"When the platform starts it on its own. Left out, it starts when POST /platform-experiments/{id}/start is called and at no time of its own."`
	EndsAt                    time.Time                 `json:"ends_at" required:"false" doc:"When it closes. State this or duration_hours, one of the two: both, or neither, answers 422."`
	DurationHours             float64                   `json:"duration_hours,omitempty" doc:"How long it lasts, in place of ends_at: it closes this many hours after starts_at, or after this create when starts_at is left out."`
	// Stages is the elimination ladder, fixed at creation. Omit to get the platform default
	// (config stages.default).
	Stages []domain.Stage `json:"stages,omitempty"`
	// DefaultQuotaTier controls signups that omit quota_tier. Empty defaults to "guaranteed" on
	// create; accepted values are "guaranteed" and "burst_only".
	DefaultQuotaTier string `json:"default_quota_tier,omitempty"`
	// CoordinatorHosted has the platform run this platform experiment's coordinator, on
	// CoordinatorModel or the platform's default model. It rents a machine, so neither is read
	// from the experiment file.
	CoordinatorHosted bool   `json:"coordinator_hosted,omitempty"`
	CoordinatorModel  string `json:"coordinator_model,omitempty"`
	// Repository is the code the platform experiment works on, owner/name on GitHub, reachable
	// through an installation its org connected. AgentImage is the image its hosted agents and
	// coordinator run, empty for the platform's own. Both are fixed at creation, and the image
	// is not read from the experiment file.
	Repository string `json:"repository,omitempty" doc:"owner/name on GitHub. An installation the org connected must cover it. Left out, the platform creates a private repository on the GitHub account the org connected, and the answer names it."`
	// RepositoryAccount is the connected GitHub account a new repository is created on.
	RepositoryAccount string `json:"repository_account,omitempty" doc:"The GitHub account, one the org connected, on which the platform creates the repository when repository is left out. Required then only when the org connected several."`
	AgentImage        string `json:"agent_image,omitempty" doc:"The image hosted agents and the coordinator run, built FROM the platform's agent image so the harness is in it. Empty is the platform's own."`

	// coordinator is CoordinatorHosted resolved by the handler: which launcher and which model.
	coordinator *domain.HostedCoordinator
	// maxHosted is how many machines the platform may run for the experiment, from config.
	maxHosted int
	// createdBy is whose credential the request came with.
	createdBy *domain.Actor
	// prepare runs on the platform experiment once the request is found valid and before it is
	// stored.
	prepare func(context.Context, *domain.PlatformExperiment) error

	// raw is the body as it arrived. Which keys it carried is what tells a field left out from
	// one stated as its zero value.
	raw json.RawMessage
}

// createFields is the request without its own decoding.
type createFields CreatePlatformExperimentRequest

func (r *CreatePlatformExperimentRequest) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, (*createFields)(r)); err != nil {
		return err
	}
	r.raw = append(json.RawMessage(nil), data...)
	return nil
}

// PlatformExperimentPatch is the body of PATCH /platform-experiments/{id}: the fields that
// change, and no others. A field left out keeps its value, so a description is cleared only by
// sending it empty. The project, the repository, the agent image, the stages and the default
// quota tier are fixed at creation and are no fields of it.
type PlatformExperimentPatch struct {
	Name                      *string                    `json:"name,omitempty" doc:"Not empty."`
	Description               *string                    `json:"description,omitempty" doc:"The brief every agent works from. The one field a coordinator's token changes."`
	BudgetAcceleratorHours    *float64                   `json:"budget_accelerator_hours,omitempty"`
	MaxAgents                 *int                       `json:"max_agents,omitempty"`
	MaxConcurrentAccelerators *int                       `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   *[]domain.MetricDefinition `json:"metrics,omitempty"`
	ReportIntervalSeconds     *int                       `json:"report_interval_seconds,omitempty"`
	StartsAt                  *time.Time                 `json:"starts_at,omitempty"`
	EndsAt                    *time.Time                 `json:"ends_at,omitempty"`
}

// beyondTheBrief names the fields the patch carries other than name and description (and name
// too when alsoName is set): the ones a running experiment no longer takes, and with the name
// the ones a coordinator's token never sets.
func (p PlatformExperimentPatch) beyondTheBrief(alsoName bool) []string {
	var out []string
	carried := func(field string, set bool) {
		if set {
			out = append(out, field)
		}
	}
	carried("name", alsoName && p.Name != nil)
	carried("budget_accelerator_hours", p.BudgetAcceleratorHours != nil)
	carried("max_agents", p.MaxAgents != nil)
	carried("max_concurrent_accelerators", p.MaxConcurrentAccelerators != nil)
	carried("metrics", p.Metrics != nil)
	carried("report_interval_seconds", p.ReportIntervalSeconds != nil)
	carried("starts_at", p.StartsAt != nil)
	carried("ends_at", p.EndsAt != nil)
	return out
}

func (p PlatformExperimentPatch) empty() bool {
	return p.Name == nil && p.Description == nil && len(p.beyondTheBrief(false)) == 0
}

// AgentResult is (agentID, finalMetric) used when closing an experiment.
type AgentResult struct {
	AgentID     string  `json:"agent_id"`
	FinalMetric float64 `json:"final_metric"`
}

// stageProgress is the read-only value served by GET /platform-experiments/{id}/stages. It omits
// the in-flight cost of running jobs that the controller's authoritative value includes, so it
// can trail the boundary the controller acts on.
func (s *PlatformExperimentsService) stageProgress(ctx context.Context, pe *domain.PlatformExperiment) (float64, error) {
	consumed, err := metricsdb.TotalObservedAccH(ctx, s.usage.URL(), pe.CreatedAt, pe.ID)
	if err != nil {
		return 0, fmt.Errorf("stages: observed usage for %s: %w", pe.ID, err)
	}
	return domain.StageProgress(consumed, pe.BudgetAcceleratorHours, pe.StartsAt, pe.EndsAt, time.Now().UTC()), nil
}
