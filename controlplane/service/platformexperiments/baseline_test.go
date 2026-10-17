package platformexperiments

import (
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func openExperiment() *domain.PlatformExperiment {
	bound := 1.0
	return &domain.PlatformExperiment{
		ID: "pe-1", ProjectID: "prj-a", Name: "n", Description: "Train train.py and report val_loss.\nBASELINE\n  config: lr 0.1\n  measured: not yet established",
		Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 6, MaxAgents: 2, ReportIntervalSeconds: 30,
		StartsAt: time.Now().UTC(), DurationHours: 4, BaselineDueAt: time.Now().UTC().Add(time.Hour), Stages: []domain.Stage{{LengthPct: 100}},
		Metrics: []domain.MetricDefinition{{Key: "val_loss", Direction: "minimize"}, {Key: "exact_match", Direction: "maximize", Role: domain.MetricRoleConstraint, Bound: &bound}},
	}
}

func measuredBaseline() *Baseline {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0})
	return &Baseline{
		Job: &domain.Experiment{ID: "job-1", AgentID: "ag-baseline", PlatformExperimentID: "pe-1", Branch: "hl/pe-1/baseline", Status: domain.StatusCompleted, CodeRef: "git://seed@abc",
			AcceleratorType: "h100", AcceleratorCount: 2, EstimatedDurationHours: 5},
		OwnerRole: domain.SignupRoleBaseline, Finding: true, Reported: map[string]float64{"val_loss": 1.8, "exact_match": 1}, ObservedHours: 0.2,
	}
}

// theMeasuredBlock is what the platform writes into the brief of an experiment whose baseline is
// measuredBaseline.
const theMeasuredBlock = "\n\n" + MeasuredBaselineHeader + "\njob: job-1\ncode_ref: git://seed@abc\nmeasured: val_loss = 1.8, exact_match = 1"

// A metric's target_vs_baseline reaches the brief as the value it sets against what the
// baseline measured, so every agent reads the goal as a number.
func TestTheBriefStatesTheTargetAMetricSetsAgainstTheBaseline(t *testing.T) {
	pe := openExperiment()
	lower := -0.25
	pe.Metrics[0].TargetVsBaseline = &lower
	block := baselineBlock(measuredBaseline(), pe.Metrics)
	if want := "\ntarget: val_loss = 1.35 (-25% of the baseline's 1.8)"; !strings.Contains(block, want) {
		t.Fatalf("block %q, want it to hold %q", block, want)
	}
	if block := baselineBlock(measuredBaseline(), openExperiment().Metrics); strings.Contains(block, "target:") {
		t.Fatalf("block %q states a target no metric declares", block)
	}
}

// A metric declared deterministic reaches the brief, so no agent repeats a run of it or looks for
// a control on its own machine.
func TestTheBriefNamesTheMetricsDeclaredDeterministic(t *testing.T) {
	pe := openExperiment()
	pe.Metrics[0].Deterministic = true
	block := baselineBlock(measuredBaseline(), pe.Metrics)
	if want := "\ndeterministic: " + pe.Metrics[0].Key + " ("; !strings.Contains(block, want) {
		t.Fatalf("block %q, want it to hold %q", block, want)
	}
	if block := baselineBlock(measuredBaseline(), openExperiment().Metrics); strings.Contains(block, "deterministic:") {
		t.Fatalf("block %q names a metric nobody declared deterministic", block)
	}
}
