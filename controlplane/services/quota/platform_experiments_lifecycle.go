package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

// Create creates a new platform experiment in open status (accepting signups immediately).
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	metrics := req.Metrics
	if metrics == nil {
		metrics = []domain.MetricDefinition{}
	}
	if err := domain.ValidateMetricDefinitions(metrics); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	if err := domain.RequireRankingMetric(metrics); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	reportInterval := req.ReportIntervalSeconds
	if reportInterval <= 0 {
		reportInterval = 30
	}
	stages := req.Stages
	if len(stages) == 0 {
		stages = s.cfg.DefaultStages
	}
	// Fixed at creation, so it is rejected here rather than defended against downstream.
	if err := domain.ValidateStages(stages); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	hypothesisPolicy, err := domain.ParseSubmitterPolicy(req.HypothesisSubmitPolicy)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: hypothesis_submit_policy: %w", err)
	}
	jobPolicy, err := domain.ParseSubmitterPolicy(req.JobSubmitPolicy)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: job_submit_policy: %w", err)
	}
	if req.MaxConcurrentAccelerators != nil && *req.MaxConcurrentAccelerators <= 0 {
		return nil, fmt.Errorf("platform_experiments.Create: max_concurrent_accelerators must be positive")
	}
	pe := &domain.PlatformExperiment{
		ID:                        "pe-" + uuid.New().String()[:8],
		Name:                      req.Name,
		Description:               req.Description,
		BudgetAcceleratorHours:    req.BudgetAcceleratorHours,
		MaxAgents:                 req.MaxAgents,
		Metrics:                   metrics,
		ReportIntervalSeconds:     reportInterval,
		StartsAt:                  req.StartsAt,
		EndsAt:                    req.EndsAt,
		Status:                    domain.PlatformExpOpen,
		Stages:                    stages,
		CurrentStage:              1,
		HypothesisSubmitPolicy:    hypothesisPolicy,
		JobSubmitPolicy:           jobPolicy,
		MaxConcurrentAccelerators: req.MaxConcurrentAccelerators,
		CreatedAt:                 now,
		UpdatedAt:                 now,
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
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, fmt.Errorf("experiment_not_editable: can only edit open or running experiments (status: %s)", pe.Status)
	}
	expectedStatus := pe.Status
	// Name/Description are informational only — an operator amending the brief mid-run cannot
	// corrupt anything already committed to quota/admission/stage state. Every other field feeds
	// admission math (budgets, max_agents), stage transitions (metrics), or scheduling (report
	// interval, window) that has already been acted on once the experiment is running, so those
	// stay locked to the open status exactly as before.
	if req.Name != "" {
		pe.Name = req.Name
	}
	pe.Description = req.Description
	if pe.Status == domain.PlatformExpRunning {
		// The form always echoes every field back (so an old CPU budget isn't silently zeroed,
		// see the UI comment above the submit payload), so "field is set" alone can't detect
		// intent to change it — it would reject on every single running-status edit, including
		// pure name/description ones. Compare against what's already stored instead: only an
		// actual attempted change to a locked field is rejected.
		// datetime-local inputs (the only client today) round-trip at minute precision, so an
		// unmodified schedule field comes back coarser than what's stored; compare at that
		// granularity rather than false-flagging every running-status edit as a schedule change.
		const timePrecision = time.Minute
		locked := (req.BudgetAcceleratorHours > 0 && req.BudgetAcceleratorHours != pe.BudgetAcceleratorHours) ||
			(req.MaxAgents > 0 && req.MaxAgents != pe.MaxAgents) ||
			(req.Metrics != nil && !domain.MetricDefinitionsEqual(req.Metrics, pe.Metrics)) ||
			(req.ReportIntervalSeconds > 0 && req.ReportIntervalSeconds != pe.ReportIntervalSeconds) ||
			(!req.StartsAt.IsZero() && !req.StartsAt.Truncate(timePrecision).Equal(pe.StartsAt.Truncate(timePrecision))) ||
			(!req.EndsAt.IsZero() && !req.EndsAt.Truncate(timePrecision).Equal(pe.EndsAt.Truncate(timePrecision))) ||
			(req.HypothesisSubmitPolicy != "" && domain.SubmitterPolicy(req.HypothesisSubmitPolicy) != pe.HypothesisSubmitPolicy) ||
			(req.JobSubmitPolicy != "" && domain.SubmitterPolicy(req.JobSubmitPolicy) != pe.JobSubmitPolicy) ||
			(req.MaxConcurrentAccelerators != nil && (pe.MaxConcurrentAccelerators == nil || *req.MaxConcurrentAccelerators != *pe.MaxConcurrentAccelerators))
		if locked {
			return nil, fmt.Errorf("experiment_running: only name and description can be amended once running")
		}
	} else {
		if req.BudgetAcceleratorHours > 0 {
			pe.BudgetAcceleratorHours = req.BudgetAcceleratorHours
		}
		if req.MaxAgents > 0 {
			pe.MaxAgents = req.MaxAgents
		}
		if req.Metrics != nil {
			if err := domain.ValidateMetricDefinitions(req.Metrics); err != nil {
				return nil, fmt.Errorf("platform_experiments.Update: %w", err)
			}
			if err := domain.RequireRankingMetric(req.Metrics); err != nil {
				return nil, fmt.Errorf("platform_experiments.Update: %w", err)
			}
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
		if req.HypothesisSubmitPolicy != "" {
			policy, err := domain.ParseSubmitterPolicy(req.HypothesisSubmitPolicy)
			if err != nil {
				return nil, fmt.Errorf("platform_experiments.Update: hypothesis_submit_policy: %w", err)
			}
			pe.HypothesisSubmitPolicy = policy
		}
		if req.JobSubmitPolicy != "" {
			policy, err := domain.ParseSubmitterPolicy(req.JobSubmitPolicy)
			if err != nil {
				return nil, fmt.Errorf("platform_experiments.Update: job_submit_policy: %w", err)
			}
			pe.JobSubmitPolicy = policy
		}
		if req.MaxConcurrentAccelerators != nil {
			if *req.MaxConcurrentAccelerators <= 0 {
				return nil, fmt.Errorf("platform_experiments.Update: max_concurrent_accelerators must be positive")
			}
			pe.MaxConcurrentAccelerators = req.MaxConcurrentAccelerators
		}
	}
	if err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	return pe, nil
}

