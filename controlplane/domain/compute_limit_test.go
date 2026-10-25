package domain

import "testing"

// A core reserves its share of an H100 of the platform compute limit, while its budget rate
// stays 1, so a project's limit holds thousands of cores.
func TestACoreReservesItsShareOfTheComputeLimitAndNotItsBudgetRate(t *testing.T) {
	SetAcceleratorRates(map[string]float64{string(CPUType): 1, "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3": 1})
	SetCPUCoreSharedRate(0.02)
	if got, err := AccHPerHour(CPUType, 64); err != nil || got != 1.28 {
		t.Errorf("64 cores reserve %v, %v, want 1.28", got, err)
	}
	if got, err := AccHPerHour("nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", 2); err != nil || got != 2 {
		t.Errorf("2 H100s reserve %v, %v, want 2", got, err)
	}
	if rate, _ := LookupAcceleratorCost(CPUType); rate != 1 {
		t.Errorf("a core's budget rate = %v, want 1", rate)
	}
}
