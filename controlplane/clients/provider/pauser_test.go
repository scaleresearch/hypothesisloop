package provider_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

// core is a provider with the required calls and nothing more, and each of the others has one
// capability beside them.
type (
	core   struct{ provider.Provider }
	pauses struct {
		core
		provider.Pauser
	}
	counts struct {
		core
		provider.Market
	}
	warms struct {
		core
		provider.Warmer
	}
	networks struct {
		core
		provider.Networker
	}
)

func TestACapabilityIsFoundOnTheProviderAndSurvivesTheBoundingWrapper(t *testing.T) {
	pausing := fake.New()
	c := core{pausing}
	for _, tc := range []struct {
		p    provider.Provider
		want []string
	}{
		{c, []string{}},
		{pauses{c, pausing}, []string{provider.CapabilityPause}},
		{counts{c, pausing}, []string{provider.CapabilityMarket}},
		{warms{c, pausing}, []string{provider.CapabilityWarm}},
		{networks{c, pausing}, []string{provider.CapabilityNetwork}},
		{pausing, []string{provider.CapabilityMarket, provider.CapabilityNetwork, provider.CapabilityPause, provider.CapabilityWarm}},
	} {
		got := provider.Capabilities(tc.p)
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Capabilities of %T = %v, want %v", tc.p, got, tc.want)
		}
		b := provider.Bound(tc.p)
		carried := []string{}
		for name, has := range map[string]bool{
			provider.CapabilityPause: b.Pauser != nil, provider.CapabilityMarket: b.Market != nil,
			provider.CapabilityWarm: b.Warmer != nil, provider.CapabilityNetwork: b.Networker != nil,
		} {
			if has {
				carried = append(carried, name)
			}
		}
		sort.Strings(carried)
		if !reflect.DeepEqual(carried, tc.want) {
			t.Fatalf("Bound(%T) carries %v, want %v: the wrapper hid what it wraps or made up what it does not", tc.p, carried, tc.want)
		}
		if got := provider.Capabilities(b); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Capabilities of Bound(%T) = %v, want %v: what the wrapper carries", tc.p, got, tc.want)
		}
	}
	pausing.Market = func(provider.Spec) provider.Supply { return provider.Supply{Count: 3} }
	if supply, err := provider.Bound(pausing).Market.Rentable(context.Background(), provider.Spec{}, 5); err != nil || supply.Count != 3 {
		t.Fatalf("Rentable through the wrapper = %+v, %v", supply, err)
	}

	ctx := context.Background()
	id, err := pausing.Create(ctx, provider.Spec{Name: "hl-a", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	pausing.Start("hl-a")
	bounded := provider.Bound(pausing).Pauser
	if err := bounded.Pause(ctx, id); err != nil {
		t.Fatal(err)
	}
	listed, _ := pausing.List(ctx, "hl-")
	if listed[0].State != provider.StatePaused {
		t.Fatalf("after Pause the instance is %q", listed[0].State)
	}
	if got := provider.PausedAsStarting(listed); got[0].State != provider.StateStarting || got[0].ID != id {
		t.Fatalf("to a caller that never pauses the instance reads %+v", got[0])
	}
	if err := bounded.Pause(ctx, id); err != nil {
		t.Fatalf("Pause of a paused instance: %v", err)
	}
	if err := bounded.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := bounded.Resume(ctx, id); err != nil {
		t.Fatalf("Resume of a running instance: %v", err)
	}
	if listed, _ := pausing.List(ctx, "hl-"); listed[0].State != provider.StateRunning {
		t.Fatalf("after Resume the instance is %q", listed[0].State)
	}
	if err := pausing.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := bounded.Pause(ctx, id); err != nil {
		t.Fatalf("Pause of an instance that is gone: %v", err)
	}
	if err := bounded.Resume(ctx, id); err == nil {
		t.Fatal("Resume of an instance that is gone returned no error")
	}
}
