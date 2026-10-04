package quota

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Baseline is what a platform experiment's run starts from and is ranked against, the one form
// everything that reads the baseline reads: what its baseline job is known to have done, read
// from the platform's own records and never from what anyone says about it, or the values its
// creator stated.
type Baseline struct {
	// Job is the job that measured it, nil for a stated one. OwnerRole, Finding and
	// ObservedHours are the job's.
	Job *domain.Experiment
	// OwnerRole is what the job's agent was signed up as in the job's platform experiment.
	OwnerRole domain.SignupRole
	// Finding says what the job found was filed.
	Finding bool
	// Values is the value of each metric key: the last one the job reported, or the stated one.
	Values map[string]float64
	// ObservedHours is how long the job ran, as it was billed. A job too short to be observed
	// alive has none.
	ObservedHours float64
}

// statedBaseline is the Baseline of pe's stated values.
func statedBaseline(pe *domain.PlatformExperiment) *Baseline {
	return &Baseline{Values: pe.StatedBaseline}
}

// baselineHeadroom is how far over the baseline's observed hours the estimate of a job that
// states none is set. A job holds its whole estimate of its agent's quota until it ends: an
// estimate of 2.2 times the run let one job at a time fit a researcher's share, and one under
// the run reserves less than the job spends.
const baselineHeadroom = 1.5

// baselineReader reads the Baseline of the job a declaration names, nil when there is no such job.
type baselineReader func(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error)

func (s *PlatformExperimentsService) readBaseline(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error) {
	job, err := s.store.GetExperiment(ctx, jobID)
	if err != nil || job == nil {
		return nil, err
	}
	out := &Baseline{Job: job, Values: map[string]float64{}}
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
		out.Values[sample.Labels["metric_name"]] = sample.Value
	}
	cost, err := s.JobCost(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if cost != nil {
		out.ObservedHours = cost.ObservedHours
	}
	return out, nil
}

// refusedBaseline is a baseline declaration the caller can fix: reason names what is wrong.
func refusedBaseline(reason, format string, args ...any) error {
	return refuseUpdate(422, reason, format, args...)
}

// checkBaselineJob holds pe, as it would be stored, to what the job it names as its baseline
// really did. Every researcher starts from the baseline and is ranked against it, so one whose
// measurement did not happen is refused.
func checkBaselineJob(pe *domain.PlatformExperiment, baseline *Baseline) error {
	if baseline == nil || baseline.Job == nil || baseline.Job.PlatformExperimentID != pe.ID || baseline.OwnerRole != domain.SignupRoleBaseline {
		return refusedBaseline("no_baseline", "baseline_job_id %q is no job of a baseline agent of this platform experiment: an agent added with role baseline runs it with `hl job submit`, and that job is the one to name", pe.BaselineJobID)
	}
	if baseline.Job.Status != domain.StatusCompleted {
		return refusedBaseline("baseline_not_completed", "baseline job %s is %s: a run rests on a baseline that ran to the end. Wait for it, or have it fixed and run again", baseline.Job.ID, baseline.Job.Status)
	}
	if !baseline.Finding {
		return refusedBaseline("baseline_without_finding", "baseline job %s has no finding: its agent files what it measured with `hl job summary`, so the researchers start from it", baseline.Job.ID)
	}
	return nil
}

