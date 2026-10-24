package runpod

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/acceleratorname"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/providertest"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

// The conformance tests rent real pods and cost real money, a few cents a run. They run on the
// account of the cluster local development rents Runpod on, when its account key
// (conformanceKey) is set:
//
//	CLOUD_COMPUTE_HYPOTHESISLOOP_RUNPOD_API_KEY=... go test ./controlplane/clients/provider/runpod -run Conformance -v -timeout 30m
//
// RUNPOD_CONFORMANCE_GPU="<driver product name>=<vendor gpu id>", for example
// "NVIDIA RTX A4500=NVIDIA RTX A4500", adds the checks of the global network on two pods of that
// GPU, and RUNPOD_CONFORMANCE_GLOO=1 with it a gloo all_reduce of torch.distributed between them.
// One run per account at a time: a run starts by destroying what an aborted earlier run left
// under the suite's prefix.
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
		StartTimeout:  10 * time.Minute,
		SettleTimeout: 5 * time.Minute,
	}
}

// conformanceKey is the account key of hypothesisloop-runpod, the cluster localdev/dev.sh names.
var conformanceKey = config.AccountKey(config.SectionCloudCompute, "hypothesisloop-runpod")

func conformanceProvider(t *testing.T, accelerators []Accelerator) *Provider {
	t.Helper()
	if os.Getenv(conformanceKey.Name) == "" {
		t.Skip(conformanceKey.Name + " is not set")
	}
	p, err := New(Settings{
		AccountID:       "conformance",
		MaxPricePerHour: 0.7,
		Cloud:           "SECURE",
		Accelerators:    accelerators,
	}, os.Getenv(conformanceKey.Name), nil, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestConformanceCPUOnly(t *testing.T) {
	providertest.Run(t, conformanceProvider(t, nil), conformanceOptions())
}

func TestConformanceMarket(t *testing.T) {
	providertest.RunMarket(t, conformanceProvider(t, nil), conformanceOptions())
}

// gpuOptions are the options and the provider of a run on RUNPOD_CONFORMANCE_GPU.
func gpuOptions(t *testing.T) (*Provider, providertest.Options) {
	t.Helper()
	product, gpuID, set := strings.Cut(os.Getenv("RUNPOD_CONFORMANCE_GPU"), "=")
	if !set {
		t.Skip("RUNPOD_CONFORMANCE_GPU is not set")
	}
	opts := conformanceOptions()
	opts.Template.AcceleratorType = acceleratorname.NVIDIAProductLabel(product)
	opts.Template.AcceleratorCount = 1
	return conformanceProvider(t, []Accelerator{{Product: product, GPUID: gpuID}}), opts
}

func TestConformanceNetwork(t *testing.T) {
	p, opts := gpuOptions(t)
	providertest.RunNetwork(t, p, opts)
}

// glooScript all_reduces its rank plus one over the world of two, which comes to 3.
const glooScript = `import datetime, os
import torch
import torch.distributed as dist
dist.init_process_group("gloo", init_method="tcp://%s:%s" % (os.environ["MASTER_ADDR"], os.environ["MASTER_PORT"]),
    rank=int(os.environ["RANK"]), world_size=int(os.environ["WORLD_SIZE"]), timeout=datetime.timedelta(minutes=5))
t = torch.tensor([float(os.environ["RANK"]) + 1])
dist.all_reduce(t)
print("allreduce rank %s result %s" % (os.environ["RANK"], t.item()), flush=True)
dist.destroy_process_group()
`

// TestConformanceGloo rents two pods in one place, rank 0 first, and runs a gloo all_reduce of
// torch.distributed between them over the global network, as the cloud cluster's runtime tells
// the ranks of a job to meet.
func TestConformanceGloo(t *testing.T) {
	if os.Getenv("RUNPOD_CONFORMANCE_GLOO") == "" {
		t.Skip("RUNPOD_CONFORMANCE_GLOO is not set")
	}
	p, opts := gpuOptions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	prefix := "hlconf-gloo-" + strconv.FormatInt(time.Now().Unix(), 36) + "-"
	var ids []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, id := range ids {
			if err := p.Destroy(ctx, id); err != nil {
				t.Errorf("Destroy(%s): %v", id, err)
			}
		}
		if left, err := p.List(ctx, "hlconf-gloo-"); err != nil || len(left) > 0 {
			t.Errorf("left after the run: %+v, %v", left, err)
		}
	})
	rank := func(r int, master string) provider.Spec {
		spec := opts.Template
		spec.Image = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"
		spec.DiskBytes = 20 << 30
		spec.Name = prefix + "r" + strconv.Itoa(r)
		spec.Command = []string{"python", "-c", glooScript}
		spec.Env = map[string]string{
			jobenv.TorchRank: strconv.Itoa(r), jobenv.TorchWorldSize: "2", jobenv.TorchMasterAddr: master, jobenv.TorchMasterPort: strconv.Itoa(jobenv.DistributedMasterPort),
			jobenv.GlooSocketInterface: p.Interface(),
		}
		return spec
	}
	place, err := p.Place(ctx, []provider.Spec{rank(0, ""), rank(1, "")})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	t.Logf("place %s", place)
	zero := rank(0, "127.0.0.1")
	zero.Place = place
	id, err := p.Create(ctx, zero)
	if err != nil {
		t.Fatalf("Create rank 0: %v", err)
	}
	ids = append(ids, id)
	first, _, err := p.Get(ctx, id)
	if err != nil || first.PrivateAddress == "" {
		t.Fatalf("rank 0 %s: %+v, %v, want a private address", id, first, err)
	}
	t.Logf("rank 0 is pod %s at %s", id, first.PrivateAddress)
	one := rank(1, first.PrivateAddress)
	one.Place = place
	id, err = p.Create(ctx, one)
	if err != nil {
		t.Fatalf("Create rank 1: %v", err)
	}
	ids = append(ids, id)
	t.Logf("rank 1 is pod %s", id)

	for r, id := range ids {
		for {
			i, exists, err := p.Get(ctx, id)
			if err != nil || !exists {
				t.Fatalf("rank %d pod %s: exists %v, %v", r, id, exists, err)
			}
			if i.State == provider.StateExited {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("rank %d pod %s is %s when the run ends", r, id, i.State)
			}
			time.Sleep(5 * time.Second)
		}
		lines, err := p.Logs(ctx, id, 50)
		if err != nil {
			t.Fatalf("Logs of rank %d: %v", r, err)
		}
		t.Logf("rank %d output: %q", r, lines)
		if want := "allreduce rank " + strconv.Itoa(r) + " result 3.0"; !slices.Contains(lines, want) {
			t.Errorf("rank %d did not print %q", r, want)
		}
	}
}
