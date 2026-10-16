package e2b

import (
	"os"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider/providertest"
)

// The conformance tests rent real sandboxes and cost real money, a few cents a run. They run
// when E2B_API_KEY is set, and need the template of the image below built first:
//
//	E2B_API_KEY=... go run ./controlplane/cmd/e2b-template alpine/curl:8.14.1 1 1024
//	E2B_API_KEY=... go test ./controlplane/shared/provider/e2b -run Conformance -v -timeout 45m
//
// E2B_CONFORMANCE_HOLD=1 adds the thirty minute hold. One run per team at a time: a run starts by
// destroying what an aborted earlier run left under the suite's prefix.
func conformanceOptions() providertest.Options {
	return providertest.Options{
		Template: provider.Spec{
			Image:       "alpine/curl:8.14.1",
			CPUMillis:   1000,
			MemoryBytes: 1 << 30,
			DiskBytes:   8 << 30,
		},
		OutboundURL:   "https://example.com/",
		StartTimeout:  5 * time.Minute,
		SettleTimeout: 5 * time.Minute,
	}
}

func conformanceProvider(t *testing.T) *Provider {
	t.Helper()
	if os.Getenv("E2B_API_KEY") == "" {
		t.Skip("E2B_API_KEY is not set")
	}
	// The hold must fit into one lifetime, and one hour is the most the free plan gives.
	p, err := New(Settings{APIKeyEnv: "E2B_API_KEY", LifetimeSeconds: 3600}, nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
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
