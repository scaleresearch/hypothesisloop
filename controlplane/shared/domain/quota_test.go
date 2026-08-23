package domain

import "testing"

func TestDominantUtilizationZeroWhenUntrackedOrNotRequested(t *testing.T) {
	untracked := &AgentQuota{} // guaranteed AccH never allocated
	job := &Experiment{EstimatedCostAccH: 5}
	if got := untracked.DominantUtilization(job); got != 0 {
		t.Errorf("DominantUtilization = %v, want 0 (accelerator quota untracked)", got)
	}

	tracked := &AgentQuota{GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 10}
	notRequested := &Experiment{EstimatedCostAccH: 0}
	if got := tracked.DominantUtilization(notRequested); got != 0 {
		t.Errorf("DominantUtilization = %v, want 0 (accelerator not requested)", got)
	}
}

func TestDominantUtilizationReflectsGuaranteedUsage(t *testing.T) {
	aq := &AgentQuota{GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 8} // 80%
	job := &Experiment{EstimatedCostAccH: 2}
	if got := aq.DominantUtilization(job); got != 0.8 {
		t.Errorf("DominantUtilization = %v, want 0.8", got)
	}
}

func TestDominantCostFractionComparableAcrossJobSizes(t *testing.T) {
	aq := &AgentQuota{GuaranteedAcceleratorHours: 10}
	smallJob := &Experiment{EstimatedCostAccH: 1} // 10% of accelerator budget
	bigJob := &Experiment{EstimatedCostAccH: 9}   // 90% of accelerator budget

	small := aq.DominantCostFraction(smallJob)
	big := aq.DominantCostFraction(bigJob)
	if !(small < big) {
		t.Errorf("expected smallJob fraction (%v) < bigJob fraction (%v)", small, big)
	}
}

func TestDominantCostFractionZeroWhenNothingTrackedOrRequested(t *testing.T) {
	aq := &AgentQuota{} // nothing tracked
	job := &Experiment{EstimatedCostAccH: 5}
	if got := aq.DominantCostFraction(job); got != 0 {
		t.Errorf("DominantCostFraction = %v, want 0 (no dimension tracked)", got)
	}
}
