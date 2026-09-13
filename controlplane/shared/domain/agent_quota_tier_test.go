package domain

import "testing"

func TestApplyQuotaTierPassesGuaranteedThroughUnchanged(t *testing.T) {
	g, b := ApplyQuotaTier(QuotaTierGuaranteed, 10, 4)
	if g != 10 || b != 4 {
		t.Errorf("got (%v, %v), want (10, 4)", g, b)
	}
}

func TestApplyQuotaTierFoldsGuaranteedIntoBurstOnly(t *testing.T) {
	g, b := ApplyQuotaTier(QuotaTierBurstOnly, 10, 4)
	if g != 0 {
		t.Errorf("guaranteed = %v, want 0", g)
	}
	if b != 14 {
		t.Errorf("burst = %v, want 14 (10 guaranteed + 4 burst preserved)", b)
	}
}

func TestApplyQuotaTierPreservesTotalEntitlement(t *testing.T) {
	for _, tier := range []QuotaTier{QuotaTierGuaranteed, QuotaTierBurstOnly} {
		g, b := ApplyQuotaTier(tier, 10, 4)
		if g+b != 14 {
			t.Errorf("tier=%q: total = %v, want 14 (nothing minted or dropped)", tier, g+b)
		}
	}
}

func TestResolveQuotaTierDefaultsToGuaranteedWithNoOverrideOrPolicy(t *testing.T) {
	if got := ResolveQuotaTier("", ""); got != QuotaTierGuaranteed {
		t.Errorf("no override, no experiment policy: got %q, want guaranteed", got)
	}
}

func TestResolveQuotaTierOverrideWinsOverExperimentPolicy(t *testing.T) {
	if got := ResolveQuotaTier(QuotaTierBurstOnly, QuotaTierGuaranteed); got != QuotaTierBurstOnly {
		t.Errorf("explicit burst_only override under a guaranteed policy: got %q, want burst_only", got)
	}
	if got := ResolveQuotaTier(QuotaTierGuaranteed, QuotaTierBurstOnly); got != QuotaTierGuaranteed {
		t.Errorf("explicit guaranteed override under a burst_only policy: got %q, want guaranteed", got)
	}
}

func TestResolveQuotaTierExperimentPolicyWinsWithNoOverride(t *testing.T) {
	if got := ResolveQuotaTier("", QuotaTierGuaranteed); got != QuotaTierGuaranteed {
		t.Errorf("no override, guaranteed policy: got %q, want guaranteed", got)
	}
	if got := ResolveQuotaTier("", QuotaTierBurstOnly); got != QuotaTierBurstOnly {
		t.Errorf("no override, burst_only policy: got %q, want burst_only", got)
	}
}

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
