package clusteragentapi

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAClustersOccupancyCountsCoresApartFromDevices(t *testing.T) {
	const class, chip, gpu = "cpu.hypothesisloop.dev/class=EPYC-9354", "tenstorrent.com/chipArch=blackhole", "nvidia.com/gpu.product=NVIDIA-L40"
	domain.SetAcceleratorKinds(map[string]domain.TypeKind{class: domain.TypeKindCPU})
	t.Cleanup(func() { domain.SetAcceleratorKinds(nil) })

	// A runtime reports a type in its driver's casing.
	total := map[string]int64{"cpu.hypothesisloop.dev/class=epyc-9354": 16, chip: 4, gpu: 2}
	available := map[string]int64{"cpu.hypothesisloop.dev/class=epyc-9354": 10, chip: 1}

	devices, cores := occupancyByKind(available, total)

	if devices != (occupancy{busy: 5, total: 6}) {
		t.Errorf("devices = %+v, want 5 of 6 busy: three chips and both gpus", devices)
	}
	if cores != (occupancy{busy: 6, total: 16}) {
		t.Errorf("cores = %+v, want 6 of 16 busy", cores)
	}

	// With no CPU class in the catalog the sums are what they were.
	domain.SetAcceleratorKinds(nil)
	if devices, cores := occupancyByKind(map[string]int64{chip: 1}, map[string]int64{chip: 4, gpu: 2}); devices != (occupancy{busy: 5, total: 6}) || cores != (occupancy{}) {
		t.Errorf("without a CPU class: devices = %+v, cores = %+v, want 5 of 6 and none", devices, cores)
	}
}
