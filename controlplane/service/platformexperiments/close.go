package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// Close transitions a platform experiment that is not closed to closed, records top-3
// placements and answers it as closed. by is whose credential asked for it, nil when the
// platform closes it at its end time, and message what that caller said of why.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries). When it is
// empty the standings are derived from the metrics store instead: closing must never silently
// discard the outcome of a run just because the caller was a timer rather than a person.
func (s *Service) Close(ctx context.Context, id string, by *domain.Actor, message string, topResults []AgentResult) (*domain.PlatformExperiment, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := closes(pe); err != nil {
		return nil, err
	}
	return s.close(ctx, pe, topResults, func() error {
		return s.store.ClosePlatformExperiment(ctx, id, by, domain.EventDetail(message))
	})
}

// Remove removes platform experiment id for by: it is answered to nobody from then on, and
// everything it holds is kept, its spend included. One not closed yet is closed as Close closes
// it, its agents stopped and its jobs evicted, with close_reason removed, in the write that
// removes it.
func (s *Service) Remove(ctx context.Context, id string, by domain.Actor) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	remove := func() error {
		err := s.store.RemovePlatformExperiment(ctx, id, by)
		if errors.Is(err, db.ErrPlatformExperimentNotFound) {
			return noPlatformExperiment(id)
		}
		return err
	}
	if pe.Status == domain.PlatformExpClosed {
		err = remove()
	} else {
		_, err = s.close(ctx, pe, nil, remove)
	}
	if err != nil {
		return err
	}
	s.logger.Info("platform experiment removed", zap.String("platform_experiment_id", id))
	return nil
}

