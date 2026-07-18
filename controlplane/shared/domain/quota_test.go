package domain

import "testing"

func TestDominantUtilizationExcludesUntrackedDimensions(t *testing.T) {
	// Agent has exhausted GPU quota but never had a CPU quota tracked at all — a CPU-only job
	// must not read this as "0% utilized" just because guaranteed_cpu_core_hours is 0/0.
	aq := &AgentQuota{
		GuaranteedT4Hours: 10, UsedGuaranteedT4H: 10, // fully used
		GuaranteedCPUCoreHours: 0, UsedGuaranteedCPUCoreH: 0, // untracked
	}
	cpuOnlyJob := &Experiment{EstimatedCostT4H: 0, EstimatedCPUCoreHours: 5}
	if got := aq.DominantUtilization(cpuOnlyJob); got != 0 {
		t.Errorf("DominantUtilization = %v, want 0 (CPU untracked, GPU not requested)", got)
	}

	gpuJob := &Experiment{EstimatedCostT4H: 2, EstimatedCPUCoreHours: 0}
	if got := aq.DominantUtilization(gpuJob); got != 1.0 {
		t.Errorf("DominantUtilization = %v, want 1.0 (GPU fully used and requested)", got)
	}
}

func TestDominantUtilizationTakesMaxAcrossRequestedDimensions(t *testing.T) {
	aq := &AgentQuota{
		GuaranteedT4Hours: 10, UsedGuaranteedT4H: 2, // 20%
		GuaranteedCPUCoreHours: 10, UsedGuaranteedCPUCoreH: 8, // 80%
	}
	mixedJob := &Experiment{EstimatedCostT4H: 1, EstimatedCPUCoreHours: 1}
	if got := aq.DominantUtilization(mixedJob); got != 0.8 {
		t.Errorf("DominantUtilization = %v, want 0.8 (max of 20%% GPU, 80%% CPU)", got)
	}
}

func TestDominantCostFractionComparableAcrossDimensions(t *testing.T) {
	aq := &AgentQuota{
		GuaranteedT4Hours:      10,
		GuaranteedCPUCoreHours: 100,
	}
	smallGPUJob := &Experiment{EstimatedCostT4H: 1}     // 10% of GPU budget
	bigCPUJob := &Experiment{EstimatedCPUCoreHours: 90} // 90% of CPU budget

	small := aq.DominantCostFraction(smallGPUJob)
	big := aq.DominantCostFraction(bigCPUJob)
	if !(small < big) {
		t.Errorf("expected smallGPUJob fraction (%v) < bigCPUJob fraction (%v)", small, big)
	}
}

func TestDominantCostFractionZeroWhenNothingTrackedOrRequested(t *testing.T) {
	aq := &AgentQuota{} // nothing tracked
	job := &Experiment{EstimatedCostT4H: 5, EstimatedCPUCoreHours: 5}
	if got := aq.DominantCostFraction(job); got != 0 {
		t.Errorf("DominantCostFraction = %v, want 0 (no dimension tracked)", got)
	}
}
