package provider_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

// core is a provider with the required calls and nothing more.
type core struct{ provider.Provider }

// some is a provider that has the capabilities of its fields that are set.
func some(p *fake.Provider, pauses, counts, warms bool) provider.Provider {
	type (
		P = provider.Pauser
		M = provider.Market
		W = provider.Warmer
	)
	c := core{p}
	switch {
	case pauses && counts && warms:
		return p
	case pauses && counts:
		return struct {
			core
			P
			M
		}{c, p, p}
	case pauses && warms:
		return struct {
			core
			P
			W
		}{c, p, p}
	case counts && warms:
		return struct {
			core
			M
			W
		}{c, p, p}
	case pauses:
		return struct {
			core
			P
		}{c, p}
	case counts:
		return struct {
			core
			M
		}{c, p}
	case warms:
		return struct {
			core
			W
		}{c, p}
	}
	return c
}

func TestACapabilityIsFoundOnTheProviderAndSurvivesTheBoundingWrapper(t *testing.T) {
	pausing := fake.New()
	for _, pauses := range []bool{false, true} {
		for _, counts := range []bool{false, true} {
			for _, warms := range []bool{false, true} {
				want := []string{}
				for name, has := range map[string]bool{provider.CapabilityPause: pauses, provider.CapabilityMarket: counts, provider.CapabilityWarm: warms} {
					if has {
						want = append(want, name)
					}
				}
				sort.Strings(want)
				one := some(pausing, pauses, counts, warms)
				for _, p := range []provider.Provider{one, provider.Bounded(one)} {
					got := provider.Capabilities(p)
					sort.Strings(got)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("Capabilities of %T = %v, want %v: the wrapper hid what it wraps or made up what it does not", p, got, want)
					}
				}
			}
		}
	}
	pausing.Market = func(provider.Spec) provider.Supply { return provider.Supply{Count: 3} }
	if supply, err := provider.Bounded(pausing).(provider.Market).Rentable(context.Background(), provider.Spec{}, 5); err != nil || supply.Count != 3 {
		t.Fatalf("Rentable through the wrapper = %+v, %v", supply, err)
	}

	ctx := context.Background()
	id, err := pausing.Create(ctx, provider.Spec{Name: "hl-a"})
	if err != nil {
		t.Fatal(err)
	}
	pausing.Start("hl-a")
	bounded := provider.Bounded(pausing).(provider.Pauser)
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
