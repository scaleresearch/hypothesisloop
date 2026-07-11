package workload

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseCPUCores parses a JobSpec.CPU-style quantity string (e.g. "4", "500m") into fractional
// CPU cores. Empty string returns 0, nil — callers treat 0 as "not specified" (cluster default
// applies at Job-build time; not counted against any quota since the actual value isn't known
// at admission time).
func ParseCPUCores(qty string) (float64, error) {
	if qty == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	return float64(q.MilliValue()) / 1000.0, nil
}

// ParseMemoryGB parses a JobSpec.Memory-style quantity string (e.g. "16Gi", "512Mi") into
// fractional gigabytes (decimal GB = 1e9 bytes, matching how "GB-hour" is billed). Empty
// string returns 0, nil.
func ParseMemoryGB(qty string) (float64, error) {
	return parseBytesGB(qty)
}

// ParseStorageGB parses a JobSpec.Storage-style quantity string into fractional gigabytes,
// same convention as ParseMemoryGB.
func ParseStorageGB(qty string) (float64, error) {
	return parseBytesGB(qty)
}

func parseBytesGB(qty string) (float64, error) {
	if qty == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	return q.AsApproximateFloat64() / 1e9, nil
}
