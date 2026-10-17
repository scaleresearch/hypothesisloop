package experimentspec

import "testing"

func TestValidQuotaTierOverride(t *testing.T) {
	for _, s := range []string{"", "guaranteed", "burst_only"} {
		if !ValidQuotaTierOverride(s) {
			t.Errorf("ValidQuotaTierOverride(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"human", "agent", "GUARANTEED", "typo"} {
		if ValidQuotaTierOverride(s) {
			t.Errorf("ValidQuotaTierOverride(%q) = true, want false", s)
		}
	}
}
