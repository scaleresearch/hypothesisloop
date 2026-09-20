package domain

import "testing"

func TestAcceleratorTypesAreTheSameWhateverTheirCase(t *testing.T) {
	const reported = AcceleratorType("nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3")
	if !reported.Is("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3") || !reported.Is(reported) {
		t.Error("one type in two casings read as two types")
	}
	if reported.Is("nvidia.com/gpu.product=NVIDIA-L40") || reported.Is("") {
		t.Error("two types read as one")
	}
}
