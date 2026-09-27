package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"go.uber.org/zap"
)

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// checkLength holds a create to stating how long it lasts in exactly one way.
func (r CreatePlatformExperimentRequest) checkLength() error {
	switch {
	case r.DurationHours < 0:
		return refuseUpdate(422, "invalid_length", "duration_hours must be positive")
	case r.DurationHours > 0 && !r.EndsAt.IsZero():
		return refuseUpdate(422, "invalid_length", "the body states both duration_hours and ends_at: state one")
	case r.DurationHours == 0 && r.EndsAt.IsZero():
		return refuseUpdate(422, "invalid_length", "state how long it lasts: duration_hours or ends_at")
	}
	return nil
}

// withSettings returns spec with the platform's settings in the fields a request may leave to
// them: scheduler.default_report_interval_seconds, stages.default and quota.default_quota_tier.
func (s *PlatformExperimentsService) withSettings(spec CreatePlatformExperimentRequest) CreatePlatformExperimentRequest {
	if spec.ReportIntervalSeconds == 0 {
		spec.ReportIntervalSeconds = s.cfg.DefaultReportIntervalSeconds
	}
	if len(spec.Stages) == 0 {
		spec.Stages = s.cfg.DefaultStages
	}
	if spec.DefaultQuotaTier == "" {
		spec.DefaultQuotaTier = string(s.cfg.DefaultQuotaTier)
	}
	return spec
}

// checkRunLength holds a platform experiment to quota.max_platform_experiment_days from its
// start, which is now for one that states no starts_at. Until its end it may rent machines.
func (s *PlatformExperimentsService) checkRunLength(startsAt, endsAt, now time.Time) error {
	if startsAt.IsZero() {
		startsAt = now
	}
	if length := endsAt.Sub(startsAt); length > s.cfg.MaxRunLength {
		return refuseUpdate(422, "run_too_long", "it would run %.1f days to its ends_at, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state an earlier end",
			length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	return nil
}

// validate is the one check of what a platform experiment is, whoever states it: a create, a
// planner's proposal, the person accepting one or a patch of an existing one. It returns spec as it is stored,
// with the platform's settings where spec leaves a field to them and its length as ends_at.
func (s *PlatformExperimentsService) validate(spec CreatePlatformExperimentRequest, now time.Time) (CreatePlatformExperimentRequest, error) {
	spec = s.withSettings(spec)
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Description) == "" {
		return spec, refuseUpdate(422, "invalid_experiment", "name and description are required: the description is the brief every agent works from")
	}
	if err := domain.ValidateMetricDefinitions(spec.Metrics); err != nil {
		return spec, refuseUpdate(422, "invalid_metrics", "metrics: %s", err)
	}
	if err := domain.RequireRankingMetric(spec.Metrics); err != nil {
		return spec, refuseUpdate(422, "invalid_metrics", "metrics: %s", err)
	}
	if spec.ReportIntervalSeconds <= 0 {
		return spec, refuseUpdate(422, "invalid_experiment", "report_interval_seconds must be positive: how often a job really reports")
	}
	if err := domain.ValidateStages(spec.Stages); err != nil {
		return spec, refuseUpdate(422, "invalid_stages", "stages: %s", err)
	}
	if tier := domain.QuotaTier(spec.DefaultQuotaTier); tier != domain.QuotaTierGuaranteed && tier != domain.QuotaTierBurstOnly {
		return spec, refuseUpdate(422, "invalid_experiment", "default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if spec.MaxConcurrentAccelerators != nil && *spec.MaxConcurrentAccelerators <= 0 {
		return spec, refuseUpdate(422, "invalid_experiment", "max_concurrent_accelerators must be positive")
	}
	if spec.BudgetAcceleratorHours <= 0 {
		return spec, refuseUpdate(422, "invalid_cost", "budget_accelerator_hours is required and must be positive: the cap on everything the run spends")
	}
	if spec.MaxAgents <= 0 {
		return spec, refuseUpdate(422, "invalid_agents", "max_agents is required and must be positive: how many agents may join")
	}
	if err := spec.checkLength(); err != nil {
		return spec, err
	}
	if spec.DurationHours > 0 {
		from := spec.StartsAt
		if from.IsZero() {
			from = now
		}
		spec.EndsAt = from.Add(time.Duration(spec.DurationHours * float64(time.Hour)))
	}
	return spec, s.checkRunLength(spec.StartsAt, spec.EndsAt, now)
}

// Create creates a new platform experiment in open status (accepting signups immediately).
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	req, err := s.validate(req, now)
	if err != nil {
		return nil, err
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
		Metrics:                   req.Metrics,
		ReportIntervalSeconds:     req.ReportIntervalSeconds,
		StartsAt:                  req.StartsAt,
		EndsAt:                    req.EndsAt,
		Status:                    domain.PlatformExpOpen,
		Stages:                    req.Stages,
		CurrentStage:              1,
		DefaultQuotaTier:          domain.QuotaTier(req.DefaultQuotaTier),
		MaxConcurrentAccelerators: req.MaxConcurrentAccelerators,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		CoordinatorHosted:         req.CoordinatorHosted,
		Coordinator:               req.coordinator,
		MaxHosted:                 req.maxHosted,
		Repository:                req.Repository,
		AgentImage:                req.AgentImage,
		Actor:                     req.createdBy,
	}
	if req.prepare != nil {
		if err := req.prepare(ctx, pe); err != nil {
			return nil, err
		}
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name))
	return pe, nil
}

