package fly

import (
	"os"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/providertest"
)

// The conformance tests rent real Machines and cost real money, under a cent a run. They run
// when FLY_CONFORMANCE_ORG_TOKEN holds a token for the organization, in apps of the prefix the
// adapter creates and deletes one for each Machine:
//
//	FLY_CONFORMANCE_ORG_TOKEN=... FLY_CONFORMANCE_ORG=<org slug> FLY_CONFORMANCE_APP_PREFIX=<prefix> FLY_CONFORMANCE_REGION=ams \
//	  go test ./controlplane/clients/provider/fly -run Conformance -v -timeout 45m
//
// FLY_CONFORMANCE_HOLD=1 adds the thirty minute hold. One run per prefix at a time: a run starts
// by destroying what an aborted earlier run left under the suite's prefix.
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
	if os.Getenv("FLY_CONFORMANCE_ORG_TOKEN") == "" {
		t.Skip("FLY_CONFORMANCE_ORG_TOKEN is not set")
	}
	org, prefix, region := os.Getenv("FLY_CONFORMANCE_ORG"), os.Getenv("FLY_CONFORMANCE_APP_PREFIX"), os.Getenv("FLY_CONFORMANCE_REGION")
	if org == "" || prefix == "" || region == "" {
		t.Fatal("FLY_CONFORMANCE_ORG, FLY_CONFORMANCE_APP_PREFIX and FLY_CONFORMANCE_REGION must be set")
	}
	p, err := New(Settings{Org: org, AppPrefix: prefix, Region: region, CPUKind: "shared"}, os.Getenv("FLY_CONFORMANCE_ORG_TOKEN"), nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestConformance(t *testing.T) {
	providertest.Run(t, conformanceProvider(t), conformanceOptions())
}

func TestConformancePause(t *testing.T) {
	providertest.RunPause(t, conformanceProvider(t), conformanceOptions(), providertest.SameProcessOrRestart)
}

func TestConformanceWarm(t *testing.T) {
	providertest.RunWarm(t, conformanceProvider(t), conformanceOptions())
}

func TestConformanceHold(t *testing.T) {
	if os.Getenv("FLY_CONFORMANCE_HOLD") == "" {
		t.Skip("FLY_CONFORMANCE_HOLD is not set")
	}
	providertest.RunHold(t, conformanceProvider(t), conformanceOptions(), 30*time.Minute)
}
