package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// ReasonCodeRefNotInRepository refuses a job whose code is not a commit of its platform
// experiment's repository.
const ReasonCodeRefNotInRepository = "code_ref_not_in_repository"

// codeInRepository checks that a code_ref, already shaped <address>@<sha>, names the repository
// of its platform experiment as the git host serves it, with no credential in the address: a
// code_ref is stored and shown.
func (s *Service) codeInRepository(pe *domain.PlatformExperiment, codeRef string) error {
	address := codeRef[:strings.LastIndex(codeRef, "@")]
	if pe.Repository == "" {
		return &AdmissionError{Reason: ReasonCodeRefNotInRepository, Message: fmt.Sprintf("platform experiment %s names no repository, so it has no code a job could run: create a platform experiment, which has one", pe.ID)}
	}
	if u, err := url.Parse(address); err == nil && u.User == nil && s.codeHost.Serves(pe.Repository, u.Scheme, u.Host, u.Path) {
		return nil
	}
	return &AdmissionError{Reason: ReasonCodeRefNotInRepository, Message: fmt.Sprintf("code_ref must be a commit of the platform experiment's repository, %s@<commit-sha>, and it names another address: push your commit there and submit from your clone with code_ref left empty, which fills it in", pe.RepositoryURL)}
}

// ReasonBaselineTestsNoHypothesis refuses the job of a baseline agent that names a hypothesis.
const ReasonBaselineTestsNoHypothesis = "baseline_tests_no_hypothesis"

// ReasonBranchNotOfExperiment refuses a job whose branch is none of its platform experiment's.
const ReasonBranchNotOfExperiment = "branch_not_of_experiment"

// branchOfExperiment checks that a job names the branch its commit was pushed on, and that the
// branch is one of its platform experiment's: the default branch is nobody's implementation. The
// baseline is measured on one branch, which the refusal of a baseline agent's job names with the
// git command that makes it, since a baseline agent has no hypothesis to check out.
func branchOfExperiment(exp *domain.Experiment, measuresBaseline bool) error {
	if measuresBaseline {
		if baseline := domain.BaselineBranch(exp.PlatformExperimentID); exp.Branch != baseline {
			return &AdmissionError{Reason: ReasonBranchNotOfExperiment, Message: fmt.Sprintf("the baseline is measured on branch %s, and this job names %q: git checkout -b %s, commit there and submit again", baseline, exp.Branch, baseline)}
		}
		return nil
	}
	prefix := domain.BranchPrefix(exp.PlatformExperimentID)
	if len(exp.Branch) > len(prefix) && strings.HasPrefix(exp.Branch, prefix) && !strings.ContainsAny(exp.Branch, " \t\n") {
		return nil
	}
	return &AdmissionError{Reason: ReasonBranchNotOfExperiment, Message: fmt.Sprintf("branch %q is no branch of platform experiment %s: a job runs a commit pushed on a branch under %s, which hl hypothesis checkout HYPOTHESIS_ID makes and hl job submit pushes and states", exp.Branch, exp.PlatformExperimentID, prefix)}
}

