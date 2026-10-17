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
	if b != 10 {
		t.Errorf("burst = %v, want 10: the share, with no burst hours on top of it", b)
	}
}

// Whatever the tier, an agent's share of the budget is what was carved for it, and shares of
// 0.4 of a 1.5 hour budget between two agents are 0.3 each with no noise from the division.
func TestAShareIsTheSameUnderEitherTier(t *testing.T) {
	guaranteed, burst := AllocateQuota(1.5*0.4, 2, 0, 0, QuotaConfig{BurstFraction: 2})
	if guaranteed != 0.3 || burst != 0.6 {
		t.Fatalf("allocated (%v, %v), want (0.3, 0.6)", guaranteed, burst)
	}
	for _, tier := range []QuotaTier{QuotaTierGuaranteed, QuotaTierBurstOnly} {
		g, b := ApplyQuotaTier(tier, guaranteed, burst)
		if share := (&AgentQuota{GuaranteedAcceleratorHours: g, BurstAcceleratorHours: b}).Share(); share != 0.3 {
			t.Errorf("tier=%q: share = %v, want 0.3", tier, share)
		}
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
