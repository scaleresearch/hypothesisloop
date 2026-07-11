package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/google/uuid"
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
	RecordTop3(ctx context.Context, platformExpID, agentID string, finalMetric float64) error
	HasTop3History(ctx context.Context, agentID string) (bool, error)
	IsAgentHeld(ctx context.Context, platformExpID, agentID string) (bool, error)
	ListPhase2HeldAgents(ctx context.Context, platformExpID string) ([]string, error)
	GetAgent(ctx context.Context, agentID string) (*domain.Agent, error)
	ListAgents(ctx context.Context) ([]*domain.Agent, error)
	UpdateAgent(ctx context.Context, agent *domain.Agent) error
	// AddToAgentGuaranteedQuota adjusts an agent's guaranteed allocation for resourceType.
	// Used for donation transfers (positive = credit, negative = debit) — donations are
	// GPU-hours only today (domain.ResourceGPUHours).
	AddToAgentGuaranteedQuota(ctx context.Context, agentID, platformExpID string, resourceType domain.ResourceType, delta float64) error
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
// backing agent quota consumption (used_guaranteed_*/used_burst_*) — the sole store for it;
// store only ever holds the guaranteed/burst allocation.
func NewPlatformExperimentsService(store PlatformExperimentsStore, cfg domain.QuotaConfig, logger *zap.Logger, metricsDBURL string) *PlatformExperimentsService {
	return &PlatformExperimentsService{store: store, usage: metricsdb.NewUsageTracker(metricsDBURL), cfg: cfg, logger: logger}
}

// Create creates a new platform experiment in open status (accepting signups immediately).
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	metrics := req.Metrics
	if metrics == nil {
		metrics = []domain.MetricDefinition{}
	}
	reportInterval := req.ReportIntervalSeconds
	if reportInterval <= 0 {
		reportInterval = 30
	}
	pe := &domain.PlatformExperiment{
		ID:                    "pe-" + uuid.New().String()[:8],
		Name:                  req.Name,
		Description:           req.Description,
		BudgetT4Hours:         req.BudgetT4Hours,
		BudgetCPUCoreHours:    req.BudgetCPUCoreHours,
		BudgetRAMGBHours:      req.BudgetRAMGBHours,
		BudgetStorageGBHours:  req.BudgetStorageGBHours,
		MaxAgents:             req.MaxAgents,
		Metrics:               metrics,
		ReportIntervalSeconds: reportInterval,
		StartsAt:              req.StartsAt,
		EndsAt:                req.EndsAt,
		Status:                domain.PlatformExpOpen,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if pe.MaxAgents <= 0 {
		pe.MaxAgents = 100
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name))
	return pe, nil
}

// Update applies editable field changes to an existing platform experiment.
func (s *PlatformExperimentsService) Update(ctx context.Context, id string, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil, fmt.Errorf("experiment_not_editable: can only edit open experiments (status: %s)", pe.Status)
	}
	if req.Name != "" {
		pe.Name = req.Name
	}
	pe.Description = req.Description
	if req.BudgetT4Hours > 0 {
		pe.BudgetT4Hours = req.BudgetT4Hours
	}
	if req.BudgetCPUCoreHours > 0 {
		pe.BudgetCPUCoreHours = req.BudgetCPUCoreHours
	}
	if req.BudgetRAMGBHours > 0 {
		pe.BudgetRAMGBHours = req.BudgetRAMGBHours
	}
	if req.BudgetStorageGBHours > 0 {
		pe.BudgetStorageGBHours = req.BudgetStorageGBHours
	}
	if req.MaxAgents > 0 {
		pe.MaxAgents = req.MaxAgents
	}
	if req.Metrics != nil {
		pe.Metrics = req.Metrics
	}
	if req.ReportIntervalSeconds > 0 {
		pe.ReportIntervalSeconds = req.ReportIntervalSeconds
	}
	if !req.StartsAt.IsZero() {
		pe.StartsAt = req.StartsAt
	}
	if !req.EndsAt.IsZero() {
		pe.EndsAt = req.EndsAt
	}
	if err := s.store.UpdatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	return pe, nil
}

