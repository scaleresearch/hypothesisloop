package platformexperiments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// Spend sets on pe what it has spent of its budget (ConsumedAcceleratorHours). A draft runs no
// job and has none.
func (s *Service) Spend(ctx context.Context, pe *domain.PlatformExperiment) error {
	if pe.Status == domain.PlatformExpDraft {
		return nil
	}
	consumed, err := s.metrics.TotalObservedAccH(ctx, pe.CreatedAt, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.Spend: %w", err)
	}
	pe.ConsumedAcceleratorHours = &consumed
	return nil
}

// spendEach is Spend of each of pes, in one read of the metrics store.
func (s *Service) spendEach(ctx context.Context, pes []*domain.PlatformExperiment) error {
	var ids []string
	var earliest time.Time
	for _, pe := range pes {
		if pe.Status == domain.PlatformExpDraft {
			continue
		}
		ids = append(ids, pe.ID)
		if earliest.IsZero() || pe.CreatedAt.Before(earliest) {
			earliest = pe.CreatedAt
		}
	}
	if len(ids) == 0 {
		return nil
	}
	spent, err := s.metrics.TotalObservedAccHOf(ctx, earliest, ids)
	if err != nil {
		return fmt.Errorf("platform_experiments.spendEach: %w", err)
	}
	for _, pe := range pes {
		if pe.Status != domain.PlatformExpDraft {
			consumed := spent[pe.ID]
			pe.ConsumedAcceleratorHours = &consumed
		}
	}
	return nil
}

// budgetKinds sets on each of pes what its budget is spent on (BudgetKind): the kind of the type
// its proposal was sized for, and without a proposal cores only once it has jobs and runs every
// one of them on cores.
func (s *Service) budgetKinds(ctx context.Context, pes []*domain.PlatformExperiment) error {
	var unproposed []string
	for _, pe := range pes {
		if len(pe.Proposal) == 0 {
			unproposed = append(unproposed, pe.ID)
			continue
		}
		var proposal Proposal
		if err := json.Unmarshal(pe.Proposal, &proposal); err != nil {
			return fmt.Errorf("platform_experiments.budgetKinds: the proposal of %s: %w", pe.ID, err)
		}
		pe.BudgetKind = proposal.AcceleratorType.Kind()
	}
	onCores, err := s.store.OnCoresOnly(ctx, unproposed)
	if err != nil {
		return fmt.Errorf("platform_experiments.budgetKinds: %w", err)
	}
	for _, pe := range pes {
		if len(pe.Proposal) > 0 {
			continue
		}
		pe.BudgetKind = domain.TypeKindAccelerator
		if onCores[pe.ID] {
			pe.BudgetKind = domain.TypeKindCPU
		}
	}
	return nil
}

// draftReason is where a draft stands with its planner.
func draftReason(pe *domain.PlatformExperiment) *domain.StatusReason {
	switch {
	case pe.DeclinedAt != nil:
		return &domain.StatusReason{Reason: "declined", Message: "its planner proposed no experiment, for declined_reason: change its goal with PATCH /platform-experiments/" + pe.ID + ", saying what the planner lacked, or close it. The platform closes it at proposal_expires_at"}
	case pe.ProposedAt != nil && pe.AcceptsAt != nil:
		return &domain.StatusReason{Reason: "accepting", Message: "its planner proposed an experiment, in proposal, and the platform accepts it as proposed at accepts_at, with its baseline agent's machine starting now. Accept it sooner with POST /platform-experiments/" + pe.ID + "/accept, hold it to edit it with POST /platform-experiments/" + pe.ID + "/hold, change its goal or close it"}
	case pe.ProposedAt != nil:
		return &domain.StatusReason{Reason: "proposed", Message: "its planner proposed an experiment, in proposal: accept it with POST /platform-experiments/" + pe.ID + "/accept, with answers to proposal.questions where it has any, change its goal or close it. The platform closes it at proposal_expires_at"}
	case pe.GoalVersion > 1:
		return &domain.StatusReason{Reason: "revising", Message: "its planner is planning again from the changed goal and has until planning_ends_at"}
	}
	return &domain.StatusReason{Reason: "planning", Message: "its planner is working out how to measure the goal and has until planning_ends_at"}
}

// The commands a status_reason names are whole as a person types them, who coordinates where the
// platform hosts no coordinator and whose token names no platform experiment.
func addBaselineAgent(id string) string {
	return "`hl platform-experiments agents add --in-agentsandbox --role baseline --platform-experiment " + id + "`"
}

