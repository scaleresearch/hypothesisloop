package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// heldToBaseline holds pe, as it would be stored, to the job it names as its baseline: the job
// must have done what a baseline does (checkBaseline) and the budget must hold a trial of it for
// each researcher (budgetCoversTrials), and with ownJobs no other job of its agent beat it
// (strongerOwnJob). It sets on pe the estimate of a job that states none and answers the block
// the description is given. It is the one check of a declaration, whoever declares.
func (s *Service) heldToBaseline(ctx context.Context, pe *domain.PlatformExperiment, ownJobs bool) (string, error) {
	baseline, err := s.baseline(ctx, pe, pe.BaselineJobID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments: read baseline %s: %w", pe.BaselineJobID, err)
	}
	if err := checkBaseline(pe, baseline); err != nil {
		return "", err
	}
	if ownJobs {
		if err := s.strongerOwnJob(ctx, pe, baseline); err != nil {
			return "", err
		}
	}
	pe.JobEstimatedDurationHours = BaselineHeadroom * baseline.ObservedHours
	if err := budgetCoversTrials(pe, baseline.Job); err != nil {
		return "", err
	}
	return baselineBlock(baseline, pe.Metrics), nil
}

// measuredJob is the job the platform declares as the baseline of the open pe by itself: the one
// COMPLETED job on the objective that reports the ranking metric, among those its baseline agents
// that finished left. With none, or several, it answers the refusal that is the experiment's
// status_reason until someone declares one or another baseline agent finishes: a choice between
// jobs, and what to do about none, need judgement.
func (s *Service) measuredJob(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (string, error) {
	var joined, measuring, finished, gaveUp []string
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			continue
		}
		joined = append(joined, signup.Name)
		switch {
		case signup.FinishedAt != nil:
			finished = append(finished, signup.Name)
			gaveUp = append(gaveUp, signup.Name+" finished with the reason \""+signup.FinishedReason+"\"")
		case signup.Instance != nil && signup.Instance.Stopped:
			gaveUp = append(gaveUp, signup.Name+"'s machine stopped with no restart left (its instance says why)")
		default:
			measuring = append(measuring, signup.Name)
		}
	}
	ranked, _ := domain.PrimaryRankingMetric(pe.Metrics)
	var measured []string
	if len(finished) > 0 {
		jobs, err := s.store.FinishedBaselineJobs(ctx, pe.ID)
		if err != nil {
			return "", fmt.Errorf("platform_experiments.measuredJob: %w", err)
		}
		for _, job := range jobs {
			baseline, err := s.baseline(ctx, pe, job.ID)
			if err != nil {
				return "", fmt.Errorf("platform_experiments.measuredJob: read %s: %w", job.ID, err)
			}
			if baseline == nil || job.Branch != gitrefs.BaselineBranch(pe.ID) {
				continue
			}
			if _, reported := baseline.Reported[ranked.Key]; reported {
				measured = append(measured, job.ID)
			}
		}
	}
	switch {
	case len(measured) == 1:
		return measured[0], nil
	case len(measured) > 1:
		return "", refusal.Refuse(http.StatusConflict, "baseline_undeclared", fmt.Sprintf("the baseline agents of platform experiment %s left %d COMPLETED jobs that report %s (%s), and the platform declares a job by itself only where there is one to declare: its coordinator declares the one the run is ranked against with %s",
			pe.ID, len(measured), ranked.Key, strings.Join(measured, ", "), declareBaseline(pe.ID)))
	case len(measuring) > 0:
		return "", refusal.Refuse(http.StatusConflict, "baseline_measuring", fmt.Sprintf("baseline agent %s is measuring the baseline of platform experiment %s: when it finishes the platform declares its COMPLETED job and starts the run, and nobody else has anything to do until then",
			strings.Join(measuring, ", "), pe.ID))
	case len(joined) == 0:
		return "", refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared and no agent of role baseline to measure one: its coordinator adds one with %s, and the platform declares the job it leaves and starts the run",
			pe.ID, addBaselineAgent(pe.ID)))
	}
	return "", refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and no baseline agent left a COMPLETED job that reports %s: %s. Its coordinator repairs what the description can and adds another baseline agent with %s, and what only its owner can repair it writes into the summary",
		pe.ID, ranked.Key, strings.Join(gaveUp, "; "), addBaselineAgent(pe.ID)))
}

