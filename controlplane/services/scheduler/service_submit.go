package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

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

// ReasonUnknownJobImage refuses a job whose image is neither a job image the platform offers
// nor a whole registry address.
const ReasonUnknownJobImage = "unknown_job_image"

// resolveImage writes the address of the image the job names into its spec (domain.ResolveJobImage),
// so that everything after admission reads one form. A job that names none is left as it is.
func (s *Service) resolveImage(job *domain.JobSpec) error {
	if job.Image == "" {
		return nil
	}
	address, ok := domain.ResolveJobImage(s.jobImages, job.Image)
	if !ok {
		names := make([]string, len(s.jobImages))
		for i, image := range s.jobImages {
			names[i] = image.Name
		}
		return &AdmissionError{Reason: ReasonUnknownJobImage, Message: fmt.Sprintf(
			"job.image %q is no job image of this platform and no whole registry address: name one of the platform's job images (%s; hl capacity says what each carries and which accelerator types it suits), or state the whole address of another image, registry host first, as in registry.example.com/team/image:tag",
			job.Image, strings.Join(names, ", "))}
	}
	job.Image = address
	return nil
}

// imageChoiceServed refuses a job that its image choice keeps off every cluster of its org
// (noClusterForImageReason): no wait admits it, and the admission loop would say the same of it
// for as long as it queued.
func (s *Service) imageChoiceServed(ctx context.Context, exp *domain.Experiment) error {
	registered, err := s.store.RegisteredClusters(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: registered clusters: %w", err)
	}
	runs, others := byImageChoice(registered, exp)
	// What is installed decides only between clusters on both sides of the choice.
	var installed map[string]domain.Footprint
	var autoscalerEnabled map[string]bool
	if len(runs) > 0 && len(others) > 0 {
		if installed, err = s.clusters.GetTotalCapacity(ctx); err != nil {
			return fmt.Errorf("scheduler: installed capacity: %w", err)
		}
		if autoscalerEnabled, err = s.clusters.GetAutoscalerCapability(ctx); err != nil {
			return fmt.Errorf("scheduler: autoscaler capability: %w", err)
		}
	}
	if reason := noClusterForImageReason(runs, others, installed, autoscalerEnabled, exp); reason != "" {
		return &AdmissionError{Reason: domain.NotAdmittedNoClusterForImage, Message: strings.TrimPrefix(reason, domain.NotAdmittedNoClusterForImage+": ")}
	}
	return nil
}

// heldWhenIdle refuses a job that asks for more than every cluster that would run it has with
// nothing else on it (fitsWhenIdle): no wait admits it, and the admission loop would say so for
// as long as it queued. A cluster that scales up, or whose installed capacity was not read, may
// yet hold it. The refusal states what the job asks for and what each cluster has.
func (s *Service) heldWhenIdle(ctx context.Context, exp *domain.Experiment) error {
	registered, err := s.store.RegisteredClusters(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: registered clusters: %w", err)
	}
	runs, _ := byImageChoice(registered, exp)
	if len(runs) == 0 {
		return nil
	}
	installed, err := s.clusters.GetTotalCapacity(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: installed capacity: %w", err)
	}
	autoscalerEnabled, err := s.clusters.GetAutoscalerCapability(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: autoscaler capability: %w", err)
	}
	nodeTotals, err := s.clusters.GetNodeTotalCapacity(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: installed capacity of nodes: %w", err)
	}
	var offers []string
	for _, c := range runs {
		total, read := installed[c.Name]
		if !read || autoscalerEnabled[c.Name] || fitsWhenIdle(total, nodeTotals[c.Name], exp) {
			return nil
		}
		offers = append(offers, c.Name+" has "+describeOffer(total, nodeTotals[c.Name], exp))
	}
	asks := describeFootprint(exp.Footprint())
	if nodes := exp.Job.Nodes(); nodes > 1 {
		asks += fmt.Sprintf(" over %d nodes", nodes)
	}
	return &AdmissionError{Reason: domain.NotAdmittedExceedsClusterCapacity, Message: fmt.Sprintf(
		"the job asks for %s, more than any cluster that runs it has when idle: %s. Ask for less (hl capacity lists what each cluster has)",
		asks, strings.Join(offers, "; "))}
}

