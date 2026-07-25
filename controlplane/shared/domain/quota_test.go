package domain

import "testing"

func TestDominantUtilizationExcludesUntrackedDimensions(t *testing.T) {
	// Accelerator quota exhausted, CPU quota never tracked — a CPU-only job must not read
	// this as 0% utilized just because guaranteed_cpu_core_hours is 0/0.
	aq := &AgentQuota{
		GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 10, // fully used
		GuaranteedCPUCoreHours: 0, UsedGuaranteedCPUCoreH: 0, // untracked
	}
	cpuOnlyJob := &Experiment{EstimatedCostAccH: 0, EstimatedCPUCoreHours: 5}
	if got := aq.DominantUtilization(cpuOnlyJob); got != 0 {
		t.Errorf("DominantUtilization = %v, want 0 (CPU untracked, accelerator not requested)", got)
	}

	acceleratorJob := &Experiment{EstimatedCostAccH: 2, EstimatedCPUCoreHours: 0}
	if got := aq.DominantUtilization(acceleratorJob); got != 1.0 {
		t.Errorf("DominantUtilization = %v, want 1.0 (Accelerator fully used and requested)", got)
	}
}

func TestDominantUtilizationTakesMaxAcrossRequestedDimensions(t *testing.T) {
	aq := &AgentQuota{
		GuaranteedAcceleratorHours: 10, UsedGuaranteedAccH: 2, // 20%
		GuaranteedCPUCoreHours: 10, UsedGuaranteedCPUCoreH: 8, // 80%
	}
	mixedJob := &Experiment{EstimatedCostAccH: 1, EstimatedCPUCoreHours: 1}
	if got := aq.DominantUtilization(mixedJob); got != 0.8 {
		t.Errorf("DominantUtilization = %v, want 0.8 (max of 20%% Accelerator, 80%% CPU)", got)
	}
}

func TestDominantCostFractionComparableAcrossDimensions(t *testing.T) {
	aq := &AgentQuota{
		GuaranteedAcceleratorHours: 10,
		GuaranteedCPUCoreHours:     100,
	}
	smallAcceleratorJob := &Experiment{EstimatedCostAccH: 1} // 10% of accelerator budget
	bigCPUJob := &Experiment{EstimatedCPUCoreHours: 90}      // 90% of CPU budget

	small := aq.DominantCostFraction(smallAcceleratorJob)
	big := aq.DominantCostFraction(bigCPUJob)
	if !(small < big) {
		t.Errorf("expected smallAcceleratorJob fraction (%v) < bigCPUJob fraction (%v)", small, big)
	}
}

func TestDominantCostFractionZeroWhenNothingTrackedOrRequested(t *testing.T) {
	aq := &AgentQuota{} // nothing tracked
	job := &Experiment{EstimatedCostAccH: 5, EstimatedCPUCoreHours: 5}
	if got := aq.DominantCostFraction(job); got != 0 {
		t.Errorf("DominantCostFraction = %v, want 0 (no dimension tracked)", got)
	}
}
