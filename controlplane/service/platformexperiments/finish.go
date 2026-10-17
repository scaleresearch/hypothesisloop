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

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// FinishAgent records that an agent has nothing left to do, which releases its machine, and
// answers its signup as finished. Calling it again changes nothing. The finish of an agent of
// role baseline is the hand-over of what it measured: the platform declares baselineJobID, or
// without it the one job it left (DeclareMeasured), in this request, which starts the run when
// nothing else is missing. A baselineJobID that could not be declared refuses the finish.
//
// A researcher that finishes with more than finishEarlyShare of the run's time and of its quota
// left, and more time than its jobs typically take, names the blocker that stops it, and the time and quota it leaves are recorded with its
// reason, which agent.finished tells the coordinator.
func (s *PlatformExperimentsService) FinishAgent(ctx context.Context, platformExpID, agentID, reason, blocker, baselineJobID string) (*domain.AgentSignup, error) {
	reason, err := s.finishReason(ctx, platformExpID, agentID, reason, blocker)
	if err != nil {
		return nil, err
	}
	if baselineJobID != "" {
		if err := s.checkNamedBaseline(ctx, platformExpID, agentID, baselineJobID); err != nil {
			return nil, err
		}
	}
	err = s.store.FinishAgent(ctx, platformExpID, agentID, domain.EventDetail(reason))
	switch {
	case errors.Is(err, domain.ErrNotSignedUp):
		return nil, refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	case errors.Is(err, db.ErrSignupClosed):
		pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
		}
		// The same answer as to anything else sent past the close (RequireActive): the token's
		// wind-down is for filing summaries, and the close already ended the agent's run.
		if pe == nil || pe.Status == domain.PlatformExpClosed {
			return nil, refusal.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed: your run is over and nothing is left to finish")
		}
		return nil, refuseSignup(409, "not_running", "platform experiment %s is %s: only an agent of role baseline finishes before it runs", platformExpID, pe.Status)
	case errors.Is(err, db.ErrWorkUnfinished):
		return nil, refuseSignup(409, "work_unfinished", "agent %s has a job queued or running, or a completed job without its summary: cancel or wait for the one, write the other, then finish", agentID)
	case err != nil:
		return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
	}
	finished, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.FinishAgent: %w", err)
	}
	if finished == nil {
		return nil, fmt.Errorf("platform_experiments.FinishAgent: agent %s is not signed up after finishing", agentID)
	}
	s.logger.Info("agent finished", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID))
	if finished.Role == domain.SignupRoleBaseline {
		// The finish is stored, so it is answered as one: a declaration that fails now is the
		// sweep's to make again, and the experiment's status_reason says what it waits for.
		if err := s.declareFinished(ctx, platformExpID, baselineJobID); err != nil {
			s.logger.Error("declare the baseline of a finished agent", zap.String("platformExpID", platformExpID), zap.String("agentID", agentID), zap.String("jobID", baselineJobID), zap.Error(err))
		}
	}
	return finished, nil
}

