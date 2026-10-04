package quota

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
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
	if baseline.Job.Status != domain.StatusCompleted {
		return refusedBaseline("baseline_not_completed", "baseline job %s is %s: a run rests on a baseline that ran to the end. Wait for it, or have it fixed and run again", baseline.Job.ID, baseline.Job.Status)
	}
	if !baseline.Finding {
		return refusedBaseline("baseline_without_finding", "baseline job %s has no finding: its agent files what it measured with `hl job summary`, so the researchers start from it", baseline.Job.ID)
	}
	var missing []string
	for _, metric := range pe.Metrics {
		value, reported := baseline.Reported[metric.Key]
		if !reported {
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
	return nil
}

// measuredBaselineHeader opens the block of the description the platform writes: what the
// declared baseline job is, from the platform's own records. The brief is all a researcher reads,
// and a coordinator that copied these lines into it by hand got one of them wrong and waited.
const measuredBaselineHeader = "MEASURED BASELINE (written by the platform from the declared job)"

// baselineBlock is the block for baseline, with the last value it reported on each of metrics
// in declared order.
func baselineBlock(baseline *Baseline, metrics []domain.MetricDefinition) string {
	measured := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		if value, reported := baseline.Reported[metric.Key]; reported {
			measured = append(measured, fmt.Sprintf("%s = %g", metric.Key, value))
		}
	}
	return strings.Join([]string{measuredBaselineHeader, "job: " + baseline.Job.ID, "code_ref: " + baseline.Job.CodeRef,
		"measured: " + strings.Join(measured, ", ")}, "\n")
}

// splitMeasuredBaseline parts description into what its author wrote and the block the platform
// wrote into it, empty when it holds none. The block runs from its header to the next empty line.
func splitMeasuredBaseline(description string) (authored, block string) {
	lines := strings.Split(description, "\n")
	start := slices.IndexFunc(lines, func(line string) bool { return strings.TrimSpace(line) == measuredBaselineHeader })
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