// DeclareMeasured declares the baseline of the open platform experiment id when its baseline
// agents left exactly one job to declare (measuredJob), by the declaration a coordinator makes
// (Update), which starts the run when nothing else is missing. The request in which a baseline
// agent finishes calls it, the one that changes what a declaration was refused for, and the
// sweep for what neither could do. A declaration that is refused stores nothing, and the
// experiment's status_reason says the refusal (undeclared) until it is cleared.
func (s *Service) DeclareMeasured(ctx context.Context, id string) error {
	return s.declareFinished(ctx, id, "")
}

// declareFinished is DeclareMeasured for the job a finishing baseline agent named, or for the
// one measuredJob finds when it named none.
func (s *Service) declareFinished(ctx context.Context, id, jobID string) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	if pe.Status != domain.PlatformExpOpen || pe.BaselineJobID != "" {
		return nil
	}
	var refused *refusal.Refusal
	if jobID == "" {
		signups, err := s.store.ListAgentSignups(ctx, id)
		if err != nil {
			return fmt.Errorf("platform_experiments.DeclareMeasured: %w", err)
		}
		jobID, err = s.measuredJob(ctx, pe, signups)
		if errors.As(err, &refused) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if _, err := s.Update(ctx, id, PlatformExperimentPatch{BaselineJobID: &jobID}); errors.As(err, &refused) {
		s.logger.Info("the platform did not declare the measured baseline", zap.String("platform_experiment_id", id), zap.String("job_id", jobID), zap.String("reason", refused.Reason))
		return nil
	} else if err != nil {
		return fmt.Errorf("platform_experiments.DeclareMeasured: %w", err)
	}
	s.logger.Info("the platform declared the measured baseline", zap.String("platform_experiment_id", id), zap.String("job_id", jobID))
	return nil
}

// undeclared is why the open pe, which has no baseline, has none: what measuredJob refuses with,
// or for the one job the platform would declare, what the declaration is refused for.
func (s *Service) undeclared(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*refusal.Refusal, error) {
	jobID, err := s.measuredJob(ctx, pe, signups)
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refused, nil
	}
	if err != nil {
		return nil, err
	}
	declared := *pe
	declared.BaselineJobID = jobID
	if _, err := s.heldToBaseline(ctx, &declared, true); errors.As(err, &refused) {
		return refusal.Refuse(http.StatusConflict, refused.Reason, fmt.Sprintf("the platform did not declare job %s, the one job its baseline agents left, as the baseline of platform experiment %s: %s. The platform declares it in the request that clears this",
			jobID, pe.ID, refused.Message)), nil
	} else if err != nil {
		return nil, err
	}
	return refusal.Refuse(http.StatusConflict, "baseline_declaring", fmt.Sprintf("job %s is the baseline of platform experiment %s and the platform declares it within a minute", jobID, pe.ID)), nil
}

// StartIfReady starts the open platform experiment id when nothing is missing, in the request
// that supplied what was: a declared baseline or a researcher. One that still waits is left open.
// The sweep starts what only time brings: a starts_at that arrives.
func (s *Service) StartIfReady(ctx context.Context, id string) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil
	}
	_, err = s.start(ctx, pe, time.Now().UTC())
	var waiting *refusal.Refusal
	if errors.As(err, &waiting) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.StartIfReady: %w", err)
	}
	s.logger.Info("started platform experiment", zap.String("platform_experiment_id", id))
	return nil
}

// maxAgentsReached refuses a researcher past max_agents, which counts the researchers at work and
// nothing else.
func maxAgentsReached(limit int) error {
	return refuseSignup(409, "max_agents_reached", "max_agents is %d and that many researchers (agents of role competitor) are at work. One that finished, was cut, was replaced or lost its machine with no restart left no longer counts, and agents of role baseline, reviewers and the coordinator never do: the owner raises max_agents, or the agent joins with another role", limit)
}

