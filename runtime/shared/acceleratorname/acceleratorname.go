// Package acceleratorname turns a vendor's name for a device into the resource catalog's string
// for it, so every runtime that discovers hardware reports one type under one name.
package acceleratorname

import (
	"fmt"
	"strings"
)

const nvidiaProductKey = "nvidia.com/gpu.product"

// NVIDIAProductLabel is the catalog string of an NVIDIA device from the product name its driver
// or a vendor's listing reports.
func NVIDIAProductLabel(name string) string {
	return nvidiaProductKey + "=" + GFDProduct(name)
}

// GFDProduct reproduces NVIDIA GPU Feature Discovery's node-label normalization exactly
// (different-backend.md §4.2): uppercase, spaces to dashes. "NVIDIA H100 80GB HBM3" ->
// "NVIDIA-H100-80GB-HBM3".
func GFDProduct(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.Join(strings.Fields(upper), "-")
}

// Validate checks a catalog string an operator wrote by hand: a key=value pair, and for an NVIDIA
// product the form NVIDIAProductLabel produces. The control plane compares the type a running
// job reports with the one it requested character for character, so a type configured in another
// casing would match jobs at admission and then fail every one of them once it ran.
func Validate(catalogType string) error {
	key, value, ok := strings.Cut(catalogType, "=")
	if !ok || key == "" || value == "" {
		return fmt.Errorf("accelerator type %q is not a key=value catalog string", catalogType)
	}
	if key == nvidiaProductKey && value != GFDProduct(value) {
		return fmt.Errorf("accelerator type %q must be written %q", catalogType, NVIDIAProductLabel(value))
	}
	return nil
}
