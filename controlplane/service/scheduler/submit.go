package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// unestimatedLimit bounds the estimate of a baseline agent's job that states none: the stage's
// per-job limit and, while the platform experiment is open, the time left until baseline_due_at.
// Sized by the quota alone, a control was held at once as baseline_late, two hours past the due
// time, and its agent cancelled it to submit it again with an estimate of its own. 0 is no bound.
func unestimatedLimit(pe *domain.PlatformExperiment, now time.Time) float64 {
	limit := pe.CurrentMaxJobHours()
	if pe.Status != domain.PlatformExpOpen || pe.BaselineDueAt.IsZero() {
		return limit
	}
	left := math.Floor(pe.BaselineDueAt.Sub(now).Hours()*1e4) / 1e4
	if left <= 0 || (limit > 0 && limit <= left) {
		return limit
	}
	return left
}

// ErrNoCaller is a request that reached the service with no authenticated caller: every route
// is behind the token check, so it is the platform's fault and never the submitter's.
var ErrNoCaller = errors.New("scheduler: no authenticated caller in the request context")

// Submit runs the admission gate for an experiment. On success the experiment is
// transitioned to QUEUED. On rejection an *AdmissionError is returned.
func (s *Service) Submit(ctx context.Context, exp *domain.Experiment) error {
	// secret_env leaves the spec before anything reads, stores or answers with it.
	exp.SecretEnv, exp.Job.SecretEnv = exp.Job.SecretEnv, nil

	// 1. The platform experiment and its state, before the spec: a job sent before the start is
	// told so at once, whatever else is wrong with it.
	if exp.PlatformExperimentID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "platform_experiment_id is required"}
	}
	pe, err := s.store.GetPlatformExperiment(ctx, exp.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("scheduler: get platform experiment: %w", err)
	}
	if pe == nil {
		return &AdmissionError{
			Reason:  "experiment_not_found",
			Message: fmt.Sprintf("platform experiment %s not found", exp.PlatformExperimentID),
		}
	}
	signup, err := s.store.GetAgentSignup(ctx, pe.ID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: read signup: %w", err)
	}
	measuresBaseline := signup != nil && signup.Role == domain.SignupRoleBaseline
	switch pe.Status {
	case domain.PlatformExpRunning:
	// Before the start the one job there is to run is the baseline's, which every researcher
	// starts from and is ranked against. Its agent holds a quota from the moment it joined.
	case domain.PlatformExpOpen:
		if !measuresBaseline {
			// What the caller does next is the platform experiment's status_reason, worded for
			// whoever asks, and is said there alone.
			reader, ok := auth.FromContext(ctx)
			if !ok {
				return ErrNoCaller
			}
			if err := s.quota.Describe(ctx, pe, reader); err != nil {
				return fmt.Errorf("scheduler: describe platform experiment: %w", err)
			}
			return &AdmissionError{
				Reason:  "experiment_not_started",
				Message: fmt.Sprintf("platform experiment %s has not started (%s). %s", pe.ID, pe.StatusReason.Reason, pe.StatusReason.Message),
			}
		}
	case domain.PlatformExpDraft:
		return &AdmissionError{
			Reason:  "experiment_not_started",
			Message: fmt.Sprintf("platform experiment %s has not started; a draft accepts no job", pe.ID),
		}
	default:
		return &AdmissionError{
			Reason:  "experiment_closed",
			Message: fmt.Sprintf("platform experiment %s is over; jobs are accepted only while it is running", pe.ID),
		}
	}

	// The platform closes it at this moment (SweepExpired) and stops what runs then, so a job sent
	// after it would be admitted only to be evicted.
	if closesAt := pe.ClosesAt(); !closesAt.IsZero() && !closesAt.After(time.Now()) {
		return &AdmissionError{
			Reason:  "experiment_closed",
			Message: fmt.Sprintf("platform experiment %s ended at %s; jobs are accepted only before its end", pe.ID, closesAt.UTC().Format(time.RFC3339)),
		}
	}

	// With no estimate stated and none measured there is nothing to hold of the agent's quota. The
	// baseline's job is the one nothing was measured before: it is sized below, once its units are known.
	if !exp.TakeEstimate(pe) && !measuresBaseline {
		return &AdmissionError{
			Reason:  ReasonEstimateRequired,
			Message: fmt.Sprintf("the job states no estimated_duration_hours and platform experiment %s has no measured one (its job_estimated_duration_hours, read from its baseline job): state the hours it is expected to run with hl job submit --estimated-duration-hours H", pe.ID),
		}
	}

	// The baseline is the objective's own measurement and no claim: its job tests no hypothesis
	// and is kept on the objective, which the platform experiment's id names.
	if measuresBaseline {
		if exp.HypothesisID != "" {
			return &AdmissionError{Reason: ReasonBaselineTestsNoHypothesis, Message: fmt.Sprintf("agent %s measures the baseline, and the baseline is the objective's own: its job names no hypothesis. Submit it without one", exp.AgentID)}
		}
		exp.HypothesisID = pe.ID
	}

	// 2. Structural validation.
	if err := ValidateExperiment(exp, s.credits); err != nil {
		return err
	}
	if err := s.codeInRepository(pe, exp.CodeRef); err != nil {
		return err
	}
	if err := branchOfExperiment(exp, measuresBaseline); err != nil {
		return err
	}
	if err := s.resolveImage(&exp.Job); err != nil {
		return err
	}
	if exp.OrgID, err = s.store.ProjectOrg(ctx, pe.ProjectID); err != nil {
		return fmt.Errorf("scheduler: org of platform experiment: %w", err)
	}
	if err := s.runsAsStated(ctx, exp); err != nil {
		return err
	}

	if signup == nil {
		return &AdmissionError{
			Reason:  "not_signed_up",
			Message: "agent has not signed up for this platform experiment",
		}
	}

	cut, err := s.store.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: check stage cut: %w", err)
	}
	if cut {
		return &AdmissionError{
			Reason:  "agent_held",
			Message: "agent was cut at a stage boundary and cannot submit new jobs for the rest of this platform experiment — check GET /platform-experiments/{id}/stages",
		}
	}

	finished, err := s.store.IsAgentFinished(ctx, exp.PlatformExperimentID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: check finished: %w", err)
	}
	if finished {
		return &AdmissionError{
			Reason:  "agent_finished",
			Message: "agent finished in this platform experiment (POST /platform-experiments/{id}/agents/{agent_id}/finish), which is final: it submits no further job. Its token reads for two more minutes and then stops working",
		}
	}

	if err := s.declaredEnvironment(ctx, pe, exp); err != nil {
		return err
	}

	// 3ab. Durable-data ceiling.
	if err := s.enforceDataCeiling(ctx, exp); err != nil {
		return err
	}

	// A baseline agent's job that states no estimate is sized by the admission transaction, from
	// the figure it admits on: the largest the agent's quota holds, within unestimatedLimit.
	if exp.EstimatedDurationHours == 0 {
		exp.MaxEstimateHours = unestimatedLimit(pe, time.Now())
	}

	// 3aa. Stage job-length cap. Rejecting at submit is the honest half of the gate — the agent
	// learns the limit before spending anything. The controller enforces the other half against
	// observed runtime, since estimated_duration_hours is a claim, not a guarantee.
	if maxHours := pe.CurrentMaxJobHours(); maxHours > 0 && exp.EstimatedDurationHours > maxHours {
		return &AdmissionError{
			Reason:  ReasonJobTooLong,
			Message: fmt.Sprintf("estimated_duration_hours %g exceeds the %g h per-job limit of stage %d — split the work into shorter runs, or wait for a later stage (see GET /platform-experiments/{id}/stages)", exp.EstimatedDurationHours, maxHours, pe.CurrentStage),
		}
	}

	now := time.Now().UTC()
	exp.CreatedAt, exp.UpdatedAt = now, now

	// 2b. Every job but the baseline's tests a hypothesis registered before it (POST /hypotheses),
	// whose text is kept on the job for cheap reads.
	if !measuresBaseline {
		if err := s.bindHypothesis(ctx, exp); err != nil {
			return err
		}
		if err := s.refuseRepeats(ctx, pe, exp); err != nil {
			return err
		}
	}

	// 4. What the job holds of its agent's quota until it ends.
	held, err := exp.EstimateHeld()
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	exp.EstimatedCostAccH = held

	// 5. Novelty detection: compare against running and queued experiments.
	// This is advisory — low novelty is NOT a hard rejection. The score is stored on the
	// experiment so agents can see it and decide whether to refine. Agents should proactively
	// check GET /experiments?status=QUEUED to spot duplicates before submitting.
	// Within this platform experiment only: a hypothesis belongs to one, so a job elsewhere
	// tests nothing this one could duplicate, and may be another org's.
	activeExps, err := s.store.GetRunningAndQueued(ctx, exp.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("scheduler: get running and queued: %w", err)
	}
	// Never against itself. Re-submitting an already-QUEUED job (step 7's `existing` branch)
	// otherwise finds its own row in the active set, scores itself a duplicate of itself, and
	// persists the depressed priority that follows — reprioritize excludes it for the same
	// reason.
	// 5a. Another agent's work on the same hypothesis: the submitter is told in the answer, and
	// whoever steers the run by job.duplicate, written with the row.
	if !measuresBaseline {
		if exp.SameHypothesis, err = s.sameHypothesis(ctx, exp, activeExps); err != nil {
			return err
		}
	}
	noveltyScore := novelty(exp.HypothesisID, excludeExperiment(activeExps, exp.ID))
	exp.NoveltyScore = noveltyScore

	// 6. Priority scoring.
	priorityScore, err := s.computePriority(ctx, exp, noveltyScore)
	if err != nil {
		return fmt.Errorf("scheduler: compute priority: %w", err)
	}
	exp.PriorityScore = priorityScore

	// 7. Persist. One transaction decides everything: whether this id is new, already QUEUED, or
	// taken, and whether the aggregate desired estimates plus observed settled usage leave room
	// for it. The id check used to run before the transaction, which made it non-atomic with the
	// insert and left the primary key as a second, competing decider that surfaced as a 500.
	exp.Status = domain.StatusQueued
	// ClusterName is intentionally left unset here: the admission loop assigns it, capacity-aware,
	// at the moment this job is actually admitted onto a specific cluster (see loop_tick.go).
	// The submission rate limit is enforced inside this transaction rather than before it: the
	// per-(agent, platform experiment) advisory lock it takes is exactly the scope the limit is
	// expressed in, so N concurrent submissions can no longer all read the same pre-limit count
	// and all pass.
	rateLimit := db.SubmissionRateLimit{
		MaxPerHour: s.credits.MaxSubmissionsPerHour,
		Window:     submissionRateLimitWindow,
	}
	// The job's one permanent store key. It is drawn here and written with the row, and the store
	// is asked to create it only once the transaction has decided to insert: a store outage then
	// refuses this submission alone and never reaches the cluster reconcile answer.
	dataKey, err := objectstore.NewDataKey()
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	exp.DataKey = dataKey
	decision, err := s.quota.AdmitExperiment(ctx, exp, rateLimit, func(ctx context.Context) error {
		return s.dataStore.CreateDataKey(ctx, dataKey, exp.PlatformExperimentID, exp.AgentID, exp.ID)
	})
	var refusal *db.AdmitRefusal
	switch {
	case errors.As(err, &refusal) && refusal.Reason == ReasonInsufficientCredits:
		return &AdmissionError{Reason: refusal.Reason, Message: refusal.Message + " " + shareOutlook(pe)}
	case errors.As(err, &refusal):
		return &AdmissionError{Reason: refusal.Reason, Message: refusal.Message}
	// The transaction found the id taken, or lost the primary key to another agent racing the
	// same id. Either way it is a duplicate, not a server fault.
	case errors.Is(err, db.ErrDuplicateExperiment):
		return &AdmissionError{Reason: ReasonDuplicate, Message: err.Error()}
	case err != nil:
		return fmt.Errorf("scheduler: atomically admit experiment: %w", err)
	}

	// A submit repeated with its key is answered with the job the first one created, as it is
	// now, and changes nothing.
	if decision == db.AdmitRepeated {
		stored, err := s.store.GetExperiment(ctx, exp.ID)
		if err != nil {
			return fmt.Errorf("scheduler: read the job of a repeated submit: %w", err)
		}
		if stored == nil {
			return fmt.Errorf("scheduler: job %s of a repeated submit is gone", exp.ID)
		}
		*exp = *stored
		return nil
	}

	// Re-submitting a job that is already QUEUED is legal and only refreshes its priority: it is
	// still waiting for capacity, so there is nothing to insert and nothing to debit.
	if decision == db.AdmitAlreadyQueued {
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, priorityScore); err != nil {
			return fmt.Errorf("scheduler: update priority: %w", err)
		}
	}

	// The row keeps why a job waits as "code: sentence", and an answer states the two apart.
	exp.NotAdmittedReason, exp.NotAdmittedMessage = domain.SplitQueueReason(exp.NotAdmittedReason)

	// 7a. The run's best, where this job runs another commit: the submitter's checkout says
	// whether its commit contains it, and a failed read leaves it out of the answer and the job
	// queued.
	if !measuresBaseline {
		if exp.RunBest, err = s.runBest(ctx, pe, exp); err != nil {
			s.logger.Warn("scheduler: the run's best for a submit's answer", zap.String("experiment_id", exp.ID), zap.Error(err))
		}
	}

	// 8. Wake the scheduler loop.
	s.loop.Trigger()
	return nil
}

