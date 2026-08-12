package domain

import "testing"

// The two silence reasons must stay distinct: an operator seeing "silent" looks for a hung
// training process, which is the wrong hunt when the reporting path never worked at all.
func TestSilenceReasonsAreDistinct(t *testing.T) {
	if EvictionSilent == EvictionNeverReportedMetrics {
		t.Fatal("silence reasons collapsed to one value")
	}
	for name, got := range map[string]EvictionReason{
		"silent":                 EvictionSilent,
		"never_reported_metrics": EvictionNeverReportedMetrics,
	} {
		if string(got) != name {
			t.Errorf("%s wire value = %q, want %q", name, got, name)
		}
	}
}
