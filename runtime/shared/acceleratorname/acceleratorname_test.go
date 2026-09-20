package acceleratorname

import "testing"

func TestNVIDIAProductLabelMatchesGPUFeatureDiscovery(t *testing.T) {
	for name, want := range map[string]string{
		"NVIDIA H100 80GB HBM3":     "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
		" NVIDIA GeForce RTX 4090 ": "nvidia.com/gpu.product=NVIDIA-GEFORCE-RTX-4090",
		"Tesla  T4":                 "nvidia.com/gpu.product=TESLA-T4",
	} {
		if got := NVIDIAProductLabel(name); got != want {
			t.Errorf("NVIDIAProductLabel(%q) = %q, want %q", name, got, want)
		}
	}
}
