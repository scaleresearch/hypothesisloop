package platformexperiments

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Baseline is what a platform experiment's baseline job is known to have done, read from the
// platform's own records and never from what anyone says about it.
type Baseline struct {
	Job *domain.Experiment
	// OwnerRole is what the job's agent was signed up as in the job's platform experiment.
	OwnerRole domain.SignupRole
	// Finding says what the job found was filed.
	Finding bool
	// Reported is the last value the job reported for each metric key.
	Reported map[string]float64
	// ObservedHours is how long the job ran, as it was billed. A job too short to be observed
	// alive has none.
	ObservedHours float64
	// Environment is the environment the job's commit declared, nil when it declared none.
	Environment *domain.Environment
	// Declared is the environment file at the head of the baseline branch, nil when it holds
	// none: what a job of the default branch runs in.
	Declared *string
	// References is what the experiment's COMPLETED jobs measured of each reference metric,
	// the baseline's follow-up jobs among them.
	References []MetricReference
}

// BaselineHeadroom is how far over the baseline's observed hours the estimate of a job that
// states none is set. A job holds its whole estimate of its agent's quota until it ends: an
// estimate of 2.2 times the run let one job at a time fit a researcher's share, and one under
// the run reserves less than the job spends.
const BaselineHeadroom = 1.5

// baselineReader reads the Baseline of the job a declaration names, nil when there is no such job.
type baselineReader func(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error)

