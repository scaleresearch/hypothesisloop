package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

func spec(name string, cpu int64) provider.Spec {
	return provider.Spec{Name: name, Architecture: "amd64", CPUMillis: cpu, MemoryBytes: 1, DiskBytes: 1}
}

func TestAPlaceHoldsWhatItSaysAndItsMembersHaveAddresses(t *testing.T) {
	ctx := context.Background()
	p := New()
	p.Network = func(s provider.Spec) provider.Supply {
		if s.CPUMillis == 2000 {
			return provider.Supply{Count: 1}
		}
		return provider.Supply{Count: 2}
	}
	if supply, err := p.Networked(ctx, spec("x", 1000), 5); err != nil || supply.Count != 2 {
		t.Fatalf("Networked = %+v, %v, want 2 of the 5 asked about", supply, err)
	}
	for _, specs := range [][]provider.Spec{
		{spec("a", 1000), spec("b", 1000), spec("c", 1000)},
		{spec("a", 2000), spec("b", 2000)},
	} {
		if _, err := p.Place(ctx, specs); !errors.Is(err, provider.ErrNoOffer) {
			t.Fatalf("a place for more than one holds: %v, want ErrNoOffer", err)
		}
	}
	place, err := p.Place(ctx, []provider.Spec{spec("a", 1000), spec("b", 1000), spec("c", 2000)})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}

	stray := spec("stray", 1000)
	stray.Place = "elsewhere"
	if _, err := p.Create(ctx, stray); err == nil {
		t.Fatal("an instance was rented in a place the fake does not have")
	}
	member := spec("member", 1000)
	member.Place = place
	id, err := p.Create(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if i, _, _ := p.Get(ctx, id); i.PrivateAddress == "" || i.Host == "" {
		t.Fatalf("a member of a place has no address or host: %+v", i)
	}
}
