package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// ErrInvalidPlatformExperiment wraps every Create rejection of the request itself.
var ErrInvalidPlatformExperiment = errors.New("invalid platform experiment")

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

func invalidPlatformExperiment(err error) error {
	return fmt.Errorf("platform_experiments.Create: %w: %w", ErrInvalidPlatformExperiment, err)
}

// Create creates a new platform experiment in open status (accepting signups immediately).
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	metrics := req.Metrics
	if metrics == nil {
		metrics = []domain.MetricDefinition{}
	}
	if err := domain.ValidateMetricDefinitions(metrics); err != nil {
		return nil, invalidPlatformExperiment(err)
	}
	if err := domain.RequireRankingMetric(metrics); err != nil {
		return nil, invalidPlatformExperiment(err)
	}
	if req.ReportIntervalSeconds <= 0 {
		return nil, invalidPlatformExperiment(errors.New("report_interval_seconds must be positive"))
	}
	if req.MaxAgents <= 0 {
		return nil, invalidPlatformExperiment(errors.New("max_agents must be positive"))
	}
	stages := req.Stages
	if len(stages) == 0 {
		stages = s.cfg.DefaultStages
	}
	// Fixed at creation, so it is rejected here rather than defended against downstream.
	if err := domain.ValidateStages(stages); err != nil {
		return nil, invalidPlatformExperiment(err)
	}
	hypothesisPolicy, err := domain.ParseSubmitterPolicy(req.HypothesisSubmitPolicy)
	if err != nil {
		return nil, invalidPlatformExperiment(fmt.Errorf("hypothesis_submit_policy: %w", err))
	}
	jobPolicy, err := domain.ParseSubmitterPolicy(req.JobSubmitPolicy)
	if err != nil {
		return nil, invalidPlatformExperiment(fmt.Errorf("job_submit_policy: %w", err))
	}
	defaultQuotaTier := domain.QuotaTier(req.DefaultQuotaTier)
	if defaultQuotaTier == "" {
		defaultQuotaTier = domain.QuotaTierGuaranteed
	}
	if !domain.ValidQuotaTierOverride(string(defaultQuotaTier)) {
		return nil, invalidPlatformExperiment(errors.New("default_quota_tier must be \"guaranteed\" or \"burst_only\""))
	}
	if req.MaxConcurrentAccelerators != nil && *req.MaxConcurrentAccelerators <= 0 {
		return nil, invalidPlatformExperiment(errors.New("max_concurrent_accelerators must be positive"))
	}
	if req.EndsAt.IsZero() {
		return nil, invalidPlatformExperiment(errors.New("ends_at is required"))
	}
	if req.CoordinatorHosted != (req.coordinator != nil) {
		return nil, fmt.Errorf("platform_experiments.Create: coordinator_hosted is %v and its launcher and model are resolved: %v", req.CoordinatorHosted, req.coordinator != nil)
	}
	pe := &domain.PlatformExperiment{
		ID:                        "pe-" + uuid.New().String()[:8],
		ProjectID:                 req.ProjectID,
		Name:                      req.Name,
		Description:               req.Description,
		BudgetAcceleratorHours:    req.BudgetAcceleratorHours,
		MaxAgents:                 req.MaxAgents,
		Metrics:                   metrics,
		ReportIntervalSeconds:     req.ReportIntervalSeconds,
		StartsAt:                  req.StartsAt,
		EndsAt:                    req.EndsAt,
		Status:                    domain.PlatformExpOpen,
		Stages:                    stages,
		CurrentStage:              1,
		HypothesisSubmitPolicy:    hypothesisPolicy,
		JobSubmitPolicy:           jobPolicy,
		DefaultQuotaTier:          defaultQuotaTier,
		MaxConcurrentAccelerators: req.MaxConcurrentAccelerators,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		CoordinatorHosted:         req.CoordinatorHosted,
		Coordinator:               req.coordinator,
		MaxHosted:                 req.maxHosted,
		Repository:                req.Repository,
		RepositoryURL:             domain.RepositoryURL(req.Repository),
		AgentImage:                req.AgentImage,
		CreatedBy:                 req.createdBy,
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name))
	return pe, nil
}

