package domain

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseCPUCores parses a JobSpec.CPU-style quantity string (e.g. "4", "500m") into fractional
// CPU cores. Empty string returns 0, nil.
func ParseCPUCores(qty string) (float64, error) {
	if qty == "" {
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

// CheckNodeRequest refuses what one node of a job asks for when a field of it is no quantity or
// is negative, naming the field. It is the one rule for these three, at a job's submit and in a
// proposal that states them for the jobs to come.
//
// A negative quantity parses cleanly, resource.ParseQuantity accepts "-100Gi", and every cap is
// an upper bound, so nothing else stops one. Downstream the two views of a job disagree about
// it: Footprint carries the negative through, so reserving the job credits the cluster capacity
// it never had and bills the agent for less than it used, while NodeShapes drops the dimension,
// so placement never sees it.
func CheckNodeRequest(cpu, memory, storage string) error {
	for _, dimension := range []struct {
		field, qty string
		parse      func(string) (float64, error)
	}{
		{"cpu", cpu, ParseCPUCores},
		{"memory", memory, ParseMemoryGB},
		{"storage", storage, ParseStorageGB},
	} {
		value, err := dimension.parse(dimension.qty)
		if err != nil {
			return fmt.Errorf("%s: %w", dimension.field, err)
		}
		if value < 0 {
			return fmt.Errorf("%s must not be negative (%q)", dimension.field, dimension.qty)
		}
	}
	return nil
}

func parseBytesGB(qty string) (float64, error) {
	if qty == "" {
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
