package clusteragentapi

import (
	"reflect"
	"strings"
	"testing"
)

// Agents report every accelerator type their hardware publishes; capacity keeps only the priced
// ones, matched case-insensitively.
func TestReconcileIngestKeepsOnlyPricedAcceleratorTypes(t *testing.T) {
	h := NewHandler(nil, 0, "", nil, 0, []string{"nvidia.com/gpu.product=Tesla-T4"}, 10, nil)
	report := capacityReport{
		AcceleratorAvailableByFlavor: map[string]int64{"nvidia.com/gpu.product=tesla-t4": 1, "nvidia.com/cuda.driver.major=550": 1},
		AcceleratorTotalByFlavor:     map[string]int64{"nvidia.com/gpu.product=tesla-t4": 2, "nvidia.com/cuda.driver.major=550": 2},
		AcceleratorAvailableByNode: map[string]map[string]int64{
			"node-a": {"nvidia.com/gpu.product=tesla-t4": 1, "nvidia.com/cuda.driver.major=550": 1},
		},
	}
	h.keepPricedAccelerators(&report)
	want := map[string]int64{"nvidia.com/gpu.product=tesla-t4": 1}
	if !reflect.DeepEqual(report.AcceleratorAvailableByFlavor, want) ||
		!reflect.DeepEqual(report.AcceleratorTotalByFlavor, map[string]int64{"nvidia.com/gpu.product=tesla-t4": 2}) ||
		!reflect.DeepEqual(report.AcceleratorAvailableByNode["node-a"], want) {
		t.Fatalf("filtered report = %+v", report)
	}
}

func TestStatusIngestSplitsLongLogLinesWithoutDroppingAnything(t *testing.T) {
	got := splitLongLines([]string{"short", strings.Repeat("x", 25)}, 10)
	want := []string{"short", strings.Repeat("x", 10), strings.Repeat("x", 10), strings.Repeat("x", 5)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split = %q, want %q", got, want)
	}
}