// checkNamedBaseline refuses, before anything is stored, a finish whose agent names as the
// baseline baselineJobID and could not have it declared: the agent is of role baseline, the job
// is its own, it passes the check of PATCH baseline_job_id, and no other job of the agent beat it.
func (s *PlatformExperimentsService) checkNamedBaseline(ctx context.Context, platformExpID, agentID, baselineJobID string) error {
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.checkNamedBaseline: %w", err)
	}
	if signup == nil {
		return refuseSignup(404, "not_signed_up", "agent %s is not in platform experiment %s", agentID, platformExpID)
	}
	if signup.Role != domain.SignupRoleBaseline {
		return refuseSignup(422, "not_a_baseline_agent", "agent %s is of role %s, and only an agent of role baseline names the baseline it measured: finish without baseline_job_id", agentID, signup.Role)
	}
	pe, err := s.stored(ctx, platformExpID)
	if err != nil {
		return err
	}
	switch {
	case pe.Status == domain.PlatformExpClosed || pe.BaselineJobID == baselineJobID:
		return nil
	case pe.BaselineJobID != "":
		return refuseSignup(409, "baseline_declared", "platform experiment %s already has job %s declared as its baseline: finish without baseline_job_id, or name that job", platformExpID, pe.BaselineJobID)
	case pe.Status != domain.PlatformExpOpen:
		return nil
	}
	baseline, err := s.baseline(ctx, pe, baselineJobID)
	if err != nil {
		return fmt.Errorf("platform_experiments: read baseline %s: %w", baselineJobID, err)
	}
	if baseline == nil || baseline.Job == nil || baseline.Job.AgentID != agentID || baseline.Job.PlatformExperimentID != platformExpID {
		return refusedBaseline("no_baseline", "baseline_job_id %q is no job of yours in platform experiment %s: name the COMPLETED job of the control you submitted (`hl job list`)", baselineJobID, platformExpID)
	}
	declared := *pe
	declared.BaselineJobID = baselineJobID
	_, err = s.heldToBaseline(ctx, &declared, true)
	return err
}

// finishEarlyShare is the share of the run's time and of a researcher's quota that, both left,
// makes its finish one that needs a blocker.
const finishEarlyShare = 0.1

