package provider_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

// core is a provider with the required calls and nothing more.
type core struct{ provider.Provider }

type pausesOnly struct {
	provider.Provider
	provider.Pauser
}

type countsOnly struct {
	provider.Provider
	provider.Market
}

func TestACapabilityIsFoundOnTheProviderAndSurvivesTheBoundingWrapper(t *testing.T) {
	pausing := fake.New()
	both := []string{provider.CapabilityPause, provider.CapabilityMarket}
	if got := provider.Capabilities(pausing); !reflect.DeepEqual(got, both) {
		t.Fatalf("Capabilities = %v", got)
	}
	if got := provider.Capabilities(provider.Bounded(pausing)); !reflect.DeepEqual(got, both) {
		t.Fatalf("Capabilities of the bounded provider = %v: the wrapper hid what it wraps", got)
	}
	for _, plain := range []provider.Provider{core{pausing}, provider.Bounded(core{pausing})} {
		if got := provider.Capabilities(plain); len(got) != 0 {
			t.Fatalf("Capabilities of a provider that cannot pause = %v", got)
		}
	}
	for name, one := range map[string]provider.Provider{provider.CapabilityPause: pausesOnly{pausing, pausing}, provider.CapabilityMarket: countsOnly{pausing, pausing}} {
		if got := provider.Capabilities(provider.Bounded(one)); !reflect.DeepEqual(got, []string{name}) {
			t.Fatalf("Capabilities of a bounded provider that has only %s = %v", name, got)
		}
	}
	pausing.Market = func(provider.Spec) int { return 3 }
	if count, err := provider.Bounded(pausing).(provider.Market).Rentable(context.Background(), provider.Spec{}, 5); err != nil || count != 3 {
		t.Fatalf("Rentable through the wrapper = %d, %v", count, err)
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
