package workload

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ParseCPUCores parses a JobSpec.CPU-style quantity string (e.g. "4", "500m") into fractional
// CPU cores. Empty string, and domain.MaxResourceSentinel ("max", not yet resolved to a literal),
// both return 0, nil — callers treat 0 as "not specified/not yet known" (cluster default applies
// at Job-build time; not counted against any quota since the actual value isn't known yet). A
// "max" field is not resolved until this job is admitted onto a specific cluster (see
// scheduler.Loop.resolveClusterLocalResources) — so this returns 0 for it wherever it is called
// before that point, e.g. this same submission's own structural validation.
func ParseCPUCores(qty string) (float64, error) {
	if qty == "" || qty == domain.MaxResourceSentinel {
		return 0, nil
	}
	q, err := parseQuantity(qty)
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
	if qty == "" || qty == domain.MaxResourceSentinel {
		return 0, nil
	}
	q, err := parseQuantity(qty)
	if err != nil {
		return 0, err
	}
	return q.AsApproximateFloat64() / 1e9, nil
}

// parseQuantity is resource.ParseQuantity with its one hole closed: it accepts a bare sign, so
// "-" parses cleanly as 0. Zero is already the meaningful answer for "not specified", so a
// truncated or corrupted field would be admitted as a job requesting nothing — costed at nothing
// and checked against no quota. Admission's negative-resource rule does not cover it either,
// because 0 is not negative.
func parseQuantity(qty string) (resource.Quantity, error) {
	if qty == "-" || qty == "+" {
		return resource.Quantity{}, fmt.Errorf("quantity %q has a sign but no number", qty)
	}
	return resource.ParseQuantity(qty)
}
