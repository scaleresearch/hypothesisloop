package domain

import (
	"math"
	"testing"
)

func TestModelSpendIsEveryKindOfTokenAtItsModelsPrice(t *testing.T) {
	catalog := ModelCatalog{
		"claude-opus-5-5": {InputUSDPerMTok: 4, OutputUSDPerMTok: 20, CacheWriteUSDPerMTok: 8, CacheReadUSDPerMTok: 0.2},
		"claude-sonnet-5": {InputUSDPerMTok: 2, OutputUSDPerMTok: 10, CacheWriteUSDPerMTok: 4, CacheReadUSDPerMTok: 0.2},
	}
	usd, err := catalog.SpendUSD([]ModelTokens{
		{Model: "claude-opus-5-5", Input: 1_000_000, Output: 100_000, CacheWrite: 500_000, CacheRead: 10_000_000},
		{Model: "claude-sonnet-5", Input: 60_143, Output: 167_819, CacheWrite: 674_374, CacheRead: 13_381_880},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := (4 + 2 + 4 + 2) + (0.120286 + 1.67819 + 2.697496 + 2.676376)
	if math.Abs(usd-want) > 1e-6 {
		t.Fatalf("spend = %f, want %f", usd, want)
	}
	if _, err := catalog.SpendUSD([]ModelTokens{{Model: "claude-fable-5-1", Output: 1}}); err == nil {
		t.Fatal("usage of a model the catalog does not price was priced")
	}
}

func TestAModelBudgetIsSpentOnceItsCallsCostIt(t *testing.T) {
	stated := 20.0
	for _, tc := range []struct {
		stated *float64
		used   float64
		spent  bool
	}{
		{nil, 49.99, false}, {nil, 50, true}, {&stated, 19.99, false}, {&stated, 20, true},
	} {
		b := ModelBudget{BudgetUSD: ModelBudgetUSD(tc.stated, 50), UsedUSD: tc.used}
		if b.Spent() != tc.spent {
			t.Errorf("%+v spent = %v, want %v", b, b.Spent(), tc.spent)
		}
	}
}
