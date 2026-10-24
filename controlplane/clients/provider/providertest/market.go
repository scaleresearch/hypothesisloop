package providertest

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// marketAsked is how many instances RunMarket asks the market about.
const marketAsked = 3

// RunMarket is the check of the Market capability, for an adapter that implements it: the count
// for the template is within what was asked about, a size no machine has counts nothing, and a
// count above nothing is one Create can rent from. It rents one instance at most.
func RunMarket(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	market, ok := p.(provider.Market)
	if !ok {
		t.Fatalf("%T does not implement provider.Market", p)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	r := newRun(t, ctx, paced, opts)

	count := rentable(t, ctx, market, opts.Template)
	if count < 0 || count > marketAsked {
		t.Fatalf("Rentable = %d, asked about %d", count, marketAsked)
	}
	if none := rentable(t, ctx, market, noMachine(opts.Template)); none != 0 {
		t.Fatalf("Rentable = %d for a size no machine has", none)
	}
	if count == 0 {
		t.Skip("the vendor has nothing on offer for the template right now, so a rent from the count was not checked")
	}
	r.create(t, ctx, r.spec("market", "sleep 600"))
}

func rentable(t *testing.T, ctx context.Context, market provider.Market, spec provider.Spec) int {
	t.Helper()
	supply, err := market.Rentable(ctx, spec, marketAsked)
	if err != nil {
		t.Fatalf("Rentable: %v", err)
	}
	if (supply.Count == 0) != (supply.Why != "") {
		t.Fatalf("Rentable = %+v: a count of nothing says why, and only such a count", supply)
	}
	return supply.Count
}

// noMachine is template at a size no machine has: more accelerators than any has, or for a
// template without accelerators more cores.
func noMachine(template provider.Spec) provider.Spec {
	if template.AcceleratorCount > 0 {
		template.AcceleratorCount = 1 << 20
	} else {
		template.CPUMillis = 1 << 40
	}
	return template
}