// checkBaselineValues holds pe's metrics to the values of its baseline: a job has reported every
// metric the harness reports, a stated baseline gives each ranking metric, and none fails a gate.
func checkBaselineValues(pe *domain.PlatformExperiment, baseline *Baseline) error {
	var missing []string
	for _, metric := range pe.Metrics {
		value, given := baseline.Values[metric.Key]
		if !given {
			if baseline.Job != nil || metric.EffectiveRole() == domain.MetricRoleRanking {
				missing = append(missing, metric.Key)
			}
			continue
		}
		if metric.EffectiveRole() == domain.MetricRoleConstraint && !metric.Holds(value) {
			return refusedBaseline("baseline_breaks_constraint", "the baseline's %s is %g, which does not hold the bound %g of the metric: a gate the baseline fails disqualifies everyone, and only its owner changes it (PATCH metrics)", metric.Key, value, *metric.Bound)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	if baseline.Job == nil {
		return refusedBaseline("baseline_missing_metric", "stated_baseline has no value of %s: a stated baseline gives one for every ranking metric", strings.Join(missing, ", "))
	}
	return refusedBaseline("baseline_missing_metric", "baseline job %s reported no %s: every metric of the platform experiment must be one the harness really reports", baseline.Job.ID, strings.Join(missing, ", "))
}

// checkStatedBaseline refuses a stated baseline with a value no metric of pe has, or one that is
// no finite number.
func checkStatedBaseline(pe *domain.PlatformExperiment) error {
	for _, key := range slices.Sorted(maps.Keys(pe.StatedBaseline)) {
		if !slices.ContainsFunc(pe.Metrics, func(metric domain.MetricDefinition) bool { return metric.Key == key }) {
			return refusedBaseline("invalid_baseline", "stated_baseline gives %s, which is no metric of the platform experiment: give a value of each ranking metric", key)
		}
		if value := pe.StatedBaseline[key]; math.IsNaN(value) || math.IsInf(value, 0) {
			return refusedBaseline("invalid_baseline", "stated_baseline gives %s no finite number", key)
		}
	}
	return checkBaselineValues(pe, statedBaseline(pe))
}

// baselineHeader opens the block of the description the platform writes: what the baseline is,
// from the platform's own records. The brief is all a researcher reads, and a coordinator that
// copied these lines into it by hand got one of them wrong and waited.
const baselineHeader = "BASELINE (written by the platform)"

// baselineBlock is the block for the baseline of pe, with its value on each metric in declared
// order: the job, its code_ref and what it measured, or the code_ref and what was stated.
func baselineBlock(baseline *Baseline, pe *domain.PlatformExperiment) string {
	values := make([]string, 0, len(pe.Metrics))
	for _, metric := range pe.Metrics {
		if value, given := baseline.Values[metric.Key]; given {
			values = append(values, fmt.Sprintf("%s = %g", metric.Key, value))
		}
	}
	if baseline.Job == nil {
		return strings.Join([]string{baselineHeader, "code_ref: " + pe.BaselineCodeRef, "stated by its creator: " + strings.Join(values, ", "),
			"no job measured it, so pass --estimated-duration-hours H on every hl job submit."}, "\n")
	}
	return strings.Join([]string{baselineHeader, "job: " + baseline.Job.ID, "code_ref: " + baseline.Job.CodeRef,
		"measured: " + strings.Join(values, ", ")}, "\n")
}

// splitBaselineBlock parts description into what its author wrote and the block the platform
// wrote into it, empty when it holds none. The block runs from its header to the next empty line.
func splitBaselineBlock(description string) (authored, block string) {
	lines := strings.Split(description, "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) == baselineHeader })
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

// withBaselineBlock is description ending in block, in place of any block it held.
func withBaselineBlock(description, block string) string {
	authored, _ := splitBaselineBlock(description)
	if block == "" {
		return authored
	}
	return authored + "\n\n" + block
}

// budgetCoversTrials refuses a budget that cannot hold one trial for each researcher. A trial is
// the baseline's job submitted again with no estimate stated, sized as the submit sizes it, so
// the floor is what the first job of each researcher will hold. A baseline too short to be
// observed leaves no estimate and no floor, and so does a stated one, which no job measured.
func budgetCoversTrials(pe *domain.PlatformExperiment, baseline *domain.Experiment) error {
	trial := *baseline
	trial.EstimatedDurationHours = 0
	if !trial.TakeEstimate(pe) {
		return nil
	}
	held, err := trial.EstimateHeld()
	if err != nil {
		return fmt.Errorf("platform_experiments.Update: size a trial of the baseline: %w", err)
	}
	if floor := held * float64(pe.MaxAgents); pe.BudgetAcceleratorHours < floor {
		return refusedBaseline("budget_below_baseline", "a trial holds %.3f accelerator-hours of its researcher's share (%g h, the estimate measured on the baseline job), so %d researchers each running one need %.3f, and the budget is %.3f: its owner raises budget_accelerator_hours or lowers max_agents", held, trial.EstimatedDurationHours, pe.MaxAgents, floor, pe.BudgetAcceleratorHours)
	}
	return nil
}