// Signup registers an agent for a platform experiment.
func (s *PlatformExperimentsService) Signup(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return err
	}
	if pe == nil {
		return fmt.Errorf("experiment_not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return fmt.Errorf("signup_closed: experiment is %s", pe.Status)
	}

	count, err := s.store.CountSignups(ctx, platformExpID)
	if err != nil {
		return err
	}
	if count >= pe.MaxAgents {
		return fmt.Errorf("max_agents_reached: limit is %d", pe.MaxAgents)
	}

	if err := s.store.Signup(ctx, platformExpID, agentID); err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	return nil
}

// Start transitions an experiment to running and allocates per-agent quotas.
func (s *PlatformExperimentsService) Start(ctx context.Context, id string) ([]*domain.AgentQuota, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil, fmt.Errorf("invalid_transition: experiment is %s, expected open", pe.Status)
	}

	signedUp, err := s.store.ListSignups(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(signedUp) == 0 {
		return nil, fmt.Errorf("no_agents: cannot start with zero sign-ups")
	}

	// Pass 1: resolve agents and compute each agent's bonus fraction.
	// Unknown agents are skipped; only resolved agents participate in quota.
	type agentBonus struct {
		id    string
		bonus float64
	}
	resolved := make([]agentBonus, 0, len(signedUp))
	totalBonusFraction := 0.0
	for _, agentID := range signedUp {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			s.logger.Warn("start: agent not found, skipping", zap.String("agentID", agentID))
			continue
		}
		hasTop3, _ := s.store.HasTop3History(ctx, agentID)
		bonus := 0.0
		if hasTop3 {
			bonus += s.cfg.Top3BonusFraction
		}
		resolved = append(resolved, agentBonus{id: agentID, bonus: bonus})
		totalBonusFraction += bonus
	}

	quotas := make([]*domain.AgentQuota, 0, len(resolved))
	now := time.Now().UTC()

	// Cap initial allocation to the explore window so no agent can exhaust the
	// full budget before phase-2 eviction kicks in. Applied uniformly to every resource
	// dimension the platform experiment tracks — GPU is always populated; CPU/RAM/storage
	// budgets of 0 correctly allocate 0 (AllocateQuota(0, ...) returns 0,0), which is exactly
	// "not tracked."
	exploreFrac := s.cfg.Phase1ExploreFraction
	if exploreFrac <= 0 {
		exploreFrac = domain.Phase1ExploreFraction
	}

	for _, ab := range resolved {
		gpuGuaranteed, gpuBurst := domain.AllocateQuota(
			pe.BudgetT4Hours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		cpuGuaranteed, cpuBurst := domain.AllocateQuota(
			pe.BudgetCPUCoreHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		ramGuaranteed, ramBurst := domain.AllocateQuota(
			pe.BudgetRAMGBHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)
		storageGuaranteed, storageBurst := domain.AllocateQuota(
			pe.BudgetStorageGBHours*exploreFrac, len(resolved), ab.bonus, totalBonusFraction, s.cfg,
		)

		aq := &domain.AgentQuota{
			ID:                       uuid.New().String(),
			AgentID:                  ab.id,
			PlatformExperimentID:     id,
			GuaranteedT4Hours:        gpuGuaranteed,
			BurstT4Hours:             gpuBurst,
			GuaranteedCPUCoreHours:   cpuGuaranteed,
			BurstCPUCoreHours:        cpuBurst,
			GuaranteedRAMGBHours:     ramGuaranteed,
			BurstRAMGBHours:          ramBurst,
			GuaranteedStorageGBHours: storageGuaranteed,
			BurstStorageGBHours:      storageBurst,
			CreatedAt:                now,
		}
		if err := s.store.UpsertAgentQuota(ctx, aq); err != nil {
			return nil, fmt.Errorf("platform_experiments.Start: upsert quota for %s: %w", ab.id, err)
		}
		quotas = append(quotas, aq)

		s.logger.Info("quota allocated",
			zap.String("agentID", ab.id),
			zap.Float64("guaranteed_gpu_hours", gpuGuaranteed),
			zap.Float64("burst_gpu_hours", gpuBurst),
		)
	}

	if err := s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpRunning); err != nil {
		return nil, err
	}
	return quotas, nil
}