func declareBaseline(id string) string {
	return "`hl platform-experiments update --baseline-job JOB --platform-experiment " + id + "`, which writes the job, its code_ref and what it measured into the description and starts the run"
}

// waiting is why the open pe, whose roster is signups, is not running yet: the reason and the
// sentence its status_reason and its roster answer with.
func (s *Service) waiting(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*domain.StatusReason, error) {
	_, err := s.notStartable(ctx, pe, signups, time.Now().UTC())
	var refused *refusal.Refusal
	switch {
	case errors.As(err, &refused) && pe.BaselineJobID == "":
		// Without a baseline the reason is where the measuring stands and whose move it is.
		if refused, err = s.undeclared(ctx, pe, signups); err != nil {
			return nil, err
		}
		return &domain.StatusReason{Reason: refused.Reason, Message: refused.Message}, nil
	case errors.As(err, &refused):
		return &domain.StatusReason{Reason: refused.Reason, Message: refused.Message}, nil
	case err != nil:
		return nil, err
	}
	return &domain.StatusReason{Reason: "starting", Message: "nothing is missing and the platform starts the run within a minute"}, nil
}

// nextStep is what an agent that reads an open pe at now does while it waits for reason, a
// sentence that ends in a space, or nothing when all it can do is wait. The reason's own sentence
// says what everyone else does. Every agent is told the run's clock with it, and an agent of role
// baseline its own.
func nextStep(pe *domain.PlatformExperiment, reason string, signups []domain.AgentSignup, reader auth.Principal, now time.Time) string {
	if reader.Kind != auth.KindAgent {
		return ""
	}
	own := slices.IndexFunc(signups, func(signup domain.AgentSignup) bool { return signup.AgentID == reader.AgentID })
	if own < 0 || signups[own].Role != domain.SignupRoleBaseline {
		return "Every job you submit is refused until the platform starts the run: prepare, then end your turn, and the start arrives as an update. " + runClock(pe)
	}
	step := ""
	if signups[own].FinishedAt == nil && reason == "baseline_measuring" {
		step = "Your job is admitted now: measure the baseline with one job (hl job submit), file its summary (hl job summary), then hl platform-experiments agents finish --baseline JOB, and the platform declares that job. Your quota is half the budget, for the measurement and any rerun of it (hl quota). "
	}
	return step + baselineClock(pe.BaselineDueAt, now) + runClock(pe)
}

// runClock is how long the run of an open pe lasts and when that counts from.
func runClock(pe *domain.PlatformExperiment) string {
	return fmt.Sprintf("The run lasts its duration_hours, %g, from the moment the platform starts it, once its baseline is declared: starts_at and ends_at are set then. ", pe.DurationHours)
}

// endsClock is when a running pe ends, and how long that is from now.
func endsClock(pe *domain.PlatformExperiment, now time.Time) string {
	return fmt.Sprintf("ends_at, %s, %d minutes from now", pe.EndsAt.UTC().Format(time.RFC3339), max(0, int(pe.EndsAt.Sub(now).Minutes())))
}

// lateBaselineJobs says of each of jobs, the jobs of an open platform experiment's baseline
// agents, that is late (lateBaselineJob): what its agent finishes on. Nothing when there is none.
func lateBaselineJobs(jobs []*domain.Experiment, due, now time.Time) string {
	var late []string
	for _, job := range jobs {
		if sentence := lateBaselineJob(job, due, now); sentence != "" {
			late = append(late, sentence)
		}
	}
	return strings.Join(late, "; ")
}

// lateBaselineJob says that job, a job of an open platform experiment's baseline agents, has not
// started running and, run from now for the estimated_duration_hours its agent stated, would end
// after due. Nothing when it would not.
func lateBaselineJob(job *domain.Experiment, due, now time.Time) string {
	if !slices.Contains(domain.UnstartedStatuses, job.Status) || job.EstimatedDurationHours <= 0 {
		return ""
	}
	runs := time.Duration(job.EstimatedDurationHours * float64(time.Hour))
	if ends := now.Add(runs); !ends.After(due) {
		return ""
	}
	state := string(job.Status) + ", not running yet"
	if job.NotAdmittedReason != "" {
		state = string(job.Status) + " (" + job.NotAdmittedReason + ")"
	}
	return fmt.Sprintf("job %s is %s and its estimated_duration_hours is %g: started now it ends after baseline_due_at, %d minutes from now, so baseline_due_at cannot be met by it",
		job.ID, state, job.EstimatedDurationHours, max(0, int(due.Sub(now).Minutes())))
}