// runBest is the competitors' best job of pe on its first ranking metric, as the standings rank
// it, ties to the lowest agent id: nil when there is none or it ran exp's commit.
func (s *Service) runBest(ctx context.Context, pe *domain.PlatformExperiment, exp *domain.Experiment) (*domain.RunBest, error) {
	metric, ok := domain.PrimaryRankingMetric(pe.Metrics)
	if !ok {
		return nil, nil
	}
	completed, err := s.store.CompletedJobIDs(ctx, pe.ID)
	if err != nil {
		return nil, err
	}
	best, _, err := s.standings.BestPerAgentOnMetric(ctx, pe, metric, completed)
	if err != nil {
		return nil, err
	}
	competitors, err := s.store.ListSignupsByRole(ctx, pe.ID, domain.SignupRoleCompetitor)
	if err != nil {
		return nil, err
	}
	slices.Sort(competitors)
	var top *domain.RunBest
	for _, agentID := range competitors {
		if b, ok := best[agentID]; ok && (top == nil || metric.Better(b.Value, top.Value)) {
			top = &domain.RunBest{JobID: b.ExperimentID, AgentID: agentID, Metric: metric.Key, Value: b.Value}
		}
	}
	if top == nil {
		return nil, nil
	}
	job, err := s.store.GetExperiment(ctx, top.JobID)
	if err != nil {
		return nil, err
	}
	if job == nil || job.CodeRef == "" || job.CodeRef == exp.CodeRef {
		return nil, nil
	}
	top.CodeRef, top.Branch = job.CodeRef, job.Branch
	return top, nil
}