// refuseUpdate is a patch the caller can fix.
func refuseUpdate(status int, reason, format string, args ...any) error {
	return &reasonError{status: status, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Update changes the fields the patch carries and no others. While the experiment is open every
// field of the patch can change. Once it runs, only the name and the description can: admission
// and the stage ladder have already acted on the rest. A closed one, and a draft, which its
// planner's proposal fills, take no patch.
func (s *PlatformExperimentsService) Update(ctx context.Context, id string, patch PlatformExperimentPatch) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, refuseUpdate(404, "experiment_not_found", "no platform experiment %s", id)
	}
	if patch.empty() {
		return nil, refuseUpdate(422, "nothing_to_change", "the body names no field: send the fields that change")
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, refuseUpdate(409, "experiment_not_editable", "platform experiment %s is %s: only an open or running one is changed", id, pe.Status)
	}
	if locked := patch.beyondTheBrief(false); pe.Status == domain.PlatformExpRunning && len(locked) > 0 {
		return nil, refuseUpdate(409, "experiment_running", "only name and description change once a platform experiment runs, and the body also carries: %s. Leave them out", strings.Join(locked, ", "))
	}
	invalid := func(format string, args ...any) error { return refuseUpdate(422, "invalid_field", format, args...) }
	expectedStatus := pe.Status
	if patch.Name != nil {
		if strings.TrimSpace(*patch.Name) == "" {
			return nil, invalid("name must not be empty")
		}
		pe.Name = *patch.Name
	}
	if patch.Description != nil {
		pe.Description = *patch.Description
	}
	if patch.BudgetAcceleratorHours != nil {
		if *patch.BudgetAcceleratorHours <= 0 {
			return nil, invalid("budget_accelerator_hours must be positive")
		}
		pe.BudgetAcceleratorHours = *patch.BudgetAcceleratorHours
	}
	if patch.MaxAgents != nil {
		if *patch.MaxAgents <= 0 {
			return nil, invalid("max_agents must be positive")
		}
		pe.MaxAgents = *patch.MaxAgents
	}
	if patch.Metrics != nil {
		if err := domain.ValidateMetricDefinitions(*patch.Metrics); err != nil {
			return nil, invalid("metrics: %s", err)
		}
		if err := domain.RequireRankingMetric(*patch.Metrics); err != nil {
			return nil, invalid("metrics: %s", err)
		}
		pe.Metrics = *patch.Metrics
	}
	if patch.ReportIntervalSeconds != nil {
		if *patch.ReportIntervalSeconds <= 0 {
			return nil, invalid("report_interval_seconds must be positive")
		}
		pe.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.StartsAt != nil {
		pe.StartsAt = *patch.StartsAt
	}
	if patch.EndsAt != nil {
		if patch.EndsAt.IsZero() {
			return nil, invalid("ends_at must be a time: a platform experiment always has an end")
		}
		pe.EndsAt = *patch.EndsAt
	}
	if patch.HypothesisSubmitPolicy != nil {
		policy, err := domain.ParseSubmitterPolicy(*patch.HypothesisSubmitPolicy)
		if err != nil {
			return nil, invalid("hypothesis_submit_policy: %s", err)
		}
		pe.HypothesisSubmitPolicy = policy
	}
	if patch.JobSubmitPolicy != nil {
		policy, err := domain.ParseSubmitterPolicy(*patch.JobSubmitPolicy)
		if err != nil {
			return nil, invalid("job_submit_policy: %s", err)
		}
		pe.JobSubmitPolicy = policy
	}
	if patch.MaxConcurrentAccelerators != nil {
		if *patch.MaxConcurrentAccelerators <= 0 {
			return nil, invalid("max_concurrent_accelerators must be positive")
		}
		pe.MaxConcurrentAccelerators = patch.MaxConcurrentAccelerators
	}
	if err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	return pe, nil
}

func maxHostedReached(limit int) string {
	return fmt.Sprintf("the platform runs at most %d machines for one platform experiment, its hosted agents of every role and its coordinator together", limit)
}

// Signup gives a registered agent its place in a platform experiment: a fixed role, and
// everything it is launched with. An empty QuotaTier defers to the experiment's
// default_quota_tier policy (see domain.ResolveQuotaTier).
func (s *PlatformExperimentsService) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) error {
	agentID := signup.AgentID
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return err
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	// A member signing up again has what it asked for, whatever the experiment's status or size.
	already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if already {
		return nil
	}
	if pe.Status != domain.PlatformExpOpen {
		return refuseSignup(400, "signup_closed", "experiment is %s", pe.Status)
	}

	inserted, err := s.store.Signup(ctx, platformExpID, signup)
	if errors.Is(err, db.ErrMaxAgentsReached) {
		return refuseSignup(400, "max_agents_reached", "limit is %d", pe.MaxAgents)
	}
	if errors.Is(err, db.ErrMaxHostedReached) {
		return refuseSignup(400, "max_hosted_reached", "%s", maxHostedReached(pe.MaxHosted))
	}
	// Not found either way: an agent of another org is not this experiment's to learn of.
	if errors.Is(err, db.ErrAgentOutsideOrg) {
		return refuseSignup(404, "agent_not_found", "no agent %s is registered", agentID)
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if !inserted {
		// The insert is guarded on the experiment still being open, so this is either a
		// concurrent signup of the same agent or a Start that committed since the reads above.
		already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("platform_experiments.Signup: %w", err)
		}
		if !already {
			return refuseSignup(400, "signup_closed", "experiment started")
		}
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID),
		zap.String("agentID", agentID), zap.String("role", string(signup.Role)))
	return nil
}

