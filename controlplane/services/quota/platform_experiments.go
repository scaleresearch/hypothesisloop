package quota

import (
	"context"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"go.uber.org/zap"
)

// PlatformExperimentsStore is the persistence interface for platform experiments.
type PlatformExperimentsStore interface {
	CreatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error
	GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error)
	ListPlatformExperiments(ctx context.Context, statusFilter string) ([]*domain.PlatformExperiment, error)
	UpdatePlatformExperimentStatus(ctx context.Context, id string, status domain.PlatformExperimentStatus) error
	UpdatePlatformExperiment(ctx context.Context, pe *domain.PlatformExperiment) error
	Signup(ctx context.Context, platformExpID, agentID string) error
	ListSignups(ctx context.Context, platformExpID string) ([]string, error)
	IsSignedUp(ctx context.Context, platformExpID, agentID string) (bool, error)
	CountSignups(ctx context.Context, platformExpID string) (int, error)
	// UpsertAgentQuota/GetAgentQuota/ListAgentQuotas cover allocation only (guaranteed/burst
	// capacity settings) — consumption (used_*) is never stored here; PlatformExperimentsService
	// merges it in from the metrics DB on every read via metricsdb.PopulateUsage(One).
	UpsertAgentQuota(ctx context.Context, q *domain.AgentQuota) error
	GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error)
	ListAgentQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error)
	AddDesiredQuotaUsage(ctx context.Context, platformExpID string, quotas []*domain.AgentQuota) error
	AddDesiredQuotaUsageOne(ctx context.Context, quota *domain.AgentQuota) error
	AdmitExperimentTx(ctx context.Context, exp *domain.Experiment, observed func(context.Context) (*domain.AgentQuota, error)) (rejectionReason string, err error)
	ReserveAdmittedFlavorTx(ctx context.Context, experimentID string, acceleratorType domain.AcceleratorType, estimatedCost float64, observed func(context.Context, string, string) (*domain.AgentQuota, error)) (rejectionReason string, err error)
	RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error
	HasTop3History(ctx context.Context, agentID string) (bool, error)
	IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error)
	ListPhase2HeldAgents(ctx context.Context, platformExpID string) ([]string, error)
	GetAgent(ctx context.Context, agentID string) (*domain.Agent, error)
	ListAgents(ctx context.Context) ([]*domain.Agent, error)
	UpdateAgent(ctx context.Context, agent *domain.Agent) error
	// FulfillDonationTx performs a donation transfer atomically and idempotently — locking the
	// donation + both quota rows, verifying the donation is still open and the donor has headroom,
	// moving the amount and marking the donation fulfilled in one transaction. See
	// db.PlatformExperimentsStore.FulfillDonationTx.
	FulfillDonationTx(ctx context.Context, donationID, donorAgentID, recipientAgentID, platformExpID string, resourceType domain.ResourceType, amount, donorUsedGuaranteed float64) (bool, error)
	// Donation persistence (experiment-scoped donations).
	CreateDonationRequest(ctx context.Context, req *domain.DonationRequest) error
	GetDonationRequest(ctx context.Context, id string) (*domain.DonationRequest, error)
	ListDonationRequests(ctx context.Context, status string) ([]*domain.DonationRequest, error)
	UpdateDonationStatus(ctx context.Context, id, status string) error
}

// PlatformExperimentsService manages the Platform Experiment lifecycle.
type PlatformExperimentsService struct {
	store  PlatformExperimentsStore
	usage  *metricsdb.UsageTracker
	cfg    domain.QuotaConfig
	logger *zap.Logger
}

// NewPlatformExperimentsService constructs the service. metricsDBURL is the GreptimeDB instance
// backing observed agent quota consumption. PostgreSQL holds allocations and current desired
// experiment estimates.
func NewPlatformExperimentsService(store PlatformExperimentsStore, cfg domain.QuotaConfig, logger *zap.Logger, metricsDBURL string) *PlatformExperimentsService {
	return &PlatformExperimentsService{store: store, usage: metricsdb.NewUsageTracker(metricsDBURL), cfg: cfg, logger: logger}
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
}

// AgentResult is (agentID, finalMetric) used when closing an experiment.
type AgentResult struct {
	AgentID     string  `json:"agent_id"`
	FinalMetric float64 `json:"final_metric"`
}
