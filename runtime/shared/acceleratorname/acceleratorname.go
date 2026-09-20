// Package acceleratorname turns a vendor's name for a device into the resource catalog's string
// for it, so every runtime that discovers hardware reports one type under one name.
package acceleratorname

import "strings"

// NVIDIAProductLabel is the catalog string of an NVIDIA device from the product name its driver
// or a vendor's listing reports.
func NVIDIAProductLabel(name string) string {
	return "nvidia.com/gpu.product=" + GFDProduct(name)
}

// GFDProduct reproduces NVIDIA GPU Feature Discovery's node-label normalization exactly
// (different-backend.md §4.2): uppercase, spaces to dashes. "NVIDIA H100 80GB HBM3" ->
// "NVIDIA-H100-80GB-HBM3".
func GFDProduct(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.Join(strings.Fields(upper), "-")
}