// RequireActive refuses a request that starts work or spends for an agent that has no further
// part in the platform experiment: it closed, or the agent was cut. A token that is still alive
// to file summaries does not reopen either.
func (s *PlatformExperimentsService) RequireActive(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if pe == nil || pe.Status == domain.PlatformExpClosed {
		return &reasonError{status: http.StatusConflict, Reason: "experiment_closed", Message: "platform experiment " + platformExpID + " is closed"}
	}
	cut, err := s.store.IsAgentCut(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if cut {
		return &reasonError{status: http.StatusConflict, Reason: "agent_held", Message: "agent " + agentID + " was cut from platform experiment " + platformExpID}
	}
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

	// Only the first stage's share of the budget is released now; each later stage releases its
	// own share at the boundary it starts (see controller.applyCut). This is what stops one agent
	// exhausting the whole budget before the ladder has cut anyone.
	exploreFrac := pe.Stages[0].LengthPct / 100.0
	now := time.Now().UTC()

	started, quotas, err := s.store.StartPlatformExperimentTx(ctx, id, func(participants []db.StartParticipant) ([]*domain.AgentQuota, error) {
		if len(participants) == 0 {
			return nil, fmt.Errorf("no_agents: cannot start with zero sign-ups")
		}
		// Every signed-up agent participates or the start fails. Skipping the ones that could not
		// be resolved used to mean a transient database error permanently disinherited an agent:
		// the experiment started without it, and there is no second allocation pass.
		bonuses := make([]float64, len(participants))
		totalBonusFraction := 0.0
		for i, p := range participants {
			if !p.AgentExists {
				return nil, fmt.Errorf("platform_experiments.Start: agent %s is signed up but does not exist", p.AgentID)
			}
			if p.HasTop3 {
				bonuses[i] = s.cfg.Top3BonusFraction
				totalBonusFraction += s.cfg.Top3BonusFraction
			}
		}

		allocated := make([]*domain.AgentQuota, 0, len(participants))
		for i, p := range participants {
			agentID := p.AgentID
			acceleratorGuaranteed, acceleratorBurst := domain.AllocateQuota(
				pe.BudgetAcceleratorHours*exploreFrac, len(participants), bonuses[i], totalBonusFraction, s.cfg,
			)
			// The tier decides which column the share lands in, never how large it is: everyone
			// is allocated the same way and a burst-only participant's guaranteed part is moved
			// into burst, not taken away.
			acceleratorGuaranteed, acceleratorBurst = domain.ApplyQuotaTier(
				domain.ResolveQuotaTier(p.QuotaTierOverride, pe.DefaultQuotaTier), acceleratorGuaranteed, acceleratorBurst)
			allocated = append(allocated, &domain.AgentQuota{
				ID:                         uuid.New().String(),
				AgentID:                    agentID,
				PlatformExperimentID:       id,
				GuaranteedAcceleratorHours: acceleratorGuaranteed,
				BurstAcceleratorHours:      acceleratorBurst,
				CreatedAt:                  now,
			})
		}
		return allocated, nil
	})
	if err != nil {
		return nil, err
	}
	if !started {
		return nil, fmt.Errorf("invalid_transition: experiment is no longer open")
	}
	for _, q := range quotas {
		s.logger.Info("quota allocated",
			zap.String("agentID", q.AgentID),
			zap.Float64("guaranteed_accelerator_hours", q.GuaranteedAcceleratorHours),
			zap.Float64("burst_accelerator_hours", q.BurstAcceleratorHours),
		)
	}
	return quotas, nil
}

// SetSummary records the operator's narrative verdict on a run. Unlike every other field, this is
// writable after the experiment closes — a summary only exists once the run is over.
func (s *PlatformExperimentsService) SetSummary(ctx context.Context, id, summary string) error {
	return s.store.SetPlatformExperimentSummary(ctx, id, summary)
}

// Close transitions a running experiment to closed and records top-3 placements.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries). When it is
// empty the standings are derived from the metrics store instead: closing must never silently
// discard the outcome of a run just because the caller was a timer rather than a person.
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, topResults []AgentResult) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil {
		return fmt.Errorf("not_found")
	}
	if pe.Status != domain.PlatformExpRunning && pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpDraft {
		return fmt.Errorf("invalid_transition: experiment is %s", pe.Status)
	}
	// A draft never ran: closing it abandons the plan, and there are no standings to record.
	if pe.Status == domain.PlatformExpDraft {
		return s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpClosed)
	}

	if len(topResults) == 0 {
		// Standings are what closing produces. Closing is irreversible, so deriving them failing
		// has to stop the close and be retried, not quietly close an experiment with no result.
		derived, err := s.derivedTopResults(ctx, id)
		if err != nil {
			return fmt.Errorf("close: derive standings for %s: %w", id, err)
		}
		topResults = derived
	}

	for i, r := range topResults {
		if i >= 3 {
			break
		}
		if err := s.store.RecordTop3(ctx, id, r.AgentID, r.FinalMetric); err != nil {
			return fmt.Errorf("close: record placement for %s: %w", r.AgentID, err)
		}
	}

	return s.store.UpdatePlatformExperimentStatus(ctx, id, domain.PlatformExpClosed)
}

