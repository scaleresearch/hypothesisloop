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

func TestACatalogTypeIsAnAcceleratorUnlessItsEntrySaysCPU(t *testing.T) {
	SetAcceleratorKinds(map[string]TypeKind{
		"cpu.hypothesisloop.dev/class=EPYC-9354": TypeKindCPU,
		"nvidia.com/gpu.product=NVIDIA-L40":      TypeKindAccelerator,
	})
	t.Cleanup(func() { SetAcceleratorKinds(nil) })

	for typ, want := range map[AcceleratorType]TypeKind{
		"cpu.hypothesisloop.dev/class=EPYC-9354": TypeKindCPU,
		// A runtime reports a type in the casing of its driver.
		"cpu.hypothesisloop.dev/class=epyc-9354": TypeKindCPU,
		"nvidia.com/gpu.product=NVIDIA-L40":      TypeKindAccelerator,
		"nvidia.com/gpu.product=not-in-catalog":  TypeKindAccelerator,
		"":                                       TypeKindAccelerator,
	} {
		if got := typ.Kind(); got != want {
			t.Errorf("kind of %q = %q, want %q", typ, got, want)
		}
	}
}

func TestAKindIsOneOfTwoWordsAndNoneMeansAccelerator(t *testing.T) {
	for text, want := range map[string]TypeKind{"": TypeKindAccelerator, "accelerator": TypeKindAccelerator, "cpu": TypeKindCPU} {
		if got, err := ParseTypeKind(text); err != nil || got != want {
			t.Errorf("ParseTypeKind(%q) = %q, %v, want %q", text, got, err, want)
		}
	}
	for _, text := range []string{"CPU", "gpu", "core", " cpu"} {
		if _, err := ParseTypeKind(text); err == nil {
			t.Errorf("ParseTypeKind(%q) accepted", text)
		}
	}
}

func TestRegisteringAKindTheCatalogCannotHoldPanics(t *testing.T) {
	t.Cleanup(func() { SetAcceleratorKinds(nil) })
	for name, kinds := range map[string]map[string]TypeKind{
		"unknown kind": {"a/b=c": "slot"},
		"empty kind":   {"a/b=c": ""},
		"empty name":   {"": TypeKindCPU},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered", name)
				}
			}()
			SetAcceleratorKinds(kinds)
		}()
	}
}
