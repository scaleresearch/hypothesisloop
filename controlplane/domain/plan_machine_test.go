package domain

import "testing"

// A baseline job measured on another machine than the plan's measures nothing the run is ranked
// on: what differs is named, quantities compared as quantities.
func TestAJobIsComparedWithThePlansMachine(t *testing.T) {
	m, ok := PlanMachine([]byte(`{"accelerator_type":"nvidia.com/gpu.product=A","cpu":"8","memory":"32Gi","storage":"100Gi"}`))
	if !ok {
		t.Fatal("no machine read")
	}
	same := JobSpec{AcceleratorType: "nvidia.com/gpu.product=A", AcceleratorCount: 1, CPU: "8000m", Memory: "32Gi", Storage: "100Gi"}
	if got := m.Differs(same); got != "" {
		t.Errorf("the plan's machine differs in %q", got)
	}
	other := JobSpec{CPU: "4", Memory: "32Gi", Storage: "100Gi"}
	if got := m.Differs(other); got != "accelerator_type "+string(CPUType)+", cpu 4" {
		t.Errorf("differs = %q", got)
	}
	if cpu, ok := PlanMachine([]byte(`{"cpu":"4","memory":"8Gi","storage":"8Gi"}`)); !ok || cpu.AcceleratorType != CPUType || cpu.Differs(JobSpec{CPU: "4", Memory: "8Gi", Storage: "8Gi"}) != "" {
		t.Errorf("a plan on CPU cores: %+v, %v", cpu, ok)
	}
	if _, ok := PlanMachine(nil); ok {
		t.Error("no proposal states a machine")
	}
}
