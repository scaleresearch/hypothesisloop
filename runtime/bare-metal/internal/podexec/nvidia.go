package podexec

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// probeNVIDIA discovers local NVIDIA accelerator hardware via NVML (libnvidia-ml.so), the same
// library nvidia-smi itself is a thin CLI wrapper around — no shelling out, no output parsing.
// The label produced here must be byte-identical to GPU Feature Discovery's own normalization, or
// a submitted job's accelerator_type matches nothing and queues forever. A node with no NVIDIA
// driver installed simply reports no accelerators (an empty, valid inventory) rather than
// erroring, since most bare nodes have none.
func probeNVIDIA(_ context.Context) ([]acceleratorDevice, error) {
	switch ret := nvml.Init(); ret {
	case nvml.SUCCESS:
		defer nvml.Shutdown()
	case nvml.ERROR_LIBRARY_NOT_FOUND, nvml.ERROR_DRIVER_NOT_LOADED:
		return nil, nil
	default:
		return nil, fmt.Errorf("podexec: nvml.Init: %v", nvml.ErrorString(ret))
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("podexec: nvml.DeviceGetCount: %v", nvml.ErrorString(ret))
	}
	// One sick GPU must not take the node's whole inventory with it (important.md #19): capacity
	// is probed before desired state is reconciled, so an error here stops deletes as well as
	// admissions. A skipped device under-reports capacity, which can only cost throughput; a
	// failed probe reports none at all.
	devices := make([]acceleratorDevice, 0, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: DeviceGetHandleByIndex: %v", i, nvml.ErrorString(ret))
			continue
		}
		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: GetName: %v", i, nvml.ErrorString(ret))
			continue
		}
		// The device node's minor number, not the NVML enumeration index. The two agree on a
		// freshly booted node and diverge after a driver reload or a hot-plug, and occupancy is
		// tracked by /dev path — so booking against the index puts a job's claim on a device it
		// is not using and leaves the one it is using looking free.
		minor, ret := device.GetMinorNumber()
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: GetMinorNumber: %v", i, nvml.ErrorString(ret))
			continue
		}
		devices = append(devices, acceleratorDevice{
			DevicePath: fmt.Sprintf("/dev/nvidia%d", minor),
			Labels:     []string{"nvidia.com/gpu.product=" + normalizeGFDProductName(name)},
		})
	}
	return devices, nil
}

// normalizeGFDProductName reproduces NVIDIA GPU Feature Discovery's node-label normalization
// exactly (different-backend.md §4.2): uppercase, spaces to dashes. "NVIDIA H100 80GB HBM3" ->
// "NVIDIA-H100-80GB-HBM3".
func normalizeGFDProductName(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.Join(strings.Fields(upper), "-")
}
