package vast

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/providertest"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/acceleratorname"
)

// The conformance tests rent real instances and cost real money, a few cents a run. They run
// when VAST_API_KEY is set:
//
//	VAST_API_KEY=... go test ./runtime/cloud/internal/provider/vast -run Conformance -v -timeout 45m
//
// VAST_CONFORMANCE_GPU="<driver product name>=<vendor gpu name>", for example
// "NVIDIA GeForce RTX 3060=RTX 3060", adds the run on that GPU, and VAST_CONFORMANCE_HOLD=1 the
// thirty minute hold. One run per account at a time: a run starts by destroying what an aborted
// earlier run left under the suite's prefix.
func conformanceOptions() providertest.Options {
	return providertest.Options{
		Template: provider.Spec{
			Image:       "alpine/curl:8.14.1",
			CPUMillis:   1000,
			MemoryBytes: 1 << 30,
			DiskBytes:   8 << 30,
		},
		OutboundURL:   "https://example.com/",
		StartTimeout:  10 * time.Minute,
		SettleTimeout: 10 * time.Minute,
	}
}

func conformanceProvider(t *testing.T, accelerators []Accelerator) *Provider {
	t.Helper()
	if os.Getenv("VAST_API_KEY") == "" {
		t.Skip("VAST_API_KEY is not set")
	}
	p, err := New(Settings{
		APIKeyEnv:       "VAST_API_KEY",
		MaxPricePerHour: 0.15,
		MinReliability:  0.99,
		VerifiedOnly:    true,
		Accelerators:    accelerators,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestConformanceCPUOnly(t *testing.T) {
	providertest.Run(t, conformanceProvider(t, nil), conformanceOptions())
}

func TestConformanceGPU(t *testing.T) {
	product, gpuName, set := strings.Cut(os.Getenv("VAST_CONFORMANCE_GPU"), "=")
	if !set {
		t.Skip("VAST_CONFORMANCE_GPU is not set")
	}
	opts := conformanceOptions()
	opts.Template.AcceleratorType = acceleratorname.NVIDIAProductLabel(product)
	opts.Template.AcceleratorCount = 1
	providertest.Run(t, conformanceProvider(t, []Accelerator{{Product: product, GPUName: gpuName}}), opts)
}

func TestConformanceHold(t *testing.T) {
	if os.Getenv("VAST_CONFORMANCE_HOLD") == "" {
		t.Skip("VAST_CONFORMANCE_HOLD is not set")
	}
	providertest.RunHold(t, conformanceProvider(t, nil), conformanceOptions(), 30*time.Minute)
}