func maxAgentSandboxesReached(limit int) string {
	return fmt.Sprintf("the platform runs at most %d machines for one platform experiment at once. One is kept for an agent of role baseline, which measures the baseline nothing starts without, and its coordinator and its other sandboxed agents take the rest. An agent that finished holds none, nor does one whose machine stopped with no restart left", limit)
}

// Signup gives a registered agent its place in a platform experiment: a fixed role, and
// everything it is launched with. An empty QuotaTier defers to the experiment's
// default_quota_tier policy (see domain.ResolveQuotaTier).
func (s *Service) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) error {
	agentID := signup.AgentID
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return err
	}
	if pe == nil {
		return refuseSignup(404, "not_found", "no platform experiment %s", platformExpID)
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
		return refuseSignup(409, "signup_closed", "experiment is %s", pe.Status)
	}

	inserted, err := s.store.Signup(ctx, platformExpID, signup)
	if errors.Is(err, db.ErrAgentNameTaken) {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			return fmt.Errorf("platform_experiments.Signup: read agent %s: %v", agentID, err)
		}
		return agentNameTaken(agent.Name)
	}
	if errors.Is(err, db.ErrMaxAgentsReached) {
		return maxAgentsReached(pe.MaxAgents)
	}
	if errors.Is(err, db.ErrMaxAgentSandboxesReached) {
		return refuseSignup(409, "max_agentsandboxes_reached", "%s", maxAgentSandboxesReached(pe.MaxAgentSandboxes))
	}
	// Not found either way: an agent of another org is not this experiment's to learn of.
	if errors.Is(err, db.ErrAgentOutsideOrg) {
		return refuseSignup(404, "not_found", "no agent %s is registered", agentID)
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if !inserted {
		// The insert is guarded on the experiment still being open, so this is either a
		// concurrent signup of the same agent or a start that committed since the reads above.
		already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("platform_experiments.Signup: %w", err)
		}
		if !already {
			return refuseSignup(409, "signup_closed", "experiment started")
		}
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID),
		zap.String("agentID", agentID), zap.String("role", string(signup.Role)))
	return nil
}

// RequireActive refuses a request that starts work or spends for an agent that has no further
// part in the platform experiment: it closed, or the agent's run is over for any other reason. A
// token that is still alive in its wind-down to file summaries does not reopen either.
func (s *Service) RequireActive(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if pe == nil || pe.Status == domain.PlatformExpClosed {
		return refusal.Refuse(http.StatusConflict, platformapi.ReasonExperimentClosed, "platform experiment "+platformExpID+" is closed")
	}
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if signup == nil {
		return nil
	}
	switch signup.Ended {
	case "":
		return nil
	case domain.RunEndClosed:
		return refusal.Refuse(http.StatusConflict, platformapi.ReasonExperimentClosed, "platform experiment "+platformExpID+" is closed")
	case domain.RunEndCut:
		return refusal.Refuse(http.StatusConflict, "stage_cut", "agent "+agentID+" was cut from platform experiment "+platformExpID)
	}
	return refusal.Refuse(http.StatusConflict, "run_over", "agent "+agentID+"'s run in platform experiment "+platformExpID+" is over ("+string(signup.Ended)+"): in its wind-down it files summaries and starts or spends nothing")
}

// notStartable says why the open platform experiment pe, whose roster is signups, is not started
// at now, as a refusal whose reason is what the experiment answers as its status_reason, or nil
// when it is to be started. With no refusal it returns what the researchers share (RunBudget).
func (s *Service) notStartable(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup, now time.Time) (float64, error) {
	// Every researcher starts from the baseline and is ranked against it, so none starts, and
	// no machine is rented for one, before it is measured and declared.
	if pe.BaselineJobID == "" {
		return 0, refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and the platform starts the run only once it has", pe.ID))
	}
	if pe.StartsAt.After(now) {
		return 0, refusal.Refuse(http.StatusConflict, "starts_at_ahead", fmt.Sprintf("platform experiment %s has its baseline and starts at its starts_at, %s", pe.ID, pe.StartsAt.UTC().Format(time.RFC3339)))
	}
	if !slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool { return signup.SharesBudget() }) {
		return 0, noResearchers(pe.ID)
	}
	runBudget, err := s.RunBudget(ctx, pe)
	if err != nil {
		return 0, err
	}
	if runBudget <= 0 {
		return 0, refusal.Refuse(http.StatusConflict, "budget_spent", fmt.Sprintf("measuring the baseline spent the whole budget of %g accelerator hours, so nothing is left for the researchers to share: its owner raises budget_accelerator_hours", pe.BudgetAcceleratorHours))
	}
	return runBudget, nil
}

