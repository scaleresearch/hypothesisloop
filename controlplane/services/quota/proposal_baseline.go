package quota

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Baseline is what a draft's baseline job is known to have done, read from the platform's own
// records and never from what the planner says about it.
type Baseline struct {
	Job *domain.Experiment
	// Finding says the planner filed what the job found.
	Finding bool
	// Reported is the last value the job reported for each metric key.
	Reported map[string]float64
	// AcceleratorHours is what the job cost.
	AcceleratorHours float64
	// Reachable are the accelerator types the org's jobs can be placed on now.
	Reachable map[string]bool
}

// baselineReader reads the Baseline of the job a proposal names, nil when there is no such job.
type baselineReader func(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error)

func (h *PlatformExperimentsHandler) readBaseline(ctx context.Context, pe *domain.PlatformExperiment, jobID string) (*Baseline, error) {
	job, err := h.svc.store.GetExperiment(ctx, jobID)
	if err != nil || job == nil {
		return nil, err
	}
	out := &Baseline{Job: job, Reported: map[string]float64{}, Reachable: map[string]bool{}}
	if out.Finding, err = h.svc.store.JobHasFinding(ctx, jobID); err != nil {
		return nil, err
	}
	samples, err := metricsdb.ExperimentMetricSamples(ctx, h.svc.metricsDBURL, jobID, pe.CreatedAt, time.Now().UTC().Add(time.Minute))
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
	cost, err := h.svc.JobCost(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if cost != nil {
		out.AcceleratorHours = cost.AcceleratorHours
	}
	clusters, err := h.liveCapacity(ctx, job.OrgID, false)
	if err != nil {
		return nil, err
	}
	for _, cluster := range clusters {
		for _, accelerator := range cluster.Accelerators {
			if accelerator.Total > 0 {
				out.Reachable[accelerator.AcceleratorType] = true
			}
		}
	}
	return out, nil
}

// checkBaseline holds a proposal to what its baseline job really did. A person reads the
// proposal's numbers as measured, so a proposal whose measurement did not happen is refused.
func checkBaseline(p Proposal, experiment CreatePlatformExperimentRequest, planner string, baseline *Baseline) error {
	if baseline == nil || baseline.Job == nil || baseline.Job.AgentID != planner {
		return refusedProposal("no_baseline", "baseline_job_id %q is no job of yours in this draft: run the baseline with `hl job submit` and name that job", p.BaselineJobID)
	}
	if baseline.Job.Status != domain.StatusCompleted {
		return refusedProposal("baseline_not_completed", "baseline job %s is %s: a proposal rests on a baseline that ran to the end. Wait for it, or fix it and run it again", baseline.Job.ID, baseline.Job.Status)
	}
	if !baseline.Finding {
		return refusedProposal("baseline_without_finding", "baseline job %s has no finding: file what it measured with `hl hypothesis finding`, so the researchers start from it", baseline.Job.ID)
	}
	var missing []string
	for _, metric := range experiment.Metrics {
		value, reported := baseline.Reported[metric.Key]
		if !reported {
			missing = append(missing, metric.Key)
			continue
		}
		if metric.EffectiveRole() != domain.MetricRoleConstraint {
			continue
		}
		holds := value >= *metric.Bound
		if metric.Direction == "minimize" {
			holds = value <= *metric.Bound
		}
		if !holds {
			return refusedProposal("baseline_breaks_constraint", "the baseline reported %s = %g, which does not hold the bound %g you propose: a gate the baseline fails disqualifies everyone", metric.Key, value, *metric.Bound)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return refusedProposal("baseline_missing_metric", "baseline job %s reported no %s: every metric you propose must be one the harness really reports", baseline.Job.ID, strings.Join(missing, ", "))
	}
	if p.AcceleratorType != "" && !baseline.Reachable[p.AcceleratorType] {
		return refusedProposal("accelerator_unreachable", "no cluster this org's jobs can be placed on has a %s now: propose a type `hl capacity` lists", p.AcceleratorType)
	}
	if floor := baseline.AcceleratorHours * float64(len(p.Agents)); p.BudgetAcceleratorHours < floor {
		return refusedProposal("budget_below_baseline", "the baseline cost %.3f accelerator-hours, so %d researchers each repeating it once need %.3f, and you propose %.3f", baseline.AcceleratorHours, len(p.Agents), floor, p.BudgetAcceleratorHours)
	}
	return nil
}
