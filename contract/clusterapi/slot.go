package clusterapi

import "github.com/scaleresearch/hypothesisloop/contract/jobspec"

// SlotDevices is how many accelerators a slot that takes a node of group has: the group's, and
// none for a job on cores, which counts its cores and holds no device.
func SlotDevices(acceleratorType AcceleratorType, group JobGroup) int {
	if acceleratorType.Kind() == TypeKindCPU {
		return 0
	}
	return group.AcceleratorCount
}

// SlotTakes says whether a slot of devices accelerators, these labels and this size, by
// NodeResource* key, takes a node of group of a job on acceleratorType: its accelerators are
// exactly SlotDevices of that type, and its size is positive and at least every quantity the group
// names. It is the one rule admission places a node on a slot by, and a runtime renting slots
// counts its demand by.
func SlotTakes(acceleratorType AcceleratorType, group JobGroup, devices int, labels map[string]string, size map[string]int64) bool {
	want := SlotDevices(acceleratorType, group)
	if devices != want || (want > 0 && !acceleratorType.MatchesLabels(labels)) {
		return false
	}
	for key, quantity := range map[string]string{NodeResourceCPUMillicores: group.CPU, NodeResourceMemoryBytes: group.Memory, NodeResourceStorageBytes: group.Storage} {
		parsed, err := jobspec.ParseQuantity(quantity)
		if err != nil || size[key] <= 0 {
			return false
		}
		amount := parsed.Value()
		if key == NodeResourceCPUMillicores {
			amount = parsed.MilliValue()
		}
		if amount > size[key] {
			return false
		}
	}
	return true
}
