package quota

import (
	"context"
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
	UpdatePlatformExperimentStatus(ctx context.Context, id string, status domain.PlatformExperimentStatus) error
	// UpdatePlatformExperiment writes pe only if it is still in expectedStatus — see the db
	// implementation's comment for why the compare-and-swap matters here.
	UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment, expectedStatus domain.PlatformExperimentStatus) error
	SetPlatformExperimentSummary(ctx context.Context, id, summary string) error
	// Signup inserts only while the experiment is still open; inserted=false means it is closed
	// or the agent was already signed up.
	Signup(ctx context.Context, platformExpID, agentID string, role domain.SignupRole) (bool, error)
	// StartPlatformExperimentTx flips open->running and writes every agent quota atomically.
	StartPlatformExperimentTx(ctx context.Context, id string, quotasFor func(participants []db.StartParticipant) ([]*domain.AgentQuota, error)) (bool, []*domain.AgentQuota, error)
	ListSignups(ctx context.Context, platformExpID string) ([]string, error)
	// ListSignupsByRole/CountSignupsByRole are the ranking-side reads: standings and the
	// max_agents field are about competitors, never about the whole roster.
	ListSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) ([]string, error)
	GetSignupRole(ctx context.Context, platformExpID, agentID string) (domain.SignupRole, bool, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	CountSignups(ctx context.Context, platformExpID string) (int, error)
	CountSignupsByRole(ctx context.Context, platformExpID string, role domain.SignupRole) (int, error)
	// UpsertAgentQuota/GetAgentQuota/ListAgentQuotas cover allocation only (guaranteed/burst
	// capacity settings) — consumption (used_*) is never stored here; PlatformExperimentsService
	// merges it in from the metrics DB on every read via metricsdb.PopulateUsage(One).
	UpsertAgentQuota(ctx context.Context, q *domain.AgentQuota) error
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
	AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observed func(context.Context) (*domain.AgentQuota, error), rateLimit db.SubmissionRateLimit) (decision db.AdmitDecision, rejectionReason string, err error)
	ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, observed func(context.Context, string, string) (*domain.AgentQuota, error)) (rejectionReason string, err error)
	RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error
	HasTop3History(ctx context.Context, agentID string) (bool, error)
	IsAgentCut(ctx context.Context, platformExpID, agentID string) (bool, error)
	ListCutAgents(ctx context.Context, platformExpID string) ([]domain.AgentCut, error)
	ListStageAdvances(ctx context.Context, platformExpID string) ([]domain.StageAdvance, error)
	GetAgent(ctx context.Context, agentID string) (*domain.Agent, error)
	ListAgents(ctx context.Context, limit, offset int) ([]*domain.Agent, error)
	UpdateAgent(ctx context.Context, agent *domain.Agent) error
	// FulfillDonationTx performs a donation transfer atomically and idempotently — locking the
	// donation + both quota rows, verifying the donation is still open and the donor has headroom,
	// moving the amount and marking the donation fulfilled in one transaction. observe is called
	// inside that transaction, with the rows already locked, so the headroom check reads usage a
	// concurrent admission can no longer change. See db.PlatformExperimentsStore.FulfillDonationTx.
	FulfillDonationTx(ctx context.Context, donationID, donorAgentID, recipientAgentID, platformExpID string, resourceType domain.ResourceType, amount, burstAmount float64, observe func(context.Context) (*domain.AgentQuota, error)) (bool, error)
	// Donation persistence (experiment-scoped donations).
	CreateDonationRequest(ctx context.Context, req *domain.DonationRequest) error
	GetDonationRequest(ctx context.Context, id string) (*domain.DonationRequest, error)
	ListDonationRequests(ctx context.Context, status string, limit, offset int) ([]*domain.DonationRequest, error)
	CountDonationRequests(ctx context.Context, status string) (int, error)
	UpdateDonationStatus(ctx context.Context, id, status string) error
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

// CreatePlatformExperimentRequest is the input for Create.
type CreatePlatformExperimentRequest struct {
	Name                   string  `json:"name"`
	Description            string  `json:"description"`
	BudgetAcceleratorHours float64 `json:"budget_accelerator_hours"`
	// BudgetCPUCoreHours/BudgetRAMGBHours/BudgetStorageGBHours are optional; 0 means that
	// resource dimension isn't tracked for this platform experiment (Accelerator-only, as before).
	BudgetCPUCoreHours    float64                   `json:"budget_cpu_core_hours,omitempty"`
	BudgetRAMGBHours      float64                   `json:"budget_ram_gb_hours,omitempty"`
	BudgetStorageGBHours  float64                   `json:"budget_storage_gb_hours,omitempty"`
	MaxAgents             int                       `json:"max_agents"`
	Metrics               []domain.MetricDefinition `json:"metrics"`                 // metric keys jobs must emit
	ReportIntervalSeconds int                       `json:"report_interval_seconds"` // expected reporting cadence
	StartsAt              time.Time                 `json:"starts_at"`
	EndsAt                time.Time                 `json:"ends_at"`
	// Stages is the elimination ladder, fixed at creation. Omit to get the platform default
	// (config stages.default).
	Stages []domain.Stage `json:"stages,omitempty"`
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