func noResearchers(id string) error {
	return refusal.Refuse(http.StatusConflict, "no_researchers", fmt.Sprintf("platform experiment %s has its baseline and no researcher to share its budget: add one with `hl platform-experiments agents add --in-agentsandbox --platform-experiment %s` (POST /platform-experiments/%s/agents), and the platform starts the run", id, id, id))
}

// start makes the open pe running and allocates each agent that shares the budget its quota. It
// answers the refusal of notStartable when pe is not to be started.
func (s *Service) start(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) ([]*domain.AgentQuota, error) {
	// Read before the transaction, which must issue no query of its own.
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.start: %w", err)
	}
	runBudget, err := s.notStartable(ctx, pe, signups, now)
	if err != nil {
		return nil, err
	}
	started, quotas, err := s.store.StartPlatformExperimentTx(ctx, pe.ID, s.cfg.AgentsDue, func(participants []db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		if len(participants) == 0 {
			return nil, nil, noResearchers(pe.ID)
		}
		researchers := 0
		for _, p := range participants {
			if p.Role == domain.SignupRoleCompetitor {
				researchers++
			}
		}
		// The ladder is decided here, from the roster the start transaction reads, and stored
		// with the start. Only its first stage's share of the budget is released now; each later
		// stage releases its own at the boundary it starts (see controller.applyCut), which stops
		// one agent exhausting the budget before the ladder has cut anyone.
		stages := domain.RunStages(pe.Stages, researchers)
		exploreFrac := stages[0].ReleaseFrac()
		// Every signed-up agent participates or the start fails. Skipping the ones that could not
		// be resolved used to mean a transient database error permanently disinherited an agent:
		// the experiment started without it, and there is no second allocation pass.
		bonuses := make([]float64, len(participants))
		totalBonusFraction := 0.0
		for i, p := range participants {
			if !p.AgentExists {
				return nil, nil, fmt.Errorf("platform_experiments.start: agent %s is signed up but does not exist", p.AgentID)
			}
			if p.HasTop3 {
				bonuses[i] = s.cfg.Top3BonusFraction
				totalBonusFraction += s.cfg.Top3BonusFraction
			}
		}

		allocated := make([]*domain.AgentQuota, 0, len(participants))
		for i, p := range participants {
			acceleratorGuaranteed, acceleratorBurst := domain.AllocateQuota(
				runBudget*exploreFrac, len(participants), bonuses[i], totalBonusFraction, s.cfg,
			)
			// The tier decides which column the share lands in, never how large it is: everyone
			// is allocated the same way and a burst-only participant's guaranteed part is moved
			// into burst, not taken away.
			acceleratorGuaranteed, acceleratorBurst = domain.ApplyQuotaTier(
				p.QuotaTier, acceleratorGuaranteed, acceleratorBurst)
			id, err := domain.NewID(domain.AgentQuotaIDPrefix)
			if err != nil {
				return nil, nil, fmt.Errorf("platform_experiments.start: %w", err)
			}
			allocated = append(allocated, &domain.AgentQuota{
				ID:                         id,
				AgentID:                    p.AgentID,
				PlatformExperimentID:       pe.ID,
				GuaranteedAcceleratorHours: acceleratorGuaranteed,
				BurstAcceleratorHours:      acceleratorBurst,
				CreatedAt:                  now,
			})
		}
		return allocated, stages, nil
	})
	if err != nil {
		return nil, err
	}
	if !started {
		return nil, invalidTransition("platform experiment %s is no longer open", pe.ID)
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
