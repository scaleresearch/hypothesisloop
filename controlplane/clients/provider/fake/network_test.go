package fake

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

func spec(name string, cpu int64) provider.Spec {
	return provider.Spec{Name: name, Architecture: "amd64", CPUMillis: cpu, MemoryBytes: 1, DiskBytes: 1}
}

func TestANetworkHoldsWhatItSaysAndItsMembersHaveAddresses(t *testing.T) {
	ctx := context.Background()
	p := New()
	p.Network = func(s provider.Spec, spread bool) provider.Supply {
		if s.CPUMillis == 2000 || !spread {
			return provider.Supply{Count: 1}
		}
		return provider.Supply{Count: 2}
	}
	if supply, err := p.Networked(ctx, spec("x", 1000), 5, true); err != nil || supply.Count != 2 {
		t.Fatalf("Networked = %+v, %v, want 2 of the 5 asked about", supply, err)
	}
	for _, specs := range [][]provider.Spec{
		{spec("a", 1000), spec("b", 1000), spec("c", 1000)},
		{spec("a", 2000), spec("b", 2000)},
	} {
		if err := p.CreateNetwork(ctx, "hl-net-a", specs, true); !errors.Is(err, provider.ErrNoOffer) {
			t.Fatalf("a network for more than one holds: %v, want ErrNoOffer", err)
		}
	}
	if err := p.CreateNetwork(ctx, "hl-net-a", []provider.Spec{spec("a", 1000), spec("b", 1000)}, false); !errors.Is(err, provider.ErrNoOffer) {
		t.Fatalf("unspread the network holds one: %v, want ErrNoOffer", err)
	}
	gang := []provider.Spec{spec("a", 1000), spec("b", 1000), spec("c", 2000)}
	for range 2 {
		if err := p.CreateNetwork(ctx, "hl-net-a", gang, true); err != nil {
			t.Fatalf("CreateNetwork: %v", err)
		}
	}
	if names, err := p.Networks(ctx, "hl-net-"); err != nil || !slices.Equal(names, []string{"hl-net-a"}) {
		t.Fatalf("Networks = %v, %v", names, err)
	}

	stray := spec("stray", 1000)
	stray.Network = "hl-net-b"
	if _, err := p.Create(ctx, stray); err == nil {
		t.Fatal("an instance joined a network that does not exist")
	}
	member := spec("member", 1000)
	member.Network = "hl-net-a"
	id, err := p.Create(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if i, _, _ := p.Get(ctx, id); i.PrivateAddress == "" {
		t.Fatalf("a member of a network has no address: %+v", i)
	}
	if err := p.DeleteNetwork(ctx, "hl-net-a"); err == nil {
		t.Fatal("a network was deleted with an instance on it")
	}
	if err := p.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.DeleteNetwork(ctx, "hl-net-a"); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
	}
	if names, _ := p.Networks(ctx, ""); len(names) != 0 {
		t.Fatalf("Networks after delete = %v", names)
	}
}