// refuseUpdate is a create or a patch the caller can fix.
func refuseUpdate(status int, reason, format string, args ...any) error {
	return api.Refuse(status, reason, fmt.Sprintf(format, args...))
}

// Update changes the fields the patch carries and no others, and the experiment as patched has
// to pass validate. While the experiment is open every
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
	expectedStatus := pe.Status
	patched := *pe
	if patch.Name != nil {
		patched.Name = *patch.Name
	}
	if patch.Description != nil {
		patched.Description = *patch.Description
	}
	if patch.BudgetAcceleratorHours != nil {
		patched.BudgetAcceleratorHours = *patch.BudgetAcceleratorHours
	}
	if patch.MaxAgents != nil {
		patched.MaxAgents = *patch.MaxAgents
	}
	if patch.Metrics != nil {
		patched.Metrics = *patch.Metrics
	}
	if patch.ReportIntervalSeconds != nil {
		patched.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.StartsAt != nil {
		patched.StartsAt = *patch.StartsAt
	}
	if patch.EndsAt != nil {
		patched.EndsAt = *patch.EndsAt
	}
	if patch.MaxConcurrentAccelerators != nil {
		patched.MaxConcurrentAccelerators = patch.MaxConcurrentAccelerators
	}
	if _, err := s.validate(CreatePlatformExperimentRequest{
		Name: patched.Name, Description: patched.Description,
		BudgetAcceleratorHours: patched.BudgetAcceleratorHours, MaxAgents: patched.MaxAgents,
		MaxConcurrentAccelerators: patched.MaxConcurrentAccelerators, Metrics: patched.Metrics,
		ReportIntervalSeconds: patched.ReportIntervalSeconds, StartsAt: patched.StartsAt, EndsAt: patched.EndsAt,
		Stages: patched.Stages, DefaultQuotaTier: string(patched.DefaultQuotaTier),
	}, time.Now().UTC()); err != nil {
		return nil, err
	}
	pe = &patched
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
		return api.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed")
	}
	cut, err := s.store.IsAgentCut(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if cut {
		return api.Refuse(http.StatusConflict, "agent_held", "agent "+agentID+" was cut from platform experiment "+platformExpID)
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
	// Read before the transaction, which must issue no query of its own.
	runBudget, err := s.RunBudget(ctx, pe)
	if err != nil {
		return nil, err
	}
	if runBudget <= 0 {
		return nil, fmt.Errorf("budget_spent: planning spent the whole budget of %g accelerator hours, so nothing is left for the researchers to share", pe.BudgetAcceleratorHours)
	}

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
				runBudget*exploreFrac, len(participants), bonuses[i], totalBonusFraction, s.cfg,
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

// Close transitions a running experiment to closed and records top-3 placements. by is whose
// credential asked for it, nil when the platform closes it at its end time.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries). When it is
// empty the standings are derived from the metrics store instead: closing must never silently
// discard the outcome of a run just because the caller was a timer rather than a person.
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, by *domain.Actor, topResults []AgentResult) error {
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
		return s.store.ClosePlatformExperiment(ctx, id, by)
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

	return s.store.ClosePlatformExperiment(ctx, id, by)
}

// SweepExpired closes every platform experiment whose ClosesAt has passed: a run at its end, and
// a draft whose planning limit passed or whose answer no person took up, which releases its
// machine. One with no such moment is skipped and runs until someone calls Close.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	now := time.Now()
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpDraft, domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			if closesAt := pe.ClosesAt(); closesAt.IsZero() || closesAt.After(now) {
				continue
			}
			if err := s.Close(ctx, pe.ID, nil, nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment",
				zap.String("platform_experiment_id", pe.ID), zap.Time("closes_at", pe.ClosesAt()))
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
