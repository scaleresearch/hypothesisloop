package fly

import (
	"fmt"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const (
	bytesPerMB = 1 << 20
	bytesPerGB = 1 << 30

	// maxRootfsGB is the largest root filesystem the API accepts. A Machine has no other disk
	// unless a volume is attached, which this adapter does not do.
	maxRootfsGB = 50
)

type guest struct {
	CPUKind  string `json:"cpu_kind"`
	CPUs     int64  `json:"cpus"`
	MemoryMB int64  `json:"memory_mb"`
}

// guestRule is what the API accepts for one CPU kind: the core counts, and per core the least
// and the most memory, in steps of StepMB.
type guestRule struct {
	CPUs      []int64
	MinPerCPU int64
	MaxPerCPU int64
	StepMB    int64
}

var guestRules = map[string]guestRule{
	"shared":      {CPUs: []int64{1, 2, 4, 6, 8}, MinPerCPU: 256, MaxPerCPU: 2048, StepMB: 256},
	"performance": {CPUs: []int64{1, 2, 4, 6, 8, 10, 12, 14, 16}, MinPerCPU: 2048, MaxPerCPU: 8192, StepMB: 1024},
}

// guestFor picks the smallest Machine of the configured CPU kind that gives the spec at least
// the cores and the memory it asks for. Memory is rounded up to the vendor's step and to the
// least a core count comes with; a spec whose memory no core count can hold is refused.
func (c Settings) guestFor(spec provider.Spec) (guest, error) {
	rule := guestRules[c.CPUKind]
	memory := provider.CeilDiv(provider.CeilDiv(spec.MemoryBytes, bytesPerMB), rule.StepMB) * rule.StepMB
	for _, cpus := range rule.CPUs {
		if cpus*1000 < spec.CPUMillis || cpus*rule.MaxPerCPU < memory {
			continue
		}
		return guest{CPUKind: c.CPUKind, CPUs: cpus, MemoryMB: max(memory, cpus*rule.MinPerCPU)}, nil
	}
	largest := rule.CPUs[len(rule.CPUs)-1]
	return guest{}, fmt.Errorf("fly: spec %q asks for %d millicores and %d MB of memory, and the largest %s machine has %d cores and %d MB",
		spec.Name, spec.CPUMillis, provider.CeilDiv(spec.MemoryBytes, bytesPerMB), c.CPUKind, largest, largest*rule.MaxPerCPU)
}

func rootfsGB(spec provider.Spec) (int64, error) {
	size := provider.CeilDiv(spec.DiskBytes, bytesPerGB)
	if size > maxRootfsGB {
		return 0, fmt.Errorf("fly: spec %q asks for %d GB of disk, and a machine's root filesystem holds at most %d GB", spec.Name, size, maxRootfsGB)
	}
	return size, nil
}
