package scheduler

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
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
// baseline is measured on one branch (and checked on those beside it), which the refusal of a baseline agent's job names with the
// git command that makes it, since a baseline agent has no hypothesis to check out.
func branchOfExperiment(exp *domain.Experiment, measuresBaseline bool) error {
	if measuresBaseline {
		if baseline := gitrefs.BaselineBranch(exp.PlatformExperimentID); !gitrefs.IsBaselineAgentBranch(exp.PlatformExperimentID, exp.Branch) {
			return &AdmissionError{Reason: ReasonBranchNotOfExperiment, Message: fmt.Sprintf("the baseline is measured on branch %s, and this job names %q: git checkout -b %s, commit there and submit again (a check of your code as an executor would run it goes on %s-check)", baseline, exp.Branch, baseline, baseline)}
		}
		return nil
	}
	if gitrefs.IsExperimentBranch(exp.PlatformExperimentID, exp.Branch) {
		return nil
	}
	return &AdmissionError{Reason: ReasonBranchNotOfExperiment, Message: fmt.Sprintf("branch %q is no branch of platform experiment %s: a job runs a commit pushed on a branch under %s, which hl hypothesis checkout HYPOTHESIS_ID makes and hl job submit pushes and states", exp.Branch, exp.PlatformExperimentID, gitrefs.BranchPrefix(exp.PlatformExperimentID))}
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

// takeImage runs exp in the image its platform experiment records now, on nodes of that image's
// architecture: a job runs only where its image does.
func takeImage(pe *domain.PlatformExperiment, exp *domain.Experiment) error {
	if pe.Image == nil {
		return &AdmissionError{Reason: domain.ReasonNoExperimentImage, Message: fmt.Sprintf("platform experiment %s records no image for its jobs to run in: record one with hl image set REF, a base image that has what the code needs or one built from .hypothesisloop/Dockerfile and pushed (hl image repository)", pe.ID)}
	}
	if stated, ok := exp.Job.NodeSelector[domain.NodeArchitectureLabel]; ok && stated != pe.Image.Architecture {
		return &AdmissionError{Reason: ReasonMalformed, Field: "job.node_selector", Message: fmt.Sprintf("job.node_selector states %s %s, and the image of platform experiment %s, %s, runs on %s", domain.NodeArchitectureLabel, stated, pe.ID, pe.Image.Image, pe.Image.Architecture)}
	}
	exp.Image = pe.Image.Image
	exp.ImageArchitecture = pe.Image.Architecture
	return nil
}

// runsAsStated refuses a job that no wait admits as it is stated, for its image's architecture,
// its size or the platform compute it reserves, with the reason the admission loop would give it for as long as it queued
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
	if whenIdle.nodeLabels, err = s.clusters.GetNodeLabels(ctx); err != nil {
		return fmt.Errorf("scheduler: node labels: %w", err)
	}
	if whenIdle.limits, err = s.store.OrgComputeLimits(ctx); err != nil {
		return fmt.Errorf("scheduler: platform compute limits: %w", err)
	}
	if reason, message := domain.SplitQueueReason(whenIdle.neverFitsReason(exp)); reason != "" {
		return &AdmissionError{Reason: reason, Message: message}
	}
	return nil
}

// sameHypothesisWindow is how long a completed job of another agent makes a new job on its
// hypothesis a likely repeat.
const sameHypothesisWindow = time.Hour

// sameHypothesis says which jobs of other agents test exp's hypothesis: of active, those queued
// or running, and those completed within sameHypothesisWindow. Empty when there are none.
func (s *Service) sameHypothesis(ctx context.Context, exp *domain.Experiment, active []*domain.Experiment) (string, error) {
	var others []*domain.Experiment
	for _, job := range active {
		if job.HypothesisID == exp.HypothesisID && job.AgentID != exp.AgentID && job.ID != exp.ID {
			copied := *job
			others = append(others, &copied)
		}
	}
	completed, err := s.store.ListExperiments(ctx, domain.ExperimentFilter{PlatformExperimentID: exp.PlatformExperimentID, HypothesisID: exp.HypothesisID,
		Status: domain.StatusCompleted, Sort: "-updated_at", Limit: 20})
	if err != nil {
		return "", fmt.Errorf("scheduler: jobs on the same hypothesis: %w", err)
	}
	since := time.Now().Add(-sameHypothesisWindow)
	for _, job := range completed {
		if job.AgentID != exp.AgentID && job.FinishedAt != nil && job.FinishedAt.After(since) {
			others = append(others, job)
		}
	}
	if len(others) == 0 {
		return "", nil
	}
	if err := s.store.DescribeJobs(ctx, others); err != nil {
		return "", fmt.Errorf("scheduler: jobs on the same hypothesis: %w", err)
	}
	named := make([]string, len(others))
	for i, job := range others {
		named[i] = fmt.Sprintf("%s of %s (%s)", job.ID, job.AgentName, job.Status)
	}
	return domain.EventDetail(fmt.Sprintf("hypothesis %s is tested by another agent too: %s. If this job repeats that work, cancel it (hl job cancel %s) and take a hypothesis no other agent runs; the coordinator is told",
		exp.HypothesisID, strings.Join(named, ", "), exp.ID)), nil
}

// bindHypothesis checks the hypothesis a job names, which is neither merged nor retired, and keeps
// its text on the job.
func (s *Service) bindHypothesis(ctx context.Context, exp *domain.Experiment) error {
	hyp, err := s.hypotheses.GetHypothesis(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("scheduler: get hypothesis: %w", err)
	}
	if hyp == nil {
		return &AdmissionError{
			Reason:  ReasonMalformed,
			Field:   "hypothesis_id",
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
			Field:   "hypothesis_id",
			Message: hypothesisNotInExperiment(exp),
		}
	}
	if err := uncurated(hyp); err != nil {
		return err
	}
	exp.Hypothesis = hyp.Text
	return nil
}

// stillUncurated reads the hypothesis exp tests again, inside the transaction that inserts exp and
// holds its hypothesis's lock (db.AdmitExperimentTx): a merge or retirement that committed since
// bindHypothesis read it refuses the job, and one that comes later finds the job and waits.
func (s *Service) stillUncurated(ctx context.Context, exp *domain.Experiment) error {
	hyp, err := s.hypotheses.GetHypothesis(ctx, exp.HypothesisID)
	if err != nil {
		return fmt.Errorf("scheduler: get hypothesis: %w", err)
	}
	if hyp == nil {
		return &AdmissionError{Reason: ReasonMalformed, Field: "hypothesis_id", Message: hypothesisNotInExperiment(exp)}
	}
	return uncurated(hyp)
}

// uncurated refuses a job on hyp once it is merged or retired.
func uncurated(hyp *domain.Hypothesis) error {
	switch hyp.Status {
	case domain.HypothesisMerged:
		return &AdmissionError{
			Reason:  platformapi.ReasonHypothesisMerged,
			Field:   "hypothesis_id",
			Message: fmt.Sprintf("hypothesis %s was merged into %s, which states the same claim: test it under %s", hyp.ID, hyp.MergedInto, hyp.MergedInto),
		}
	case domain.HypothesisRetired:
		return &AdmissionError{
			Reason:  platformapi.ReasonHypothesisRetired,
			Field:   "hypothesis_id",
			Message: fmt.Sprintf("hypothesis %s was retired and takes no job: %s. Take an open hypothesis instead", hyp.ID, hyp.StatusReason),
		}
	}
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
