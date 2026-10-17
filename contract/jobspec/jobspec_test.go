package jobspec

import (
	"strings"
	"testing"
)

// Milli-suffixed CPU is the form Kubernetes-shaped specs are usually written in, and it is the one
// a naive numeric parse gets wrong by a factor of a thousand, in the direction that admits a job
// asking for 500 cores as if it wanted half of one.
func TestCPUCoresHandlesMillicores(t *testing.T) {
	for qty, want := range map[string]float64{"4": 4, "500m": 0.5, "1500m": 1.5, "0": 0} {
		got, err := CPUCores(qty)
		if err != nil {
			t.Fatalf("CPUCores(%q) = %v", qty, err)
		}
		if got != want {
			t.Errorf("CPUCores(%q) = %v, want %v", qty, got, want)
		}
	}
}

// Bytes are billed in decimal GB (1e9), while the quantities are written in binary units (Gi =
// 2^30). Conflating the two silently misbills every job by ~7%.
func TestBytesParseIntoDecimalGigabytes(t *testing.T) {
	got, err := GB("1Gi")
	if err != nil {
		t.Fatalf("GB: %v", err)
	}
	if want := float64(1<<30) / 1e9; got != want {
		t.Errorf("GB(\"1Gi\") = %v, want %v", got, want)
	}
}

// A quantity that cannot be read is an error, never a zero: folding a typo into zero would admit
// a job requesting garbage as one requesting nothing. A negative is refused too, as every cap is
// an upper bound.
func TestAnUnparseableOrNegativeQuantityIsAnError(t *testing.T) {
	for _, qty := range []string{"", "max", "lots", "4x", "-", "+", "1 Gi", "-100Gi", "-1"} {
		if _, err := ParseQuantity(qty); err == nil {
			t.Errorf("ParseQuantity(%q) = nil error", qty)
		}
	}
}

func TestANodeStatesEachQuantity(t *testing.T) {
	if err := CheckNode("job", "2", "8Gi", "4Gi"); err != nil {
		t.Fatalf("CheckNode refused a whole node: %v", err)
	}
	for _, c := range []struct {
		cpu, memory, storage, field string
		unstated                    bool
	}{
		{"", "8Gi", "4Gi", "job.cpu", true},
		{"2", "max", "4Gi", "job.memory", true},
		{"2", "8Gi", "-4Gi", "job.storage", false},
		{"lots", "8Gi", "4Gi", "job.cpu", false},
	} {
		err := CheckNode("job", c.cpu, c.memory, c.storage)
		if err == nil || err.Field != c.field || err.Unstated != c.unstated {
			t.Errorf("CheckNode(%q, %q, %q) = %+v, want field %s unstated %v", c.cpu, c.memory, c.storage, err, c.field, c.unstated)
		}
	}
}

// Each quantity is named by the node that states it, a group by its own field, and the first
// one wrong is the one answered. A field that names nothing reads as its quantity alone.
func TestANodeErrorNamesTheFieldOfTheNodeThatStatesIt(t *testing.T) {
	for _, c := range []struct {
		owner, cpu, memory, storage string
		field, quantity, says       string
		unstated                    bool
	}{
		{`job.groups["b"]`, "2", "-100Gi", "4Gi", `job.groups["b"].memory`, "memory", "negative", false},
		{`job.groups["a"]`, "max", "8Gi", "4Gi", `job.groups["a"].cpu`, "cpu", "never as a share of the node", true},
		{"job", "2", "8Gi", "", "job.storage", "storage", "storage: 4Gi", true},
		{"job", "2", "8Gi", "max", "job.storage", "storage", "never as a share of the node", true},
		{"", "2", "eight", "4Gi", "memory", "memory", "memory", false},
		{"job", "", "", "", "job.cpu", "cpu", "cpu: 2", true},
		{"job", "1 Gi", "8Gi", "-", "job.cpu", "cpu", "job.cpu", false},
	} {
		err := CheckNode(c.owner, c.cpu, c.memory, c.storage)
		if err == nil || err.Field != c.field || err.Quantity != c.quantity || err.Unstated != c.unstated || !strings.Contains(err.Message, c.says) || err.Error() != err.Message {
			t.Errorf("CheckNode(%q, %q, %q, %q) = %+v, want %s (%s) unstated %v saying %q", c.owner, c.cpu, c.memory, c.storage, err, c.field, c.quantity, c.unstated, c.says)
		}
	}
	for _, node := range [][3]string{{"500m", "512Mi", "1G"}, {"0.5", "1Gi", "0"}, {"64", "2Ti", "100Gi"}} {
		if err := CheckNode(`job.groups["a"]`, node[0], node[1], node[2]); err != nil {
			t.Errorf("CheckNode(%v) = %v, want a node that states each quantity taken", node, err)
		}
	}
}
