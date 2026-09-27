package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

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
	switch pe.Status {
	case domain.PlatformExpRunning:
	// A draft's only signup is its planner (the store takes no other until it is accepted), so
	// the signup check below admits the planner's baseline job and nobody else's, against the
	// quota the draft gave it: the planning allowance.
	case domain.PlatformExpDraft:
	case domain.PlatformExpOpen:
		return &AdmissionError{
			Reason:  "experiment_not_started",
			Message: fmt.Sprintf("platform experiment %s has not started; jobs are accepted only while it is running", pe.ID),
		}
	default:
		return &AdmissionError{
			Reason:  "experiment_closed",
			Message: fmt.Sprintf("platform experiment %s is over; jobs are accepted only while it is running", pe.ID),
		}
	}

	// 2. Structural validation.
	if err := ValidateExperiment(exp, s.credits); err != nil {
		return err
	}
	if err := s.codeInRepository(pe, exp.CodeRef); err != nil {
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
			Message: "agent finished in this platform experiment (POST /platform-experiments/{id}/agents/{agent_id}/finish), which is final: it submits no further job. Its token still reads",
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
	unsummarized, err := s.store.HasUnsummarizedCompleted(ctx, exp.AgentID, exp.PlatformExperimentID)
	if err != nil {
		return fmt.Errorf("scheduler: check unsummarized completed: %w", err)
	}
	if unsummarized {
		exp.NotAdmittedReason = domain.NotAdmittedSummaryGate
	}

	// 2b. Validate hypothesis reference: every experiment must test a specific,
	// previously-registered hypothesis (POST /hypotheses) rather than restating
	// free text ad hoc. Denormalize its text onto the experiment for cheap reads.
	if exp.HypothesisID == "" {
		return &AdmissionError{Reason: ReasonMalformed, Message: "hypothesis_id is required — register or retrieve one via POST /hypotheses"}
	}
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

	// 4. Compute estimated cost if not already set. Accelerator-hours is the primary/always-populated
	// dimension. CPU/Memory/Storage are always set on JobSpec now (see ValidateExperiment's
	// "explicit resource requests" cross-cutting fix), so there is no more "left unset,
	// resolved later by a cluster-side default" case to work around here.
	//
	// RAM/storage are hard physical-fit-checked at admission (see
	// domain.Experiment.Footprint()/domain.Fits, wired into loop_tick.go), never hours-estimated
	// or hours-debited: the RAM/storage hours-quota machinery (PlatformExperiment budget fields,
	// AgentQuota guaranteed/burst columns, domain.ResourceType entries) has been removed entirely.
	if exp.EstimatedCostAccH == 0 && exp.AcceleratorCount > 0 {
		exp.EstimatedCostAccH = estimatedAcceleratorCost(exp)
	}

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

func estimatedAcceleratorCost(exp *domain.Experiment) float64 {
	if exp.AcceleratorCount == 0 {
		return 0
	}
	return exp.AcceleratorType.Cost() * float64(exp.AcceleratorCount) * exp.EstimatedDurationHours
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
