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

func TestOnlyTheCPUTypeCountsCores(t *testing.T) {
	for typ, want := range map[AcceleratorType]TypeKind{
		CPUType:                             TypeKindCPU,
		"HypothesisLoop.dev/CPU=Core":       TypeKindCPU,
		"nvidia.com/gpu.product=NVIDIA-L40": TypeKindAccelerator,
		"":                                  TypeKindAccelerator,
	} {
		if got := typ.Kind(); got != want {
			t.Errorf("kind of %q = %q, want %q", typ, got, want)
		}
	}
	if err := CPUType.Validate(); err != nil {
		t.Errorf("the CPU type is no valid type name: %v", err)
	}
}