// close records the standings and the agent lines of pe, which is not closed, runs write, which
// closes it, and writes the record of the close once its every job settled.
func (s *Service) close(ctx context.Context, pe *domain.PlatformExperiment, topResults []AgentResult, write func() error) (*domain.PlatformExperiment, error) {
	id := pe.ID
	var err error
	// A draft never ran: closing it abandons the plan, and there are no standings to record.
	if pe.Status != domain.PlatformExpDraft {
		if len(topResults) == 0 {
			// Standings are what closing produces. Closing is irreversible, so deriving them failing
			// has to stop the close and be retried, not quietly close an experiment with no result.
			if topResults, err = s.derivedTopResults(ctx, id); err != nil {
				return nil, fmt.Errorf("close: derive standings for %s: %w", id, err)
			}
		}
		for i, r := range topResults {
			if i >= 3 {
				break
			}
			if err := s.store.RecordTop3(ctx, id, r.AgentID, r.FinalMetric); err != nil {
				return nil, fmt.Errorf("close: record placement for %s: %w", r.AgentID, err)
			}
		}
	}
	if pe.Status != domain.PlatformExpDraft {
		lines, err := s.closeAgents(ctx, pe, time.Now())
		if err != nil {
			return nil, fmt.Errorf("close: %w", err)
		}
		if err := s.store.SetCloseAgents(ctx, id, lines); err != nil {
			return nil, fmt.Errorf("close: %w", err)
		}
	}
	if err := write(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	closed, err := s.stored(ctx, id)
	if err != nil || pe.Status == domain.PlatformExpDraft {
		return closed, err
	}
	// A job still live at the close is billed when it ends, and a record written before would
	// state less than consumed_accelerator_hours: RecordCloses writes it once every job settled.
	settled, err := s.store.JobsSettled(ctx, id)
	if err != nil || !settled {
		return closed, err
	}
	if err := s.recordClose(ctx, closed); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	return s.stored(ctx, id)
}

// Reopen sets the closed platform experiment id, whose run started, running again until endsAt as
// the same run: its registry, jobs, branches, description, baseline and agents stay, and what the close
// wrote goes. Its agents and coordinator are live again by the rows alone: those whose run ended
// only with the close have no end of their own, while one that finished or was cut before keeps
// it. endsAt is held to the length a create is held to, counted from the run's start.
// coordinatorModel, resolved by the caller, moves a sandboxed coordinator to that model, and empty
// keeps the one stored.
func (s *Service) Reopen(ctx context.Context, id string, endsAt time.Time, coordinatorModel string) (*domain.PlatformExperiment, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case pe.Status != domain.PlatformExpClosed:
		return nil, invalidTransition("platform experiment %s is %s: only a closed one is reopened", id, pe.Status)
	case pe.StartedAt.IsZero():
		return nil, invalidTransition("platform experiment %s closed before its run started (close_reason %s): there is no run to continue, create it again", id, pe.CloseReason)
	case !endsAt.After(time.Now()):
		return nil, refuseUpdate(422, "invalid_length", "ends_at %s has passed: state a time ahead", endsAt.UTC().Format(time.RFC3339))
	case coordinatorModel != "" && !pe.CoordinatorInAgentSandbox:
		return nil, refusal.Invalid(refusal.ReasonInvalidRequest, "coordinator_model is set and platform experiment "+id+" has no sandboxed coordinator").On("coordinator_model")
	}
	if length := endsAt.Sub(pe.StartsAt); length > s.cfg.MaxRunLength {
		return nil, refuseUpdate(422, "run_too_long", "ends_at %s would have the run, started at %s, last %.1f days, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state an earlier ends_at",
			endsAt.UTC().Format(time.RFC3339), pe.StartsAt.UTC().Format(time.RFC3339), length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	durationHours := endsAt.Sub(pe.StartsAt).Hours()
	err = s.store.ReopenPlatformExperiment(ctx, id, endsAt, durationHours, coordinatorModel)
	switch {
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return nil, noPlatformExperiment(id)
	case errors.Is(err, db.ErrNotReopenable):
		return nil, invalidTransition("platform experiment %s is continued by a later run, or changed since it was read: a continued run stays closed", id)
	case err != nil:
		return nil, fmt.Errorf("reopen: %w", err)
	}
	s.logger.Info("platform experiment reopened", zap.String("platform_experiment_id", id), zap.Time("ends_at", endsAt))
	return s.stored(ctx, id)
}

// closeRecordWindow is how long after its close a platform experiment whose jobs have not all
// settled waits for its record of the close.
const closeRecordWindow = 24 * time.Hour

// RecordCloses writes the record of the close of every platform experiment Close left without
// one, because a job of it was still live, once its every job ended and was settled.
func (s *Service) RecordCloses(ctx context.Context) error {
	ids, err := s.store.ClosesToRecord(ctx, closeRecordWindow)
	if err != nil {
		return fmt.Errorf("closes to record: %w", err)
	}
	for _, id := range ids {
		pe, err := s.stored(ctx, id)
		if err == nil {
			err = s.recordClose(ctx, pe)
		}
		if err != nil {
			s.logger.Error("record_closes: failed", zap.String("platform_experiment_id", id), zap.Error(err))
		}
	}
	return nil
}

// closeRecordHeading opens the platform's record of a close.
const closeRecordHeading = "Recorded by the platform:"

// recordClose writes beside the summary of pe, which closed after it was open and whose every
// job ended and was settled, what the platform knows of it: why it closed, the jobs submitted,
// each agent's last activity and how its run ended, and the spend, which is then
// consumed_accelerator_hours whole. It is answered after the coordinator's summary, which a
// coordinator on its last turn still rewrites, so an interim summary never stands alone.
func (s *Service) recordClose(ctx context.Context, pe *domain.PlatformExperiment) error {
	why, err := s.closedReason(ctx, pe)
	if err != nil {
		return err
	}
	jobs, err := s.store.AgentJobsByAgent(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	agents, err := s.store.CloseAgents(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	if agents == "" {
		return fmt.Errorf("recordClose: %s has no agent lines taken at its close", pe.ID)
	}
	if err := s.Spend(ctx, pe); err != nil {
		return err
	}
	submitted := 0
	for _, agent := range jobs {
		submitted += agent.Submitted
	}
	var record strings.Builder
	fmt.Fprintf(&record, "%s it closed at %s.\n\n", closeRecordHeading, pe.ClosedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&record, "Close reason: %s: %s.\n", why.Reason, why.Message)
	fmt.Fprintf(&record, "Jobs submitted: %d.\n", submitted)
	fmt.Fprintf(&record, "Spent: %g of the budget of %g accelerator hours.\n\n%s", *pe.ConsumedAcceleratorHours, pe.BudgetAcceleratorHours, agents)
	if len(pe.BaselineRaises) > 0 {
		fmt.Fprintf(&record, "\nBaseline raised while it ran:\n")
		for _, raise := range pe.BaselineRaises {
			by := string(raise.Actor.Kind)
			if raise.Actor.ID != "" {
				by += " " + raise.Actor.ID
			}
			fmt.Fprintf(&record, "- at %s, from job %s to job %s, by %s: %s.\n", raise.At.UTC().Format(time.RFC3339), raise.From, raise.To, by, raise.Reason)
		}
	}
	if err := s.messagesSent(ctx, pe.ID, signups, &record); err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	if pe.CloseReason == domain.CloseBaselineLimitReached {
		if err := s.baselineLeft(ctx, pe, &record); err != nil {
			return fmt.Errorf("recordClose: %w", err)
		}
	}
	return s.store.RecordClose(ctx, pe.ID, record.String())
}

// messagesSent writes to record every message sent to an agent of the platform experiment.
func (s *Service) messagesSent(ctx context.Context, id string, signups []domain.AgentSignup, record *strings.Builder) error {
	messages, err := s.store.ListAgentMessages(ctx, id, "")
	if err != nil || len(messages) == 0 {
		return err
	}
	names := make(map[string]string, len(signups))
	for _, signup := range signups {
		names[signup.AgentID] = signup.Name
	}
	fmt.Fprintf(record, "\nMessages to agents:\n")
	for _, m := range messages {
		to := names[m.AgentID]
		if to == "" {
			to = m.AgentID
		}
		fmt.Fprintf(record, "- at %s, from the %s to %s: %s\n", m.At.UTC().Format(time.RFC3339), m.From, to, strings.Join(strings.Fields(m.Text), " "))
	}
	return nil
}

// baselineLeft writes to record what the baseline agents of pe, closed before its run started,
// left for another run to start from: each job with how it ended, the commit it ran and the
// environment built for it, and the last commit of the baseline branch a job was submitted from.
func (s *Service) baselineLeft(ctx context.Context, pe *domain.PlatformExperiment, record *strings.Builder) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
	if err != nil {
		return err
	}
	branch := gitrefs.BaselineBranch(pe.ID)
	fmt.Fprintf(record, "\nWhat the baseline left, on branch %s:\n", branch)
	last := ""
	for _, job := range jobs {
		fmt.Fprintf(record, "- job %s: %s", job.ID, domain.AnsweredStatus(job.Status, job.EvictionReason))
		if job.EvictionReason != "" {
			fmt.Fprintf(record, " (%s)", job.EvictionReason)
		}
		if job.InfraRequeueCount > 0 {
			fmt.Fprintf(record, "; requeued %d times for faults of the platform's, the last %s", job.InfraRequeueCount, job.InfraRequeueReason)
		}
		fmt.Fprintf(record, "; code %s", job.CodeRef)
		if job.EnvironmentKey != "" {
			env, err := s.environments.Get(ctx, job.EnvironmentKey)
			if err != nil {
				return fmt.Errorf("environment of %s: %w", job.ID, err)
			}
			switch {
			case env == nil:
				return fmt.Errorf("job %s names environment %s, which does not exist", job.ID, job.EnvironmentKey)
			case env.State == domain.EnvironmentReady:
				fmt.Fprintf(record, "; its environment %s built as %s", domain.EnvironmentFile, env.Image)
			default:
				fmt.Fprintf(record, "; its environment %s %s, not built", domain.EnvironmentFile, env.State)
			}
		}
		record.WriteString(".\n")
		if job.Branch == branch {
			last = job.CodeRef
		}
	}
	switch {
	case len(jobs) == 0:
		record.WriteString("No job.\n")
	case last != "":
		fmt.Fprintf(record, "The last commit of %s a job was submitted from: %s.\n", branch, last)
	}
	return nil
}

// closeAgents is the agent lines of the record of the close of pe, as its agents stand at
// closedAt, the moment just before the close is written: once the record is written, after every
// job settled, their machines are released and what their agents last said is gone.
func (s *Service) closeAgents(ctx context.Context, pe *domain.PlatformExperiment, closedAt time.Time) (string, error) {
	jobs, err := s.store.AgentJobsByAgent(ctx, pe.ID)
	if err != nil {
		return "", err
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return "", err
	}
	var lines strings.Builder
	lines.WriteString("Agents:\n")
	if pe.Coordinator != nil {
		ended := runEnded(&pe.Coordinator.Instance, nil, "", closedAt)
		if pe.Coordinator.Instance.AskedAt == nil {
			ended += ": the platform launches a coordinator once the run starts or the experiment waits for it, and this one closed before either, so this record is its summary"
		}
		fmt.Fprintf(&lines, "- coordinator: %s; %s.\n", lastActivity(pe.Coordinator.Activity), ended)
	}
	for _, signup := range signups {
		if signup.Role == domain.SignupRolePlanner {
			continue
		}
		fmt.Fprintf(&lines, "- %s (%s): %d jobs submitted; %s; %s.\n", signup.Name, signup.Role, jobs[signup.AgentID].Submitted,
			lastActivity(signup.Activity), runEnded(signup.Instance, signup.FinishedAt, signup.FinishedReason, closedAt))
	}
	return lines.String(), nil
}

// lastActivity is what an agent last said it was doing, as a close record states it.
func lastActivity(activity *domain.AgentActivity) string {
	if activity == nil {
		return "no activity reported"
	}
	return fmt.Sprintf("last activity %q since %s", activity.Text, activity.At.UTC().Format(time.RFC3339))
}

// runEnded is how an agent's run stood at closedAt, the close: finished for its reason, never
// launched when no machine was asked for it, at work when it held a running machine or one that
// ended only with the close, and otherwise what its machine was doing. An end is reported only of
// a machine that ended before the close and was not replaced: EndReason is why the last machine
// ended, which a healthy replacement leaves standing.
func runEnded(instance *domain.AgentInstance, finishedAt *time.Time, finishedReason string, closedAt time.Time) string {
	switch {
	case finishedAt != nil:
		return fmt.Sprintf("finished at %s: %q", finishedAt.UTC().Format(time.RFC3339), finishedReason)
	case instance == nil || instance.AskedAt == nil:
		return "never launched"
	case instance.Name == "" && instance.ExitedAt != nil && !instance.ExitedAt.Before(closedAt),
		instance.Name != "" && instance.Shown() == domain.InstanceRunning:
		return "did not finish: at work at the close"
	case instance.Name == "" && instance.EndReason != "":
		ended := fmt.Sprintf("did not finish: its machine ended (%s)", instance.EndReason)
		if instance.ExitedAt != nil {
			ended += " at " + instance.ExitedAt.UTC().Format(time.RFC3339)
		}
		if instance.Stopped {
			ended += " with no restart left"
		}
		return ended + ", and none ran after it"
	}
	return "did not finish: its machine was " + instance.Shown() + " at the close"
}

// CheckCoordinatorModel refuses a coordinator model a reopen names that is no model.
func (s *Service) CheckCoordinatorModel(model string) error {
	if s.launch.CoordinatorModel == "" {
		return auth.Internal(s.logger, "reopen platform experiment failed", errors.New("the handler has no agent launch settings"))
	}
	if err := domain.ValidateModel(model); err != nil {
		return refusal.Invalid(refusal.ReasonInvalidRequest, "coordinator_model: "+err.Error()).On("coordinator_model")
	}
	return nil
}