// Close transitions a running experiment to closed and records top-3 placements.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries).
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, topResults []AgentResult) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil {
		return fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpRunning && pe.Status != domain.PlatformExpOpen {
		return fmt.Errorf("invalid_transition: experiment is %s", pe.Status)
	}

	// Record top-3 placements and increment periods_active.
	for i, r := range topResults {
		if i >= 3 {
			break
		}
		if err := s.store.RecordTop3(ctx, id, r.AgentID, r.FinalMetric); err != nil {
			s.logger.Warn("close: record top3 failed", zap.String("agentID", r.AgentID), zap.Error(err))
		}
	}

	return s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpClosed)
}

// GetQuota returns an agent's allocation and current usage for a platform experiment, merging
// the allocation (Postgres) with consumption (metrics DB) into one struct.
func (s *PlatformExperimentsService) GetQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: %w", err)
	}
	if aq == nil {
		return nil, nil
	}
	if err := metricsdb.PopulateUsageOne(ctx, s.usage.URL(), aq); err != nil {
		return nil, fmt.Errorf("platform_experiments.GetQuota: populate usage: %w", err)
	}
	return aq, nil
}

// GetAgentQuota is an alias for GetQuota satisfying the controller.QuotaService interface.
func (s *PlatformExperimentsService) GetAgentQuota(ctx context.Context, agentID, platformExpID string) (*domain.AgentQuota, error) {
	return s.GetQuota(ctx, agentID, platformExpID)
}

// ListQuotas returns every agent's allocation and current usage for a platform experiment,
// merging the allocation (Postgres) with consumption (metrics DB) in one batched usage query.
func (s *PlatformExperimentsService) ListQuotas(ctx context.Context, platformExpID string) ([]*domain.AgentQuota, error) {
	quotas, err := s.store.ListAgentQuotas(ctx, platformExpID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: %w", err)
	}
	if err := metricsdb.PopulateUsage(ctx, s.usage.URL(), platformExpID, quotas); err != nil {
		return nil, fmt.Errorf("platform_experiments.ListQuotas: populate usage: %w", err)
	}
	return quotas, nil
}

// allocationFor returns the (guaranteed, burst) allocation limit for resourceType on aq — the
// Postgres-side half of the check; usage (the other half) lives in the metrics DB and is checked
// by UsageTracker.CheckAndDebit itself.
func allocationFor(aq *domain.AgentQuota, resourceType domain.ResourceType, tier domain.CapacityTier) float64 {
	guaranteed := tier == domain.CapacityGuaranteed
	switch resourceType {
	case domain.ResourceCPUCoreHours:
		if guaranteed {
			return aq.GuaranteedCPUCoreHours
		}
		return aq.BurstCPUCoreHours
	case domain.ResourceRAMGBHours:
		if guaranteed {
			return aq.GuaranteedRAMGBHours
		}
		return aq.BurstRAMGBHours
	case domain.ResourceStorageGBHours:
		if guaranteed {
			return aq.GuaranteedStorageGBHours
		}
		return aq.BurstStorageGBHours
	default: // domain.ResourceGPUHours
		if guaranteed {
			return aq.GuaranteedT4Hours
		}
		return aq.BurstT4Hours
	}
}

// CheckAndDebitQuota validates that the agent has sufficient quota in resourceType's pool and
// debits it. amount is 0 for any dimension the platform experiment doesn't track — a no-op
// debit that only ever succeeds (0 <= 0 remaining), so untracked dimensions never block
// admission.
func (s *PlatformExperimentsService) CheckAndDebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	if amount <= 0 {
		return nil
	}
	// Block held agents from submitting new jobs (Domain 10).
	held, err := s.store.IsAgentHeld(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("phase2 hold check: %w", err)
	}
	if held {
		return fmt.Errorf("agent_phase2_held: agent %s is on hold for experiment %s", agentID, platformExpID)
	}

	aq, err := s.store.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return err
	}
	if aq == nil {
		return fmt.Errorf("insufficient_quota: no quota found (agent not signed up?)")
	}

	limit := allocationFor(aq, resourceType, tier)
	return s.usage.CheckAndDebit(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount, limit)
}

