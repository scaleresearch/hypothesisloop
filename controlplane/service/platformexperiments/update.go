package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// CoordinatorWanted says when the platform runs a hosted coordinator's machine (db.launcherCoordinatorsWhere).
const CoordinatorWanted = "The coordinator's machine is asked for once the plan is accepted and the experiment is open, so it supervises the baseline and can change the lineup before the start, and is kept until the experiment closes."

// refuseUpdate is a create or a patch the caller can fix.
func refuseUpdate(status int, reason, format string, args ...any) error {
	return refusal.Refuse(status, reason, fmt.Sprintf(format, args...))
}

// Update changes the fields the patch carries and no others, and the experiment as patched has
// to pass validate. While the experiment is open every
// field of the patch can change. Once it runs, only the name, the description, duration_hours and
// max_agents can, and the baseline be raised: admission and the stage ladder have already acted on
// the rest. A new duration_hours moves the end of the run, counted from its start as before. A
// max_agents of a running one changes only the places not taken, and is held to the unspent hours
// of its researchers (seatsHoldTrials). A closed one, and a draft, which its
// planner's proposal fills, take no patch.
//
// While it runs, baseline_job_id with baseline_reason raises the baseline (raisedBaseline).
//
// baseline_job_id declares the baseline. An open experiment that has one, or is given one, is
// held as patched to what that job did (checkBaseline), so its metrics and its budget cannot
// leave the baseline behind before it starts, and its description is given what the job is and
// measured (baselineBlock). Stored with a baseline, it is started at once when nothing else
// is missing (StartIfReady), and answered as that left it.
func (s *PlatformExperimentsService) Update(ctx context.Context, id string, patch PlatformExperimentPatch) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, refuseUpdate(404, "experiment_not_found", "no platform experiment %s", id)
	}
	if len(patch.carried()) == 0 {
		return nil, refuseUpdate(422, "nothing_to_change", "the body names no field: send the fields that change")
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, refuseUpdate(409, "experiment_not_editable", "platform experiment %s is %s: only an open or running one is changed", id, pe.Status)
	}
	if locked := patch.carried("name", "description", "duration_hours", "max_agents", "researchers", "baseline_job_id", "baseline_reason"); pe.Status == domain.PlatformExpRunning && len(locked) > 0 {
		return nil, refuseUpdate(409, "experiment_running", "only name, description, duration_hours, max_agents, researchers and the baseline (baseline_job_id with baseline_reason) change once a platform experiment runs, and the body also carries: %s. Leave them out", strings.Join(locked, ", "))
	}
	raising := pe.Status == domain.PlatformExpRunning && patch.BaselineJobID != nil
	if err := checkRaiseRequest(pe, patch); err != nil {
		return nil, err
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
	if patch.Researchers != nil {
		patched.Researchers = *patch.Researchers
	}
	// Held to the researchers at work in the write that stores it, the count a join is held to: a
	// running one loses the newest above it, and an open one refuses it (db.PoolBelowAtWorkError).
	// Researchers the patch does not state are capped to it there.
	if patch.MaxAgents != nil {
		patched.MaxAgents = *patch.MaxAgents
		if patch.Researchers == nil {
			patched.Researchers = patched.Researchers.Capped(patched.MaxAgents)
		}
		if pe.Status == domain.PlatformExpRunning && patched.MaxAgents > pe.MaxAgents {
			if err := s.seatsHoldTrials(ctx, &patched); err != nil {
				return nil, err
			}
		}
	}
	if patch.Metrics != nil {
		patched.Metrics = *patch.Metrics
	}
	if patch.ReportIntervalSeconds != nil {
		// A create that leaves it out takes the platform's setting. A patch that carries it states it.
		if *patch.ReportIntervalSeconds <= 0 {
			return nil, refuseUpdate(422, "invalid_experiment", "report_interval_seconds must be positive: how often a job really reports. Leave it out to keep %d", pe.ReportIntervalSeconds)
		}
		patched.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.StartsAt != nil {
		patched.StartsAt = domain.ClockStart(*patch.StartsAt, time.Now().UTC())
		patched.BaselineDueAt = patched.StartsAt.Add(pe.BaselineDueAt.Sub(pe.StartsAt))
	}
	if patch.DurationHours != nil {
		patched.DurationHours = *patch.DurationHours
		if pe.Status == domain.PlatformExpRunning {
			end := pe.StartsAt.Add(time.Duration(patched.DurationHours * float64(time.Hour)))
			if !end.After(time.Now()) {
				return nil, refuseUpdate(422, "invalid_length", "duration_hours %g ends the run at %s, which has passed: close it instead, or state a duration that ends later", patched.DurationHours, end.UTC().Format(time.RFC3339))
			}
			if !pe.StartedAt.IsZero() {
				patched.EndsAt = end
			} else {
				patched.EndsAt, patched.AgentsDueAt = end, minTime(pe.AgentsDueAt, end)
			}
		}
	}
	if patch.MaxConcurrentAccelerators != nil {
		patched.MaxConcurrentAccelerators = patch.MaxConcurrentAccelerators
	}
	if patch.BaselineJobID != nil {
		patched.BaselineJobID = *patch.BaselineJobID
	}
	// The block that hands the baseline over is the platform's: a patch of the description
	// neither writes it nor loses it.
	authored, _ := splitMeasuredBaseline(patched.Description)
	_, measured := splitMeasuredBaseline(pe.Description)
	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	checked, err := s.validate(CreatePlatformExperimentRequest{
		Name: patched.Name, Description: authored,
		BudgetAcceleratorHours: patched.BudgetAcceleratorHours, MaxAgents: patched.MaxAgents, Researchers: patched.Researchers,
		MaxConcurrentAccelerators: patched.MaxConcurrentAccelerators, Metrics: patched.Metrics,
		ReportIntervalSeconds: patched.ReportIntervalSeconds, StartsAt: patched.StartsAt, DurationHours: patched.DurationHours,
		Stages: patched.Stages, DefaultQuotaTier: string(patched.DefaultQuotaTier),
	}, catalog)
	if err != nil {
		return nil, err
	}
	patched.Name, patched.Description, patched.Metrics = checked.Name, checked.Description, checked.Metrics
	if patch.Metrics != nil {
		if err := s.requireContinuedMetrics(ctx, pe.Continues, patched.Metrics); err != nil {
			return nil, err
		}
	}
	if pe.Status == domain.PlatformExpOpen && (patch.BaselineJobID != nil || pe.BaselineJobID != "") {
		// The platform declares what its baseline agent measured, held to the agent's own
		// stronger jobs; a person or coordinator that declares chose among them.
		ownJobs := patch.BaselineJobID != nil && patch.by == nil
		if measured, err = s.heldToBaseline(ctx, &patched, ownJobs); err != nil {
			return nil, err
		}
	}
	var raise domain.BaselineRaise
	if raising {
		if raise, measured, err = s.raisedBaseline(ctx, pe, &patched, strings.TrimSpace(*patch.BaselineReason), patch.by); err != nil {
			return nil, err
		}
	}
	patched.Description = withMeasuredBaseline(patched.Description, measured)
	if raising {
		if err := s.store.RaiseBaseline(ctx, &patched, raise); err != nil {
			return nil, fmt.Errorf("platform_experiments.Update: %w", err)
		}
		s.logger.Info("baseline raised", zap.String("platform_experiment_id", pe.ID), zap.String("from", raise.From), zap.String("to", raise.To))
		return s.stored(ctx, pe.ID)
	}
	pe = &patched
	var researchers domain.ResearcherPool
	if patch.Researchers != nil {
		researchers = patched.Researchers
	}
	var below *db.PoolBelowAtWorkError
	switch err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus, researchers); {
	case errors.As(err, &below) && below.Key == (domain.PoolKey{}):
		return nil, refuseUpdate(409, "max_agents_below_researchers", "max_agents %d is below the %d researchers (agents of role competitor) of platform experiment %s, which has not started: state at least %d, or remove researchers first. Once it runs, a lower max_agents cuts the newest of them", below.Count, below.AtWork, pe.ID, below.AtWork)
	case errors.As(err, &below):
		return nil, refuseUpdate(409, "researchers_below_at_work", "%s: a change of researchers takes out no researcher at work, so state at least that many", below)
	case err != nil:
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	if pe.Status != domain.PlatformExpOpen {
		return pe, nil
	}
	if pe.BaselineJobID == "" {
		// The change may be what a job its baseline agent left was refused for: a budget raised
		// to hold a trial apiece, a metric taken out.
		if err := s.DeclareMeasured(ctx, pe.ID); err != nil {
			return nil, err
		}
		return s.stored(ctx, pe.ID)
	}
	// Nothing but the baseline may have been missing: the run starts in the request that
	// declared it, and one that still waits says for what in its status_reason.
	if err := s.StartIfReady(ctx, pe.ID); err != nil {
		return nil, err
	}
	return s.stored(ctx, pe.ID)
}

// seatsHoldTrials refuses a max_agents raised on the running pe when what its researchers left
// unspent, split evenly over max_agents researchers, would not hold one trial of the baseline
// each: the share a researcher that joins is given (domain.LateShare) would not run one.
func (s *PlatformExperimentsService) seatsHoldTrials(ctx context.Context, pe *domain.PlatformExperiment) error {
	held, err := s.runningTrialHeld(ctx, pe)
	if err != nil || held == 0 {
		return err
	}
	quotas, err := s.researcherQuotas(ctx, pe)
	if err != nil {
		return fmt.Errorf("platform_experiments.Update: %w", err)
	}
	unspent := 0.0
	for _, quota := range quotas {
		unspent += quota.Unspent()
	}
	if each := unspent / float64(pe.MaxAgents); each < held {
		return refuseUpdate(422, "budget_below_baseline", "a trial holds %.3f accelerator-hours of its researcher's share (%g h, the estimate measured on the baseline job), and the researchers left %.3f unspent, %.3f for each of %d: state a max_agents that leaves each at least one trial", held, pe.JobEstimatedDurationHours, unspent, each, pe.MaxAgents)
	}
	return nil
}

// SetSummary records the operator's narrative verdict on a run and answers the platform
// experiment with it. Unlike every other field, this is writable after the experiment closes: a
// summary only exists once the run is over. The platform's record of the close is kept beside it
// and answered after it, so a copy of the record read back with the summary is dropped.
func (s *PlatformExperimentsService) SetSummary(ctx context.Context, id, summary string) (*domain.PlatformExperiment, error) {
	if i := strings.Index(summary, closeRecordHeading); i >= 0 {
		summary = strings.TrimRight(summary[:i], " \n")
	}
	if len(summary) > domain.MaxProseBytes {
		return nil, refuseUpdate(422, "summary_too_long", "%s", tooLong("summary", len(summary)))
	}
	err := s.store.SetPlatformExperimentSummary(ctx, id, summary)
	if errors.Is(err, db.ErrPlatformExperimentNotFound) {
		return nil, noPlatformExperiment(id)
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.SetSummary: %w", err)
	}
	return s.stored(ctx, id)
}