// waitsOnPlatform says job, a job of an open platform experiment's baseline, waits on the
// platform: placed on its cluster and not running yet, which includes pulling its image.
func waitsOnPlatform(job *domain.Experiment) bool {
	return job.Status == domain.StatusSubmitted
}

// baselineClock says when an open platform experiment whose baseline is due at due closes, how
// long that is from now, and what moves it.
func baselineClock(due, now time.Time) string {
	return fmt.Sprintf("baseline_due_at is %s, %d minutes from now: the platform closes it then if the run has not started. While a job of the baseline waits for the platform to start it on its cluster, baseline_due_at moves out by that time, at most %d minutes in all. ",
		due.UTC().Format(time.RFC3339), max(0, int(due.Sub(now).Minutes())), int(domain.MaxBaselineHold.Minutes()))
}

// executorLeft says an executor of signups may still work: one that neither finished nor was
// cut, and whose machine, where the platform runs it, did not stop with no restart left.
func executorLeft(signups []domain.AgentSignup) bool {
	return slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool {
		stopped := signup.InAgentSandbox && signup.Instance != nil && signup.Instance.Stopped
		return signup.Role == domain.SignupRoleCompetitor && signup.Ended == "" && !stopped
	})
}

// runningReason is where a running pe, whose roster is signups, stands with its clock and with
// the executors that work in it.
func runningReason(pe *domain.PlatformExperiment, signups []domain.AgentSignup, now time.Time) *domain.StatusReason {
	switch {
	case pe.AwaitsAgents():
		return &domain.StatusReason{Reason: "agents_starting", Message: "the run started at starts_at, " + pe.StartsAt.UTC().Format(time.RFC3339) + ", and its executors' machines are starting: the first one running sets started_at. The platform closes it at agents_due_at, with close_reason " + string(domain.CloseAgentsNotStarted) + ", if none is running by then, and at " + endsClock(pe, now) + ", in any case"}
	case !executorLeft(signups):
		return &domain.StatusReason{Reason: "executors_done", Message: fmt.Sprintf("every executor of platform experiment %s has finished, was cut or lost its machine with no restart left, so nobody is left to submit a job. Where it runs them the platform adds an executor of its agents in the place of one that finished or lost its machine while the run has time for a trial and its budget a share for one, and none is added now; its coordinator may add one up to max_agents with `hl platform-experiments agents add --in-agentsandbox --platform-experiment %s`. The platform closes it at its %s, and its coordinator may write the summary with `hl platform-experiments summary --file FILE --platform-experiment %s` and close it sooner with `hl platform-experiments close --platform-experiment %s`",
			pe.ID, pe.ID, endsClock(pe, now), pe.ID, pe.ID)}
	}
	return &domain.StatusReason{Reason: "running", Message: "its executors are at work and the platform closes it at " + endsClock(pe, now)}
}