// shareOutlook tells an agent refused for quota whether waiting can raise its share. A share
// grows at a stage boundary alone, by the hours of the agents cut there, so the boundary's latest
// moment is all the platform knows: how much is given depends on who is cut. Before the run starts
// that moment is no date yet.
func shareOutlook(pe *domain.PlatformExperiment) string {
	stage, ok := pe.CurrentStageDef()
	if !ok || pe.CurrentStage >= len(pe.Stages) || stage.EvictPct <= 0 {
		return "No later stage raises the agent's share: what remains is all it has for this platform experiment."
	}
	// The run has no start and no end until it starts, so the boundary is a time into it.
	progress := domain.BoundaryProgress(pe.Stages, pe.CurrentStage)
	ends := fmt.Sprintf("within %s of the run's start, and the run has not started", time.Duration(progress*pe.DurationHours*float64(time.Hour)).Round(time.Second))
	if endsAt := pe.StageEndsAt(pe.CurrentStage); !endsAt.IsZero() {
		ends = "by " + endsAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("Stage %d of %d ends %s, or sooner as the budget is spent: the agents ranked lowest are cut then and their hours go to those that remain, so the share rises only for an agent that passes it.",
		pe.CurrentStage, len(pe.Stages), ends)
}

// ReasonImageRequired refuses a job that names no image.
const ReasonImageRequired = "image_required"

// resolveImage writes the address of the image the job names into its spec (domain.ResolveJobImage),
// so that everything after admission reads one form. Every job names its image: a cluster with one
// image runs the jobs that name it, so no job is placed by what it leaves out.
func (s *Service) resolveImage(job *domain.JobSpec) error {
	if job.Image == "" {
		return &AdmissionError{Reason: ReasonImageRequired, Message: "job.image is required: state the image the job runs in, a job_image of hl capacity or the whole address of another, registry host first"}
	}
	address, ok := domain.ResolveJobImage(s.jobImages, job.Image)
	if !ok {
		return &AdmissionError{Reason: domain.ReasonUnknownJobImage, Message: domain.UnknownJobImage(s.jobImages, "job.image", job.Image)}
	}
	job.Image = address
	return nil
}

// ReasonEnvironmentUnreadable refuses a job whose environment file the git host did not hand over.
const ReasonEnvironmentUnreadable = "environment_unreadable"

// declaredEnvironment keeps on exp the environment its commit declares, built on its image, which
// admission waits for. The file is read at the commit itself, so the environment is the one the
// code was pushed with.
func (s *Service) declaredEnvironment(ctx context.Context, pe *domain.PlatformExperiment, exp *domain.Experiment) error {
	commit := exp.CodeRef[strings.LastIndex(exp.CodeRef, "@")+1:]
	env, err := s.environments.Declared(ctx, pe.ProjectID, pe.Repository, commit, exp.Job.Image)
	var invalid *domain.EnvironmentFileError
	var refused *api.Refusal
	switch {
	case errors.As(err, &invalid):
		return &AdmissionError{Reason: domain.ReasonInvalidEnvironmentFile, Message: invalid.Why + ". Fix it, commit and submit again"}
	case errors.As(err, &refused):
		return &AdmissionError{Reason: ReasonEnvironmentUnreadable, Message: fmt.Sprintf("the platform could not read %s at commit %s: %s", domain.EnvironmentFile, commit, refused.Message)}
	case err != nil:
		return fmt.Errorf("scheduler: environment of %s: %w", commit, err)
	case env != nil:
		exp.EnvironmentKey = env.Key
	}
	return nil
}

// runsAsStated refuses a job that no wait admits as it is stated, for its image or its
// size, with the reason the admission loop would give it for as long as it queued
// (installed.neverFitsReason).
func (s *Service) runsAsStated(ctx context.Context, exp *domain.Experiment) error {
	var whenIdle installed
	var err error
	if whenIdle.registered, err = s.store.RegisteredClusters(ctx); err != nil {
		return fmt.Errorf("scheduler: registered clusters: %w", err)
	}
	if whenIdle.totals, err = s.clusters.GetTotalCapacity(ctx); err != nil {
		return fmt.Errorf("scheduler: installed capacity: %w", err)
	}
	if whenIdle.nodeTotals, err = s.clusters.GetNodeTotalCapacity(ctx); err != nil {
		return fmt.Errorf("scheduler: installed capacity of nodes: %w", err)
	}
	if whenIdle.scalesUp, err = s.clusters.GetAutoscalerCapability(ctx); err != nil {
		return fmt.Errorf("scheduler: autoscaler capability: %w", err)
	}
	if whenIdle.multiNode, err = s.clusters.GetMultiNodeCapability(ctx); err != nil {
		return fmt.Errorf("scheduler: multi-node capability: %w", err)
	}
	if reason, message := domain.SplitQueueReason(whenIdle.neverFitsReason(exp)); reason != "" {
		return &AdmissionError{Reason: reason, Message: message}
	}
	return nil
}

// errNoCaller is a request that reached the service with no authenticated caller: every route
// is behind the token check, so it is the platform's fault and never the submitter's.
var errNoCaller = errors.New("scheduler: no authenticated caller in the request context")

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
				return errNoCaller
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
	// the figure it admits on: the largest the agent's quota holds, within the stage's limit.
	if exp.EstimatedDurationHours == 0 {
		exp.MaxEstimateHours = pe.CurrentMaxJobHours()
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

	// 3a. Summary gate. A job that completed during the agent's turn is unknown to the agent until
	// the turn ends, so a refusal here would hit every submit of a batch. The job is queued and
	// says at once why the admission loop will hold it: for the jobs that ended before this
	// moment, the one the row is dated by.
	now := time.Now().UTC()
	exp.CreatedAt, exp.UpdatedAt = now, now
	owed, err := s.store.UnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID, exp.CreatedAt)
	if err != nil {
		return fmt.Errorf("scheduler: check unsummarized completed: %w", err)
	}
	if len(owed) > 0 {
		exp.NotAdmittedReason = summaryGateReason(owed)
	}

	// 2b. Every job but the baseline's tests a hypothesis registered before it (POST /hypotheses),
	// whose text is kept on the job for cheap reads.
	if !measuresBaseline {
		if err := s.bindHypothesis(ctx, exp); err != nil {
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
	noveltyScore, err := s.novelty.ComputeNovelty(ctx, exp.HypothesisID, excludeExperiment(activeExps, exp.ID))
	if err != nil {
		return fmt.Errorf("scheduler: compute novelty: %w", err)
	}
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

	// 8. Wake the scheduler loop.
	s.loop.Trigger()
	return nil
}

// bindHypothesis checks the hypothesis a job names and keeps its text on the job.
func (s *Service) bindHypothesis(ctx context.Context, exp *domain.Experiment) error {
	hyp, err := s.hypotheses.GetHypothesis(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("scheduler: get hypothesis: %w", err)
	}
	if hyp == nil {
		return &AdmissionError{
			Reason:  ReasonMalformed,
			Message: hypothesisNotInExperiment(exp),
		}
	}
	// An idea no agent owns is testable like any other: any agent may bind a job to it. The
	// hypothesis is the idea, the job is the agent's — exp.AgentID is the owner every accounting
	// path keys on, so quota is debited from the running agent and its result lands in that
	// agent's standings, exactly as for a hypothesis it registered itself. Nothing here has to
	// know whose the row is. Refusing instead (the earlier rule) made a human idea inert and
	// pushed agents into registering a near-duplicate claim to test one, which fights the pool's
	// own dedup and splits one claim's evidence across two rows.
	//
	// The hypothesis must belong to the same platform experiment this job is submitted under.
	// Without this check an agent could point a job at a hypothesis registered under a different
	// program, contaminating cross-program scope/ranking (invariant #7).
	if hyp.PlatformExperimentID != exp.PlatformExperimentID {
		return &AdmissionError{
			Reason:  ReasonMalformed,
			Message: hypothesisNotInExperiment(exp),
		}
	}
	exp.Hypothesis = hyp.Text
	return nil
}

// enforceDataCeiling refuses a submission from an agent already at or over its durable-data
// ceiling. The figure is asked of the object store, which is the only thing that knows what an
// agent actually holds — there is no counter in PostgreSQL to drift from the bytes.
//
// This is the only place the ceiling is applied. Enforcing it mid-write would need a gateway in
// the data path, and a job killed as it saves loses the run it was about to preserve; refusing
// the next submission instead costs an agent nothing it has already earned.
func (s *Service) enforceDataCeiling(ctx context.Context, exp *domain.Experiment) error {
	if s.maxDataBytesPerAgent <= 0 {
		return nil
	}
	held, err := s.dataStore.TotalBytes(ctx, objectstore.AgentPrefix(exp.PlatformExperimentID, exp.AgentID))
	if err != nil {
		return fmt.Errorf("scheduler: measure agent durable data: %w", err)
	}
	if held < s.maxDataBytesPerAgent {
		return nil
	}
	return &AdmissionError{
		Reason: ReasonDataQuotaExceeded,
		Message: fmt.Sprintf(
			"agent holds %d bytes of durable data in this platform experiment, at or over the %d-byte ceiling — delete what you no longer need under HYPOTHESISLOOP_DATA_URI (list a job's files with GET /experiments/{id}/data) before submitting again",
			held, s.maxDataBytesPerAgent),
	}
}

// hypothesisNotInExperiment is the one answer for a hypothesis that does not exist and for one
// of another platform experiment, so a submission cannot probe for the latter.
func hypothesisNotInExperiment(exp *domain.Experiment) string {
	return fmt.Sprintf("hypothesis %s not found in platform experiment %s — register it first via POST /hypotheses", exp.HypothesisID, exp.PlatformExperimentID)
}
