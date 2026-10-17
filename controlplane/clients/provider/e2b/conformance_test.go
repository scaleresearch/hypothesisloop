package e2b

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/providertest"
)

// The conformance tests rent real sandboxes and cost real money, a few cents a run. They run
// when SANDBOX_E2B_API_KEY is set, and first wait for the template of the image below, building
// it on the first run:
//
//	SANDBOX_E2B_API_KEY=... go test ./controlplane/clients/provider/e2b -run Conformance -v -timeout 45m
//
// E2B_CONFORMANCE_HOLD=1 adds the thirty minute hold. One run per team at a time: a run starts by
// destroying what an aborted earlier run left under the suite's prefix.
func conformanceOptions() providertest.Options {
	return providertest.Options{
		Template: provider.Spec{
			Image:        "alpine/curl:8.14.1",
			Architecture: "amd64",
			CPUMillis:    1000,
			MemoryBytes:  1 << 30,
			DiskBytes:    8 << 30,
		},
		OutboundURL:   "https://example.com/",
		StartTimeout:  5 * time.Minute,
		SettleTimeout: 5 * time.Minute,
	}
}

func conformanceProvider(t *testing.T) *Provider {
	t.Helper()
	if os.Getenv("SANDBOX_E2B_API_KEY") == "" {
		t.Skip("SANDBOX_E2B_API_KEY is not set")
	}
	// The hold must fit into one lifetime, and one hour is the most the free plan gives.
	p, err := New(Settings{LifetimeSeconds: 3600}, os.Getenv("SANDBOX_E2B_API_KEY"), nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for {
		_, err := p.readyTemplate(ctx, conformanceOptions().Template)
		if !errors.Is(err, errBuilding) {
			if err != nil {
				t.Fatalf("the conformance template: %v", err)
			}
			return p
		}
		t.Log(err)
		select {
		case <-ctx.Done():
			t.Fatalf("the conformance template was not built in time: %v", err)
		case <-time.After(10 * time.Second):
		}
	}
}

func TestConformance(t *testing.T) {
	providertest.Run(t, conformanceProvider(t), conformanceOptions())
}

func TestConformancePause(t *testing.T) {
	providertest.RunPause(t, conformanceProvider(t), conformanceOptions(), providertest.SameProcess)
}

func TestConformanceHold(t *testing.T) {
	if os.Getenv("E2B_CONFORMANCE_HOLD") == "" {
		t.Skip("E2B_CONFORMANCE_HOLD is not set")
	}
	providertest.RunHold(t, conformanceProvider(t), conformanceOptions(), 30*time.Minute)
}