// RefundQuota overwrites experimentID's own resourceType usage with its observed cost (an
// absolute set, not a delta — see UsageTracker.SetObserved). Despite the name (kept for callers
// that still think of this as "the refund step"), amount here means the job's final observed
// cost for this dimension, computed by the caller from confirmed-alive time.
func (s *PlatformExperimentsService) RefundQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return s.usage.SetObserved(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount)
}

// DebitQuota adds amount to experimentID's own resourceType usage without a balance check.
// Used for system-level adjustments such as flavor substitution cost corrections
// (e.g. H100 job admitted on H200 — agent owes the rate difference).
func (s *PlatformExperimentsService) DebitQuota(ctx context.Context, agentID, platformExpID, experimentID string, resourceType domain.ResourceType, tier domain.CapacityTier, amount float64) error {
	return s.usage.Debit(ctx, agentID, platformExpID, experimentID, resourceType, tier, amount)
}

// FulfillDonation transfers T4h from donor to recipient within a platform experiment.
// Debits donor's guaranteed_t4_hours and credits recipient's guaranteed_t4_hours.
// The donation must have a platform_experiment_id; the donor must have sufficient available quota.
func (s *PlatformExperimentsService) FulfillDonation(ctx context.Context, donationID, donorAgentID string) error {
	req, err := s.store.GetDonationRequest(ctx, donationID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donation: %w", err)
	}
	if req == nil {
		return fmt.Errorf("FulfillDonation: donation %s not found", donationID)
	}
	if req.Status != "open" {
		return fmt.Errorf("FulfillDonation: donation is %s, not open", req.Status)
	}
	if req.AgentID == donorAgentID {
		return fmt.Errorf("FulfillDonation: donor and recipient must be different agents")
	}
	if req.PlatformExperimentID == "" {
		return fmt.Errorf("FulfillDonation: donation has no platform_experiment_id")
	}

	donorQuota, err := s.GetQuota(ctx, donorAgentID, req.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("FulfillDonation: get donor quota: %w", err)
	}
	if donorQuota == nil || donorQuota.AvailableGuaranteed() < req.CreditsWant {
		avail := 0.0
		if donorQuota != nil {
			avail = donorQuota.AvailableGuaranteed()
		}
		return fmt.Errorf("insufficient_quota: donor has %.2f T4h available, need %.2f", avail, req.CreditsWant)
	}

	// Debit donor's allocation. Donations are GPU-hours only today.
	if err := s.store.AddToAgentGuaranteedQuota(ctx, donorAgentID, req.PlatformExperimentID, domain.ResourceGPUHours, -req.CreditsWant); err != nil {
		return fmt.Errorf("FulfillDonation: debit donor: %w", err)
	}
	// Credit recipient's allocation.
	if err := s.store.AddToAgentGuaranteedQuota(ctx, req.AgentID, req.PlatformExperimentID, domain.ResourceGPUHours, req.CreditsWant); err != nil {
		_ = s.store.AddToAgentGuaranteedQuota(ctx, donorAgentID, req.PlatformExperimentID, domain.ResourceGPUHours, req.CreditsWant) // rollback
		return fmt.Errorf("FulfillDonation: credit recipient: %w", err)
	}

	if err := s.store.UpdateDonationStatus(ctx, donationID, "fulfilled"); err != nil {
		return fmt.Errorf("FulfillDonation: update status: %w", err)
	}

	s.logger.Info("FulfillDonation: quota transferred",
		zap.String("donationID", donationID),
		zap.String("donor", donorAgentID),
		zap.String("recipient", req.AgentID),
		zap.String("platformExpID", req.PlatformExperimentID),
		zap.Float64("amount", req.CreditsWant),
	)
	return nil
}

// CreatePlatformExperimentRequest is the input for Create.
type CreatePlatformExperimentRequest struct {
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	BudgetT4Hours float64 `json:"budget_t4_hours"`
	// BudgetCPUCoreHours/BudgetRAMGBHours/BudgetStorageGBHours are optional; 0 means that
	// resource dimension isn't tracked for this platform experiment (GPU-only, as before).
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