// SweepExpired closes every Open/Running platform experiment whose EndsAt has passed. EndsAt is
// the zero value when unset (PlatformExperiment.EndsAt is not a pointer), so those are skipped —
// a platform experiment with no end time runs until someone calls Close explicitly. A draft's
// EndsAt is its planning limit, and once proposed how long the proposal waits for a person, so
// the same sweep closes a plan nobody finished or accepted and releases its machine.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	now := time.Now()
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpDraft, domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			if pe.EndsAt.IsZero() || pe.EndsAt.After(now) {
				continue
			}
			if err := s.Close(ctx, pe.ID, nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment past ends_at",
				zap.String("platform_experiment_id", pe.ID), zap.Time("ends_at", pe.EndsAt))
		}
	}
	return nil
}

// SweepAutoStart starts every Open platform experiment whose StartsAt has passed and has at
// least one sign-up. Without this, an operator has to call POST /start by hand the moment
// starts_at arrives — StartsAt would otherwise be inert metadata, which breaks the case this
// platform is actually for: agents discovering and running a platform experiment end-to-end with
// nobody watching the clock for them. A zero StartsAt (unset) is treated as "start immediately
// on first sign-up" is *not* implied here — Start still requires an explicit call in that case,
// since there's no scheduled moment to sweep against; only a non-zero, past StartsAt auto-fires.
func (s *PlatformExperimentsService) SweepAutoStart(ctx context.Context) error {
	now := time.Now()
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.StartsAt.IsZero() || pe.StartsAt.After(now) {
			continue
		}
		signedUp, err := s.store.ListSignups(ctx, pe.ID)
		if err != nil {
			s.logger.Error("sweep_auto_start: list signups failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		if len(signedUp) == 0 {
			continue // wait for at least one sign-up before starting, same rule Start() enforces
		}
		if _, err := s.Start(ctx, pe.ID); err != nil {
			s.logger.Error("sweep_auto_start: start failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		s.logger.Info("sweep_auto_start: started platform experiment past starts_at",
			zap.String("platform_experiment_id", pe.ID), zap.Time("starts_at", pe.StartsAt))
	}
	return nil
}

// StartExpirySweep runs SweepAutoStart and SweepExpired on a ticker until ctx is cancelled.
// Mirrors the idiomatic ticker-loop pattern used elsewhere in this codebase (see
// scheduler.JobWatcher.Start). Auto-start runs before auto-close so a platform experiment whose
// starts_at and ends_at are both already in the past (e.g. after a control-service restart) is
// briefly started and then immediately closed, rather than closed while still "open" — closing
// requires status running-or-open already, so this ordering isn't strictly required for
// correctness, but it keeps the transition history sane (open -> running -> closed).
func (s *PlatformExperimentsService) StartExpirySweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SweepAutoStart(ctx); err != nil {
				s.logger.Error("sweep_auto_start: scan", zap.Error(err))
			}
			if err := s.SweepExpired(ctx); err != nil {
				s.logger.Error("sweep_expired: scan", zap.Error(err))
			}
		}
	}
}
