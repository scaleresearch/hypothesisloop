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

func TestResolveQuotaTierDefaultsByKind(t *testing.T) {
	if got := ResolveQuotaTier(AgentKindHuman, ""); got != QuotaTierGuaranteed {
		t.Errorf("human, no override: got %q, want guaranteed", got)
	}
	if got := ResolveQuotaTier(AgentKindAgent, ""); got != QuotaTierBurstOnly {
		t.Errorf("agent, no override: got %q, want burst_only", got)
	}
	if got := ResolveQuotaTier(AgentKind(""), ""); got != QuotaTierBurstOnly {
		t.Errorf("unrecognized kind, no override: got %q, want burst_only", got)
	}
	if got := ResolveQuotaTier(AgentKind("typo"), ""); got != QuotaTierBurstOnly {
		t.Errorf("typo'd kind, no override: got %q, want burst_only", got)
	}
}

func TestResolveQuotaTierOverrideWinsOverKind(t *testing.T) {
	if got := ResolveQuotaTier(AgentKindAgent, QuotaTierGuaranteed); got != QuotaTierGuaranteed {
		t.Errorf("agent explicitly granted guaranteed: got %q, want guaranteed", got)
	}
	if got := ResolveQuotaTier(AgentKindHuman, QuotaTierBurstOnly); got != QuotaTierBurstOnly {
		t.Errorf("human explicitly restricted to burst_only: got %q, want burst_only", got)
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