// closedReason is why the closed pe closed: its close_reason and what that means. Of one closed
// before its run started for want of a baseline, it says whether a job was submitted for one.
func (s *Service) closedReason(ctx context.Context, pe *domain.PlatformExperiment) (*domain.StatusReason, error) {
	closer := "the owner"
	if (pe.CloseReason == domain.CloseByRequest || pe.CloseReason == domain.CloseRemoved) && pe.ClosedBy != nil {
		if err := s.store.NameActors(ctx, []*domain.Actor{pe.ClosedBy}); err != nil {
			return nil, fmt.Errorf("platform_experiments.closedReason: %w", err)
		}
		closer = pe.ClosedBy.Name
	}
	how := map[domain.CloseReason]string{
		domain.CloseEndTimeReached:       "the run reached its ends_at and the platform closed it",
		domain.CloseByRequest:            closer + " asked for it to be closed",
		domain.CloseRemoved:              closer + " removed it",
		domain.ClosePlanningLimitReached: "its planner answered nothing by planning_ends_at and the platform closed the draft",
		domain.CloseProposalExpired:      "no person accepted its planner's answer or changed its goal by proposal_expires_at and the platform closed the draft",
		domain.ClosePlannerStopped:       "its planner's machine ended before it answered and the platform had restarted it as often as it restarts one, so it closed the draft: the planner's instance in the roster says how the last machine ended (end_reason, exit_log)",
		domain.CloseAgentsNotStarted:     "it started and no executor's machine was running by agents_due_at, so the platform closed it. Each agent's instance says why its machine did not come up",
		domain.CloseRepositoryNotCreated: "the platform could not create its repository, so none of its machines started. The git host answered: " + pe.RepositoryError,
	}
	if pe.CloseReason == domain.CloseBaselineLimitReached {
		submitted, err := s.jobsSubmitted(ctx, pe.ID)
		if err != nil {
			return nil, err
		}
		how[pe.CloseReason] = baselineLimitReached(pe, submitted)
	}
	if pe.CloseMessage != "" {
		how[domain.CloseByRequest] += `, saying "` + strings.Join(strings.Fields(pe.CloseMessage), " ") + `"`
	}
	if sentence, known := how[pe.CloseReason]; known {
		return &domain.StatusReason{Reason: string(pe.CloseReason), Message: sentence}, nil
	}
	return &domain.StatusReason{Reason: "closed", Message: "it closed before the platform kept why"}, nil
}

// baselineLimitReached is why pe, closed at its baseline_due_at with submitted jobs, never ran.
// An open platform experiment admits the jobs of its baseline agents alone.
func baselineLimitReached(pe *domain.PlatformExperiment, submitted int) string {
	const closed = "it was not running by baseline_due_at and the platform closed it: "
	switch {
	case pe.BaselineJobID != "":
		return closed + "its baseline, job " + pe.BaselineJobID + ", was declared and the start still waited for an executor, or for budget left to share among them"
	case submitted == 0:
		return closed + "no job was ever submitted to measure its baseline"
	}
	return closed + fmt.Sprintf("%d jobs were submitted to measure its baseline and none was declared: the roster says how each baseline agent ended", submitted)
}

// jobsSubmitted is how many jobs the platform experiment id took, by every agent.
func (s *Service) jobsSubmitted(ctx context.Context, id string) (int, error) {
	jobs, err := s.store.AgentJobsByAgent(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments.jobsSubmitted: %w", err)
	}
	total := 0
	for _, agent := range jobs {
		total += agent.Submitted
	}
	return total, nil
}

// Describe sets on pe why it is in its status (StatusReason): on a draft where it stands with its
// planner, on an open one why it is not running yet and what reader does about it, on a running
// one whether its clock runs and whether an executor is left to work, and on a closed one why it closed. It is the one place that says so.
func (s *Service) Describe(ctx context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error {
	if err := s.budgetKinds(ctx, []*domain.PlatformExperiment{pe}); err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	var why *domain.StatusReason
	var signups []domain.AgentSignup
	var err error
	if pe.Status == domain.PlatformExpClosed {
		if why, err = s.closedReason(ctx, pe); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
		if err := summarize(why, pe, nil, time.Now()); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
		pe.StatusReason = why
		return nil
	}
	// A draft's planner has a machine only while it plans.
	if pe.Status != domain.PlatformExpDraft || !pe.Answered() {
		if signups, err = s.store.ListAgentSignups(ctx, pe.ID); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
	}
	now := time.Now().UTC()
	switch {
	case pe.AwaitsRepository():
		why = &domain.StatusReason{Reason: "repository_creating", Message: "the platform is creating its repository on the git host, and none of its machines starts before the repository is there"}
	case pe.Status == domain.PlatformExpDraft:
		why = draftReason(pe)
	case pe.Status == domain.PlatformExpRunning:
		why = runningReason(pe, signups, now)
	default:
		if why, err = s.waiting(ctx, pe, signups); err != nil {
			return err
		}
		if why.Reason == "baseline_measuring" {
			jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
			if err != nil {
				return fmt.Errorf("platform_experiments.Describe: %w", err)
			}
			if late := lateBaselineJobs(jobs, pe.BaselineDueAt, now); late != "" {
				why.Message += ". " + late
			}
		}
		why.Message = nextStep(pe, why.Reason, signups, reader, now) + why.Message
	}
	if err := summarize(why, pe, signups, now); err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	if why.WaitsFor, err = domain.WaitsFor(why.Reason, pe.CoordinatorInAgentSandbox); err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	pe.StatusReason = why
	return nil
}
