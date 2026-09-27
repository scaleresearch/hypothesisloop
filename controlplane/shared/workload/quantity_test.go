package workload

import (
	"testing"
)

// A quantity left out parses as 0. Admission refuses a job that leaves one out before it parses
// any, and "max", the retired share of a node, is no quantity.
func TestAQuantityLeftOutParsesAsZeroAndMaxIsNoQuantity(t *testing.T) {
	if _, err := ParseCPUCores("max"); err == nil {
		t.Error(`ParseCPUCores("max") = nil error`)
	}
	if _, err := ParseMemoryGB("max"); err == nil {
		t.Error(`ParseMemoryGB("max") = nil error`)
	}
	if cpu, err := ParseCPUCores(""); err != nil || cpu != 0 {
		t.Errorf(`ParseCPUCores("") = %v, %v; want 0, nil`, cpu, err)
	}
	if mem, err := ParseMemoryGB(""); err != nil || mem != 0 {
		t.Errorf(`ParseMemoryGB("") = %v, %v; want 0, nil`, mem, err)
	}
	if storage, err := ParseStorageGB(""); err != nil || storage != 0 {
		t.Errorf(`ParseStorageGB("") = %v, %v; want 0, nil`, storage, err)
	}
}

// Milli-suffixed CPU is the form Kubernetes-shaped specs are usually written in, and it is the one
// a naive numeric parse gets wrong by a factor of a thousand — in the direction that admits a job
// asking for 500 cores as if it wanted half of one.
func TestParseCPUCoresHandlesMillicores(t *testing.T) {
	for qty, want := range map[string]float64{"4": 4, "500m": 0.5, "1500m": 1.5, "0": 0} {
		got, err := ParseCPUCores(qty)
		if err != nil {
			t.Fatalf("ParseCPUCores(%q) = %v", qty, err)
		}
		if got != want {
			t.Errorf("ParseCPUCores(%q) = %v, want %v", qty, got, want)
		}
	}
}

// Bytes are billed in decimal GB (1e9), while the quantities are written in binary units (Gi =
// 2^30). Conflating the two silently misbills every job by ~7%, in the platform's favour and
// without anything looking wrong.
func TestBytesParseIntoDecimalGigabytesNotBinaryOnes(t *testing.T) {
	got, err := ParseMemoryGB("1Gi")
	if err != nil {
		t.Fatalf("ParseMemoryGB: %v", err)
	}
	if want := float64(1<<30) / 1e9; got != want {
		t.Errorf("ParseMemoryGB(\"1Gi\") = %v, want %v — GB-hour billing is decimal, the quantity is binary", got, want)
	}
	// Storage bills on the same convention; the two silently diverging would put the same number
	// of bytes on an invoice twice at two different prices.
	storage, err := ParseStorageGB("1Gi")
	if err != nil {
		t.Fatalf("ParseStorageGB: %v", err)
	}
	if storage != got {
		t.Errorf("ParseStorageGB(\"1Gi\") = %v but ParseMemoryGB gives %v — both bill in GB-hours and must agree", storage, got)
	}
}

// A quantity that cannot be read is an error, never a zero. Zero is already the meaningful answer
// for "not specified", so folding a typo into it would admit a job requesting garbage as one
// requesting nothing — costed at nothing and checked against no quota.
func TestAnUnparseableQuantityIsAnErrorNotAZero(t *testing.T) {
	for _, qty := range []string{"lots", "4x", "-", "1 Gi"} {
		if _, err := ParseCPUCores(qty); err == nil {
			t.Errorf("ParseCPUCores(%q) = nil error, want a failure rather than a silent zero", qty)
		}
		if _, err := ParseMemoryGB(qty); err == nil {
			t.Errorf("ParseMemoryGB(%q) = nil error, want a failure rather than a silent zero", qty)
		}
	}
}