// describeOffer says what a cluster has installed of what exp asks for, and the most one node of
// it has where it has several.
func describeOffer(total domain.Footprint, nodeTotals map[string]map[string]int64, exp *domain.Experiment) string {
	asked := domain.NewFootprint()
	probe := *exp
	types := candidateAcceleratorTypes(exp)
	if len(types) == 0 {
		types = []domain.AcceleratorType{exp.AcceleratorType}
	}
	for _, t := range types {
		probe.AcceleratorType = t
		for key := range probe.Footprint() {
			asked[key] = total[key]
		}
	}
	offer := describeFootprint(asked)
	if len(nodeTotals) < 2 {
		return offer
	}
	largest := map[string]int64{}
	for _, node := range nodeTotals {
		for name, amount := range node {
			largest[name] = max(largest[name], amount)
		}
	}
	return offer + ", at most " + describeNodeResources(largest) + " on one node"
}

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
			reader, _ := auth.FromContext(ctx)
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

	// The estimate is the one the submit states, and nothing replaces a stated one. A submit that
	// states none takes what the platform measured on the experiment's baseline job, and with
	// neither there is nothing to hold of the agent's quota.
	if exp.EstimatedDurationHours == 0 {
		if pe.JobEstimatedDurationHours == 0 {
			return &AdmissionError{
				Reason:  ReasonEstimateRequired,
				Message: fmt.Sprintf("the job states no estimated_duration_hours and platform experiment %s has no measured one (its job_estimated_duration_hours, read from its baseline job): state the hours it is expected to run with hl job submit --estimated-duration-hours H", pe.ID),
			}
		}
		exp.EstimatedDurationHours = pe.JobEstimatedDurationHours
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
	if err := s.imageChoiceServed(ctx, exp); err != nil {
		return err
	}
	if err := s.heldWhenIdle(ctx, exp); err != nil {
		return err
	}

	// Default capacity tier so quota debit and DB insert agree.
	if exp.CapacityTier == "" {
		exp.CapacityTier = domain.CapacityGuaranteed
	}

	signedUp, err := s.store.IsSignedUp(ctx, exp.PlatformExperimentID, exp.AgentID)
	if err != nil {
		return fmt.Errorf("scheduler: check signup: %w", err)
	}
	if !signedUp {
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

	// 3ab. Durable-data ceiling.
	if err := s.enforceDataCeiling(ctx, exp); err != nil {
		return err
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
	// says at once why the admission loop will hold it.
	owed, err := s.store.UnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID)
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
	activeExps, err := s.store.GetRunningAndQueued(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: get running and queued: %w", err)
	}
	activeExps = inPlatformExperiment(activeExps, exp.PlatformExperimentID)
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
	now := time.Now().UTC()
	exp.CreatedAt = now
	exp.UpdatedAt = now
	exp.Status = domain.StatusQueued
	exp.PriorityScore = priorityScore
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
	if err != nil {
		var insufficient interface{ InsufficientQuota() bool }
		if !errors.As(err, &insufficient) {
			// The transaction found the id taken, or lost the primary key to another agent
			// racing the same id. Either way it is a duplicate, not a server fault.
			if errors.Is(err, db.ErrDuplicateExperiment) {
				return &AdmissionError{Reason: ReasonDuplicate, Message: err.Error()}
			}
			return fmt.Errorf("scheduler: atomically admit experiment: %w", err)
		}
		reason := ReasonInsufficientCredits
		switch {
		case strings.HasPrefix(err.Error(), db.RejectionRateLimited):
			reason = ReasonRateLimited
		case strings.HasPrefix(err.Error(), db.RejectionAgentFinished):
			reason = "agent_finished"
		}
		return &AdmissionError{
			Reason:  reason,
			Message: err.Error(),
		}
	}

	// Re-submitting a job that is already QUEUED is legal and only refreshes its priority: it is
	// still waiting for capacity, so there is nothing to insert and nothing to debit.
	if decision == db.AdmitAlreadyQueued {
		if err := s.store.UpdateExperimentPriority(ctx, exp.ID, priorityScore); err != nil {
			return fmt.Errorf("scheduler: update priority: %w", err)
		}
	}

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

func inPlatformExperiment(exps []*domain.Experiment, platformExperimentID string) []*domain.Experiment {
	kept := make([]*domain.Experiment, 0, len(exps))
	for _, e := range exps {
		if e.PlatformExperimentID == platformExperimentID {
			kept = append(kept, e)
		}
	}
	return kept
}
