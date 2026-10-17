package clusteragentapi

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestAClustersOccupancyCountsCoresApartFromDevices(t *testing.T) {
	const chip, gpu = "tenstorrent.com/chipArch=blackhole", "nvidia.com/gpu.product=NVIDIA-L40"
	total := map[string]int64{string(domain.CPUType): 16, chip: 4, gpu: 2}
	available := map[string]int64{string(domain.CPUType): 10, chip: 1}

	devices, cores := occupancyByKind(available, total)

	if devices != (occupancy{busy: 5, total: 6}) {
		t.Errorf("devices = %+v, want 5 of 6 busy: three chips and both gpus", devices)
	}
	if cores != (occupancy{busy: 6, total: 16}) {
		t.Errorf("cores = %+v, want 6 of 16 busy", cores)
	}
}
