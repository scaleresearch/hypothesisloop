package scheduler

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// describeFootprint says a footprint as a person reads it: devices by their type, cores, and
// memory and storage in binary units. The cores of a job on CPU are its units of domain.CPUType
// and its cpu both, and are said once where the two agree.
func describeFootprint(fp domain.Footprint) string {
	var devices, sizes []string
	cpu, statesCPU := fp[domain.ResourceKey{Kind: domain.ResourceKindCPU}]
	for key, amount := range fp {
		switch key.Kind {
		case domain.ResourceKindAccelerator:
			if statesCPU && amount*1000 == cpu && domain.AcceleratorType(key.Flavor).Kind() == domain.TypeKindCPU {
				continue
			}
			devices = append(devices, strconv.FormatInt(amount, 10)+" x "+key.Flavor)
		case domain.ResourceKindExtended:
			devices = append(devices, strconv.FormatInt(amount, 10)+" x "+key.Flavor)
		}
	}
	sort.Strings(devices)
	if amount, ok := fp[domain.ResourceKey{Kind: domain.ResourceKindCPU}]; ok {
		sizes = append(sizes, describeCores(amount))
	}
	if amount, ok := fp[domain.ResourceKey{Kind: domain.ResourceKindMemory}]; ok {
		sizes = append(sizes, describeBytes(amount)+" memory")
	}
	if amount, ok := fp[domain.ResourceKey{Kind: domain.ResourceKindStorage}]; ok {
		sizes = append(sizes, describeBytes(amount)+" storage")
	}
	return strings.Join(append(devices, sizes...), ", ")
}

func describeCores(millicores int64) string {
	if millicores == 1000 {
		return "1 core"
	}
	return strconv.FormatFloat(float64(millicores)/1000, 'f', -1, 64) + " cores"
}

// describeBytes rounds to one decimal of the largest binary unit the amount fills.
func describeBytes(bytes int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value, unit := float64(bytes), 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + units[unit]
}

// describeNodeResources says cpu, memory and storage keyed by domain.NodeResource*.
func describeNodeResources(resources map[string]int64) string {
	fp := domain.NewFootprint()
	for name, amount := range resources {
		if kind, ok := nodeResourceKind(name); ok {
			fp.Add(domain.ResourceKey{Kind: kind}, amount)
		}
	}
	return describeFootprint(fp)
}
