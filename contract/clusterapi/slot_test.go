package clusterapi

import "testing"

func TestSlotTakesANodeOfItsDevicesWithinItsSize(t *testing.T) {
	const h100 = AcceleratorType("nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3")
	labels := map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}
	size := map[string]int64{NodeResourceCPUMillicores: 8000, NodeResourceMemoryBytes: 32 << 30, NodeResourceStorageBytes: 100 << 30}
	group := JobGroup{AcceleratorCount: 1, CPU: "8", Memory: "32Gi", Storage: "100Gi"}
	for name, tc := range map[string]struct {
		acceleratorType AcceleratorType
		group           JobGroup
		devices         int
		labels          map[string]string
		size            map[string]int64
		want            bool
	}{
		"exactly its size":           {h100, group, 1, labels, size, true},
		"other devices":              {h100, group, 2, labels, size, false},
		"another type":               {h100, group, 1, map[string]string{"nvidia.com/gpu.product": "A100"}, size, false},
		"more cpu than it has":       {h100, JobGroup{AcceleratorCount: 1, CPU: "9", Memory: "1Gi", Storage: "1Gi"}, 1, labels, size, false},
		"a quantity left unstated":   {h100, JobGroup{AcceleratorCount: 1, CPU: "1", Memory: "1Gi"}, 1, labels, size, false},
		"a negative quantity":        {h100, JobGroup{AcceleratorCount: 1, CPU: "-1", Memory: "1Gi", Storage: "1Gi"}, 1, labels, size, false},
		"a slot of no size":          {h100, group, 1, labels, map[string]int64{NodeResourceCPUMillicores: 8000}, false},
		"cores on a slot of none":    {CPUType, JobGroup{AcceleratorCount: 4, CPU: "4", Memory: "1Gi", Storage: "1Gi"}, 0, nil, size, true},
		"cores on a slot of devices": {CPUType, JobGroup{AcceleratorCount: 4, CPU: "4", Memory: "1Gi", Storage: "1Gi"}, 1, labels, size, false},
	} {
		if got := SlotTakes(tc.acceleratorType, tc.group, tc.devices, tc.labels, tc.size); got != tc.want {
			t.Errorf("%s: SlotTakes = %v, want %v", name, got, tc.want)
		}
	}
}
