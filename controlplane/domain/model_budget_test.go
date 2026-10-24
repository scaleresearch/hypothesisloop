package domain

import (
	"math"
	"testing"
)

func TestACallCostsEveryKindOfTokenAtItsModelsPrice(t *testing.T) {
	p := ModelPrice{InputUSDPerMTok: 4, OutputUSDPerMTok: 20, CacheWriteUSDPerMTok: 8, CacheReadUSDPerMTok: 0.2}
	if got, want := p.CostUSD(ModelTokens{Input: 1_000_000, Output: 100_000, CacheWrite: 500_000, CacheRead: 10_000_000}), 4.0+2+4+2; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %f, want %f", got, want)
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