func (s *PlatformExperimentsService) readBaseline(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error) {
	job, err := s.store.GetExperiment(ctx, jobID)
	if err != nil || job == nil {
		return nil, err
	}
	out := &Baseline{Job: job, Reported: map[string]float64{}}
	signup, err := s.store.GetAgentSignup(ctx, job.PlatformExperimentID, job.AgentID)
	if err != nil {
		return nil, err
	}
	if signup != nil {
		out.OwnerRole = signup.Role
	}
	if out.Finding, err = s.store.JobHasFinding(ctx, jobID); err != nil {
		return nil, err
	}
	samples, err := metricsdb.ExperimentMetricSamples(ctx, s.metricsDBURL, jobID, pe.CreatedAt, time.Now().UTC().Add(time.Minute))
	if err != nil {
		return nil, err
	}
	// In timestamp order, so the last one of a key stands.
	for _, sample := range samples.Values {
		if basis := sample.Labels["metric_basis"]; basis != "" && basis != "raw" {
			continue
		}
		out.Reported[sample.Labels["metric_name"]] = sample.Value
	}
	cost, err := s.JobCost(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if cost != nil {
		out.ObservedHours = cost.ObservedHours
	}
	if job.EnvironmentKey != "" {
		if out.Environment, err = s.environments.Get(ctx, job.EnvironmentKey); err != nil {
			return nil, err
		}
	}
	if out.Declared, err = s.environments.Read(ctx, pe.ProjectID, pe.Repository, domain.BaselineBranch(pe.ID)); err != nil {
		return nil, err
	}
	if out.References, err = s.references(ctx, pe); err != nil {
		return nil, err
	}
	return out, nil
}

// refusedBaseline is a baseline declaration the caller can fix: reason names what is wrong.
func refusedBaseline(reason, format string, args ...any) error {
	return refuseUpdate(422, reason, format, args...)
}

// checkBaseline holds pe, as it would be stored, to what the job it names as its baseline really
// did. Every researcher starts from the baseline and is ranked against it, so one whose
// measurement did not happen is refused.
func checkBaseline(pe *domain.PlatformExperiment, baseline *Baseline) error {
	if baseline == nil || baseline.Job == nil || baseline.Job.PlatformExperimentID != pe.ID || baseline.OwnerRole != domain.SignupRoleBaseline {
		return refusedBaseline("no_baseline", "baseline_job_id %q is no job of a baseline agent of this platform experiment: an agent added with role baseline runs it with `hl job submit`, and that job is the one to name", pe.BaselineJobID)
	}
	if branch := domain.BaselineBranch(pe.ID); baseline.Job.Branch != branch {
		return refusedBaseline("baseline_not_on_branch", "baseline job %s ran branch %q, and the baseline is measured on %s alone: name the control's job of that branch", baseline.Job.ID, baseline.Job.Branch, branch)
	}
	return checkMeasurement(pe, baseline)
}

// checkMeasurement holds a job named as the baseline, whoever ran it on whichever branch, to what
// a baseline measures: it completed, filed its finding, reported every metric but the references,
// held every constraint and ran in the environment of the baseline branch.
func checkMeasurement(pe *domain.PlatformExperiment, baseline *Baseline) error {
	if baseline.Job.Status != domain.StatusCompleted {
		return refusedBaseline("baseline_not_completed", "baseline job %s is %s: a run rests on a baseline that ran to the end. Wait for it, or have it fixed and run again", baseline.Job.ID, baseline.Job.Status)
	}
	if !baseline.Finding {
		return refusedBaseline("baseline_without_finding", "baseline job %s has no finding: its agent files what it measured with `hl job summary`, so the researchers start from it", baseline.Job.ID)
	}
	var missing []string
	for _, metric := range pe.Metrics {
		value, reported := baseline.Reported[metric.Key]
		if !reported && metric.EffectiveRole() != domain.MetricRoleReference {
			missing = append(missing, metric.Key)
			continue
		}
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		if !metric.Holds(value) {
			return refusedBaseline("baseline_breaks_constraint", "the baseline reported %s = %g, which does not hold the bound %g of the metric: a gate the baseline fails disqualifies everyone, and only its owner changes it (PATCH metrics)", metric.Key, value, *metric.Bound)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return refusedBaseline("baseline_missing_metric", "baseline job %s reported no %s: every metric of the platform experiment must be one the harness really reports", baseline.Job.ID, strings.Join(missing, ", "))
	}
	return checkBaselineEnvironment(pe, baseline)
}

// checkBaselineEnvironment holds the baseline job to the environment the baseline branch
// declares: every researcher's job inherits it, so the baseline is measured in it too. The
// environment is the file's content, so a job of another branch that declares the same file ran
// in it.
func checkBaselineEnvironment(pe *domain.PlatformExperiment, baseline *Baseline) error {
	ran, declared := baseline.Environment, baseline.Declared
	branch := domain.BaselineBranch(pe.ID)
	switch {
	case declared != nil && (ran == nil || ran.Dockerfile != *declared):
		return refusedBaseline("baseline_environment_differs", "branch %s declares in %s an environment that baseline job %s did not run in: every researcher's job runs in it, so the baseline is measured in it too. Name a job submitted from a commit that declares the same file, as the head of the branch does", branch, domain.EnvironmentFile, baseline.Job.ID)
	case declared == nil && ran != nil:
		return refusedBaseline("baseline_environment_differs", "baseline job %s ran in the environment its commit declares in %s, and the head of branch %s declares none: commit the file there again, or name a job submitted from a commit that declares none", baseline.Job.ID, domain.EnvironmentFile, branch)
	case ran != nil && (baseline.Job.ResolvedJob == nil || !inEnvironment(ran, baseline.Job.ResolvedJob.Image)):
		return refusedBaseline("baseline_not_in_environment", "baseline job %s did not run in the image of the environment its commit declares (%s): submit it again and name that job", baseline.Job.ID, ran.Image)
	}
	return nil
}

// strongerOwnJob refuses baseline, as its agent's declaration, when another COMPLETED job of the
// same agent in pe, of the baseline branch or one beside it, held every constraint, reported the
// first ranking metric and beat baseline on it. A baseline agent declared a default its own
// check had beaten, and the researchers were ranked against the weaker control. It names the
// strongest such job.
func (s *PlatformExperimentsService) strongerOwnJob(ctx context.Context, pe *domain.PlatformExperiment, baseline *Baseline) error {
	ranked, ok := domain.PrimaryRankingMetric(pe.Metrics)
	if !ok {
		return nil
	}
	declared, reported := baseline.Reported[ranked.Key]
	if !reported {
		return nil
	}
	jobs, err := s.store.CompletedJobs(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments: list the completed jobs of %s: %w", pe.ID, err)
	}
	var stronger *Baseline
	best := declared
	for _, job := range jobs {
		if job.ID == baseline.Job.ID || job.AgentID != baseline.Job.AgentID {
			continue
		}
		other, err := s.baseline(ctx, pe, job.ID)
		if err != nil {
			return fmt.Errorf("platform_experiments: read job %s: %w", job.ID, err)
		}
		if other == nil || other.Job == nil || other.Job.Status != domain.StatusCompleted {
			continue
		}
		value, reported := other.Reported[ranked.Key]
		if reported && ranked.Better(value, best) && holdsConstraints(pe.Metrics, other.Reported) {
			stronger, best = other, value
		}
	}
	if stronger == nil {
		return nil
	}
	return refusedBaseline("baseline_weaker_than_own_job", "job %s of the same baseline agent (branch %s) measured %s = %g and held every constraint, better than %g of job %s: the baseline is the control in its strongest configuration. Run the configuration of job %s as a job of %s and name that job",
		stronger.Job.ID, stronger.Job.Branch, ranked.Key, best, declared, baseline.Job.ID, stronger.Job.ID, domain.BaselineBranch(pe.ID))
}

// holdsConstraints is whether reported holds every constraint metric, each of them reported.
func holdsConstraints(metrics []domain.MetricDefinition, reported map[string]float64) bool {
	for _, metric := range metrics {
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		if value, ok := reported[metric.Key]; !ok || !metric.Holds(value) {
			return false
		}
	}
	return true
}

// checkRaiseRequest refuses a raise of the baseline outside a running platform experiment, and
// one with no reason: while it is open the baseline agent's job is declared, and once it is
// closed nothing changes.
func checkRaiseRequest(pe *domain.PlatformExperiment, patch PlatformExperimentPatch) error {
	switch {
	case patch.BaselineReason != nil && pe.Status != domain.PlatformExpRunning:
		return refuseUpdate(409, "baseline_not_running", "baseline_reason raises the baseline of a running platform experiment, and %s is %s: while it is open its baseline agent measures the baseline and the job it finishes with is declared. Leave baseline_reason out", pe.ID, pe.Status)
	case pe.Status != domain.PlatformExpRunning:
		return nil
	case patch.BaselineJobID == nil && patch.BaselineReason != nil:
		return refuseUpdate(422, "baseline_job_required", "baseline_reason says why the baseline is raised, and the body names no job to raise it to: add baseline_job_id")
	case patch.BaselineJobID != nil && (patch.BaselineReason == nil || strings.TrimSpace(*patch.BaselineReason) == ""):
		return refuseUpdate(422, "baseline_reason_required", "the baseline of a running platform experiment is raised with a reason: say in baseline_reason which configuration of the control job %s measured that the declared baseline did not", *patch.BaselineJobID)
	case patch.BaselineJobID != nil && *patch.BaselineJobID == pe.BaselineJobID:
		return refuseUpdate(422, "baseline_unchanged", "job %s is the baseline already", pe.BaselineJobID)
	}
	return nil
}

// raisedBaseline holds a raise of the running pe's baseline to the job patched names: a completed
// job of the experiment, whoever ran it on whichever branch, that ran the baseline's commit
// unchanged, measured as a baseline is (checkMeasurement) and strictly better than the baseline it
// replaces on the first ranking metric. Only its arguments or environment differ, so a researcher's
// new method never becomes the bar; reason says which configuration it measured. It answers the
// raise to record and the block the brief is given. The estimate of a job that states none stays
// the declared baseline's.
func (s *PlatformExperimentsService) raisedBaseline(ctx context.Context, pe, patched *domain.PlatformExperiment, reason string, by *domain.Actor) (domain.BaselineRaise, string, error) {
	current, err := s.baseline(ctx, pe, pe.BaselineJobID)
	if err != nil {
		return domain.BaselineRaise{}, "", fmt.Errorf("platform_experiments: read baseline %s: %w", pe.BaselineJobID, err)
	}
	if current == nil || current.Job == nil {
		return domain.BaselineRaise{}, "", fmt.Errorf("platform_experiments: baseline job %s of running %s not found", pe.BaselineJobID, pe.ID)
	}
	candidate, err := s.baseline(ctx, patched, patched.BaselineJobID)
	if err != nil {
		return domain.BaselineRaise{}, "", fmt.Errorf("platform_experiments: read job %s: %w", patched.BaselineJobID, err)
	}
	if candidate == nil || candidate.Job == nil || candidate.Job.PlatformExperimentID != pe.ID {
		return domain.BaselineRaise{}, "", refusedBaseline("no_baseline", "baseline_job_id %q is no job of this platform experiment: name a COMPLETED job of it that measured the control in a stronger configuration", patched.BaselineJobID)
	}
	if candidate.Job.CodeRef != current.Job.CodeRef {
		return domain.BaselineRaise{}, "", refuseUpdate(422, "baseline_code_differs", "job %s ran %s, and the baseline's code is %s (job %s): a raised baseline runs the baseline's commit unchanged, only its arguments or environment differ. Run that commit in the stronger configuration and name that job", candidate.Job.ID, candidate.Job.CodeRef, current.Job.CodeRef, pe.BaselineJobID)
	}
	if err := checkMeasurement(patched, candidate); err != nil {
		return domain.BaselineRaise{}, "", err
	}
	ranked, _ := domain.PrimaryRankingMetric(patched.Metrics)
	to := candidate.Reported[ranked.Key]
	from, measured := current.Reported[ranked.Key]
	if measured && !ranked.Better(to, from) {
		return domain.BaselineRaise{}, "", refuseUpdate(422, "baseline_not_stronger", "job %s measured %s = %g, and the baseline, job %s, %g: a raised baseline is strictly better on the ranking metric (%s)", candidate.Job.ID, ranked.Key, to, pe.BaselineJobID, from, ranked.Direction)
	}
	actor := domain.Actor{Kind: domain.ActorUnverified}
	if by != nil {
		actor = *by
	}
	who := "its owner"
	if actor.Kind == domain.ActorCoordinator {
		who = "its coordinator"
	}
	detail := fmt.Sprintf("the baseline was raised to %s = %g by job %s", ranked.Key, to, candidate.Job.ID)
	if measured {
		detail += fmt.Sprintf(" (from %g, job %s)", from, pe.BaselineJobID)
	}
	detail += fmt.Sprintf(", by %s: %s. Re-read the brief: standings are set against the new baseline", who, reason)
	raise := domain.BaselineRaise{From: pe.BaselineJobID, To: candidate.Job.ID, Reason: reason, Detail: detail, Actor: actor}
	return raise, baselineBlock(candidate, patched.Metrics), nil
}

// inEnvironment is whether image is the environment's: the pushed one, or before its push ends the one
// loaded into the builder's node, which a job placed there runs.
func inEnvironment(env *domain.Environment, image string) bool {
	return image != "" && (image == env.Image || image == env.LoadedImage)
}

// MeasuredBaselineHeader opens the block of the description the platform writes: what the
// declared baseline job is, from the platform's own records. The brief is all a researcher reads,
// and a coordinator that copied these lines into it by hand got one of them wrong and waited.
const MeasuredBaselineHeader = "MEASURED BASELINE (written by the platform from the declared job)"

// baselineBlock is the block for baseline, with the last value it reported on each of metrics
// in declared order but the references, the target of each metric that declares one, the
// metrics declared deterministic, then each value a completed job reported on a reference
// and whether that job met the constraints, and the references no job reported.
func baselineBlock(baseline *Baseline, metrics []domain.MetricDefinition) string {
	measured := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		if value, reported := baseline.Reported[metric.Key]; reported && metric.EffectiveRole() != domain.MetricRoleReference {
			measured = append(measured, fmt.Sprintf("%s = %g", metric.Key, value))
		}
	}
	lines := []string{MeasuredBaselineHeader, "job: " + baseline.Job.ID, "code_ref: " + baseline.Job.CodeRef}
	if env := baseline.Job.Job.Env; len(env) > 0 {
		settings := make([]string, 0, len(env))
		for _, key := range slices.Sorted(maps.Keys(env)) {
			settings = append(settings, key+"="+env[key])
		}
		lines = append(lines, "env: "+strings.Join(settings, " "))
	}
	lines = append(lines, "measured: "+strings.Join(measured, ", "))
	for _, metric := range metrics {
		if value, reported := baseline.Reported[metric.Key]; reported && metric.TargetVsBaseline != nil {
			if target := metric.Target(value); target != nil {
				lines = append(lines, fmt.Sprintf("target: %s = %g (%+g%% of the baseline's %g)", metric.Key, *target, *metric.TargetVsBaseline*100, value))
			}
		}
	}
	var deterministic []string
	for _, metric := range metrics {
		if metric.Deterministic {
			deterministic = append(deterministic, metric.Key)
		}
	}
	if len(deterministic) > 0 {
		lines = append(lines, "deterministic: "+strings.Join(deterministic, ", ")+" (the same code and data give the same value on any machine: compared across machines, with no repeat)")
	}
	var unmeasured []string
	for _, reference := range baseline.References {
		if len(reference.Values) == 0 {
			unmeasured = append(unmeasured, reference.Metric)
		}
		for _, v := range reference.Values {
			held := map[string]string{
				metricsdb.ConstraintsHeld:   "constraints met",
				metricsdb.ConstraintsBroken: "a constraint broken, so it does not count",
			}[v.ConstraintsMet]
			if held == "" {
				held = "a constraint not reported, so it does not count"
			}
			lines = append(lines, fmt.Sprintf("reference %s = %g (job %s, %s)", reference.Metric, v.Value, v.ExperimentID, held))
		}
	}
	if len(unmeasured) > 0 {
		lines = append(lines, "references not measured: "+strings.Join(unmeasured, ", "))
	}
	return strings.Join(lines, "\n")
}

// splitMeasuredBaseline parts description into what its author wrote and the block the platform
// wrote into it, empty when it holds none. The block runs from its header to the next empty line.
func splitMeasuredBaseline(description string) (authored, block string) {
	lines := strings.Split(description, "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) == MeasuredBaselineHeader })
	if start < 0 {
		return description, ""
	}
	end := start
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	before, after := strings.TrimSpace(strings.Join(lines[:start], "\n")), strings.TrimSpace(strings.Join(lines[end:], "\n"))
	authored = before
	if before != "" && after != "" {
		authored += "\n\n"
	}
	return authored + after, strings.Join(lines[start:end], "\n")
}

// withMeasuredBaseline is description ending in block, in place of any block it held.
func withMeasuredBaseline(description, block string) string {
	authored, _ := splitMeasuredBaseline(description)
	if block == "" {
		return authored
	}
	return authored + "\n\n" + block
}

// budgetCoversTrials refuses a budget that cannot hold one trial for each researcher. A trial is
// the baseline's job submitted again with no estimate stated, sized as the submit sizes it, so
// the floor is what the first job of each researcher will hold. A baseline too short to be
// observed leaves no estimate and no floor.
func budgetCoversTrials(pe *domain.PlatformExperiment, baseline *domain.Experiment) error {
	held, err := trialHeld(pe, baseline)
	if err != nil {
		return err
	}
	if floor := held * float64(pe.MaxAgents); pe.BudgetAcceleratorHours < floor {
		return refusedBaseline("budget_below_baseline", "a trial holds %.3f accelerator-hours of its researcher's share (%g h, the estimate measured on the baseline job), so %d researchers each running one need %.3f, and the budget is %.3f: its owner raises budget_accelerator_hours or lowers max_agents", held, pe.JobEstimatedDurationHours, pe.MaxAgents, floor, pe.BudgetAcceleratorHours)
	}
	return nil
}

// trialHeld is what one trial of baseline holds of a researcher's share (budgetCoversTrials), 0
// when the baseline left no estimate.
func trialHeld(pe *domain.PlatformExperiment, baseline *domain.Experiment) (float64, error) {
	trial := *baseline
	trial.EstimatedDurationHours = 0
	if !trial.TakeEstimate(pe) {
		return 0, nil
	}
	held, err := trial.EstimateHeld()
	if err != nil {
		return 0, fmt.Errorf("platform_experiments: size a trial of the baseline: %w", err)
	}
	return held, nil
}

// runningTrialHeld is what one trial of the running pe's baseline job holds, 0 without one.
func (s *PlatformExperimentsService) runningTrialHeld(ctx context.Context, pe *domain.PlatformExperiment) (float64, error) {
	if pe.BaselineJobID == "" {
		return 0, nil
	}
	baseline, err := s.baseline(ctx, pe, pe.BaselineJobID)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments: read baseline %s: %w", pe.BaselineJobID, err)
	}
	if baseline == nil {
		return 0, nil
	}
	return trialHeld(pe, baseline.Job)
}