// finishReason is what the finish of agentID records as its reason: reason, the blocker, and for
// a researcher in a run that has a clock the time and quota it leaves. A researcher with more than
// finishEarlyShare of both left, and more time than its jobs typically take (typicalJobTime), and
// no blocker is refused, told what is left.
func (s *PlatformExperimentsService) finishReason(ctx context.Context, platformExpID, agentID, reason, blocker string) (string, error) {
	if blocker != "" {
		reason = sentence(reason) + " Blocker: " + blocker
	}
	signup, err := s.store.GetAgentSignup(ctx, platformExpID, agentID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if signup == nil || signup.Role != domain.SignupRoleCompetitor || signup.FinishedAt != nil {
		return reason, nil
	}
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if pe == nil || pe.Status != domain.PlatformExpRunning || pe.EndsAt.IsZero() {
		return reason, nil
	}
	quota, err := s.GetAgentQuota(ctx, agentID, platformExpID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	if quota == nil {
		return reason, nil
	}
	run := time.Duration(pe.DurationHours * float64(time.Hour))
	timeLeft := max(0, time.Until(pe.EndsAt))
	room := max(quota.Remaining(domain.CapacityGuaranteed), quota.Remaining(domain.CapacityBurst))
	left := fmt.Sprintf("%d of the run's %d minutes and %.3g of the agent's %.3g AccH were left, room for a job of %.3g AccH",
		int(timeLeft.Minutes()), int(run.Minutes()), max(0, quota.Share()-quota.Used()), quota.Share(), room)
	// A researcher was refused with 4 minutes left, when its jobs took 2 to 7, and sat idle
	// until the close: time no job of it fits in is no time left.
	typical, err := s.typicalJobTime(ctx, platformExpID, agentID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments.finishReason: %w", err)
	}
	enough := max(time.Duration(finishEarlyShare*float64(run)), typical)
	if blocker == "" && timeLeft > enough && room > finishEarlyShare*quota.Share() {
		return "", refuseSignup(409, "time_and_quota_left", "%s. Keep pursuing credible improvements while both last. "+
			"If something stops you that this time and quota cannot fix, finish again with --blocker saying what it is "+
			"(the request's blocker field): a blocker written into --reason is not read as one", left)
	}
	return sentence(reason) + " When it finished, " + left + ".", nil
}

// typicalJobTime is the median time agentID's COMPLETED jobs in the platform experiment took from
// submit to end, or with none of its own every COMPLETED job's there, and 0 with none at all.
func (s *PlatformExperimentsService) typicalJobTime(ctx context.Context, platformExpID, agentID string) (time.Duration, error) {
	jobs, err := s.store.CompletedJobs(ctx, platformExpID)
	if err != nil {
		return 0, err
	}
	var own, all []time.Duration
	for _, job := range jobs {
		if job.FinishedAt == nil || job.FinishedAt.Before(job.CreatedAt) {
			continue
		}
		took := job.FinishedAt.Sub(job.CreatedAt)
		all = append(all, took)
		if job.AgentID == agentID {
			own = append(own, took)
		}
	}
	if len(own) == 0 {
		own = all
	}
	if len(own) == 0 {
		return 0, nil
	}
	slices.Sort(own)
	return own[len(own)/2], nil
}

// sentence is text ending in a full stop, so that another can follow it on the same line.
func sentence(text string) string {
	return strings.TrimRight(text, ". ") + "."
}

// Hypotheses reads the hypothesis graph, through the graph's own package. Implemented by
// graphdb.Store.
type Hypotheses interface {
	ListHypotheses(ctx context.Context, f graphdb.ListFilter) ([]*domain.HypothesisListItem, error)
}

// OpenHypothesis is a hypothesis an agent left without a verdict: its own, or a person's idea its
// jobs tested.
type OpenHypothesis struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// maxOpenHypotheses is how many a finish answers: the largest page the graph lists.
const maxOpenHypotheses = 200

// openHypotheses is what the agent leaves without a verdict in the platform experiment: its own
// hypotheses, then the hypotheses no agent owns that one of its completed jobs tested, whose
// verdict is as much its to give.
func openHypotheses(ctx context.Context, graph Hypotheses, completed []*domain.Experiment, platformExpID, agentID string) ([]OpenHypothesis, error) {
	items, err := graph.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExpID, AgentID: agentID, Status: domain.HypothesisOpen, Limit: maxOpenHypotheses})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.openHypotheses: %w", err)
	}
	tested := []string{}
	for _, job := range completed {
		if job.AgentID == agentID && job.PlatformExperimentID == platformExpID && job.HypothesisID != "" && !slices.Contains(tested, job.HypothesisID) {
			tested = append(tested, job.HypothesisID)
		}
	}
	if len(tested) > 0 && len(items) < maxOpenHypotheses {
		others, err := graph.ListHypotheses(ctx, graphdb.ListFilter{PlatformExperimentID: platformExpID, Status: domain.HypothesisOpen, IDs: tested, Limit: maxOpenHypotheses})
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.openHypotheses: %w", err)
		}
		for _, item := range others {
			if item.AgentID == "" && len(items) < maxOpenHypotheses {
				items = append(items, item)
			}
		}
	}
	open := make([]OpenHypothesis, len(items))
	for i, item := range items {
		open[i] = OpenHypothesis{ID: item.ID, Text: item.Text}
	}
	return open, nil
}

// Finish finishes agentID, as FinishAgent does, and answers it as reader reads the roster with the
// hypotheses it leaves without a verdict, read from graph before the finish is stored: a graph
// that does not answer leaves nothing changed.
func (s *PlatformExperimentsService) Finish(ctx context.Context, graph Hypotheses, platformExpID, agentID, reason, blocker, baselineJobID string, reader domain.Actor) ([]JoinedAgent, []OpenHypothesis, error) {
	completed, err := s.store.CompletedJobs(ctx, platformExpID)
	if err != nil {
		return nil, nil, err
	}
	open, err := openHypotheses(ctx, graph, completed, platformExpID, agentID)
	if err != nil {
		return nil, nil, err
	}
	finished, err := s.FinishAgent(ctx, platformExpID, agentID, reason, blocker, baselineJobID)
	if err != nil {
		return nil, nil, err
	}
	agents, err := s.namedRoster(ctx, platformExpID, []domain.AgentSignup{*finished}, reader)
	if err != nil {
		return nil, nil, err
	}
	return agents, open, nil
}