// Signup registers an agent for a platform experiment in a fixed role. quotaTierOverride is the
// signup-time explicit tier override ("" defers to domain.ResolveQuotaTier's kind default).
func (s *PlatformExperimentsService) Signup(ctx context.Context, platformExpID, agentID string, role domain.SignupRole, quotaTierOverride domain.QuotaTier) error {
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

	// max_agents sizes the field being ranked, so it counts competitors only: adding a baseline
	// or a reviewer must not shrink the competition it exists to measure.
	if role == domain.SignupRoleCompetitor {
		count, err := s.store.CountSignupsByRole(ctx, platformExpID, domain.SignupRoleCompetitor)
		if err != nil {
			return err
		}
		if count >= pe.MaxAgents {
			return fmt.Errorf("max_agents_reached: limit is %d", pe.MaxAgents)
		}
	}

	inserted, err := s.store.Signup(ctx, platformExpID, agentID, role, quotaTierOverride)
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if !inserted {
		// The insert is guarded on the experiment still being open, so this is either a repeat
		// signup or a Start that committed since the status was read above.
		already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("platform_experiments.Signup: %w", err)
		}
		if !already {
			return fmt.Errorf("signup_closed: experiment started")
		}
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID),
		zap.String("agentID", agentID), zap.String("role", string(role)))
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
				domain.ResolveQuotaTier(p.Kind, p.QuotaTierOverride), acceleratorGuaranteed, acceleratorBurst)
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
	if pe.Status != domain.PlatformExpRunning && pe.Status != domain.PlatformExpOpen {
		return fmt.Errorf("invalid_transition: experiment is %s", pe.Status)
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
// a platform experiment with no end time runs until someone calls Close explicitly.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	now := time.Now()
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			if pe.EndsAt.IsZero() || pe.EndsAt.After(now) {
				continue
			}
			// TODO(winners): topResults is nil here, so Close never calls RecordTop3 for an
			// auto-closed (ends_at-expired) platform experiment — the only realistic path for a
			// real deadline-bound experiment. RecordTop3/HasTop3History (the quota bonus new
			// experiments give agents with a prior top-3) is effectively dead code as a result.
			// Fix: compute topResults here from the same per-agent-best-metric ranking the stage
			// boundary and the final standings both already use
			// (metricsdb.BestPerAgentOnMetric), keyed off pe.Metrics[0] as the primary metric,
			// sorted best-first, then pass the top 3 into Close. Deliberately not done here —
			// see agent/ session notes: no leaderboard/reputation use case needs it yet.
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
