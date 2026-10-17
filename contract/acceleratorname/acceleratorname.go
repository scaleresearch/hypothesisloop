// Package acceleratorname turns a vendor's name for a device into the resource catalog's string
// for it, so every runtime that discovers hardware reports one type under one name.
package acceleratorname

import (
	"fmt"
	"regexp"
	"strings"
)

const nvidiaProductKey = "nvidia.com/gpu.product"

// NVIDIAProductLabel is the catalog string of an NVIDIA device from the product name its driver
// or a vendor's listing reports.
func NVIDIAProductLabel(name string) string {
	return nvidiaProductKey + "=" + GFDProduct(name)
}

// gfdDropped is every character NVIDIA GPU Feature Discovery drops from a product name.
var gfdDropped = regexp.MustCompile(`[^A-Za-z0-9-_. ]`)

// GFDProduct is the nvidia.com/gpu.product value GPU Feature Discovery labels a node with for the
// product name NVML reports (sanitise in k8s-device-plugin's internal/lm/resource.go): characters
// other than ASCII letters, digits, '-', '_', '.' and ' ' dropped, each run of spaces one '-',
// and the case kept. "NVIDIA GeForce RTX 4090" -> "NVIDIA-GeForce-RTX-4090".
func GFDProduct(name string) string {
	return strings.Join(strings.Fields(gfdDropped.ReplaceAllString(name, "")), "-")
}

// Validate checks a catalog string an operator wrote by hand: a key=value pair, and for an NVIDIA
// product a value GPU Feature Discovery could have labelled. Types compare exactly, so the
// catalog must also keep the case GFD keeps (NVIDIA-GeForce-RTX-4090, Tesla-T4).
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
