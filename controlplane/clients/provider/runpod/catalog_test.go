package runpod

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

const a4500 = "nvidia.com/gpu.product=NVIDIA-RTX-A4500"

const cpusJSON = `{"cpus":[
 {"id":"cpu5c","name":"Compute-Optimized","vcpu":{"min":2,"max":32},"ramGbPerVcpu":2,"price":{"securePerVcpu":0.035,"serverlessPerVcpu":0},"availability":"HIGH","dataCenters":[{"id":"EU-RO-1","availability":"HIGH"}]},
 {"id":"cpu3c","name":"Compute-Optimized","vcpu":{"min":2,"max":32},"ramGbPerVcpu":2,"price":{"securePerVcpu":0.03,"serverlessPerVcpu":0},"availability":"HIGH","dataCenters":[{"id":"US-CA-2","availability":"LOW"},{"id":"EU-SE-1","availability":"HIGH"}]},
 {"id":"cpu3m","name":"Memory-Optimized","vcpu":{"min":2,"max":32},"ramGbPerVcpu":8,"price":{"securePerVcpu":0.055,"serverlessPerVcpu":0},"availability":"NONE"}
]}`

func gpuJSON(dataCenters string) string {
	return `{"id":"NVIDIA RTX A4500","name":"RTX A4500","memory":20,"secure":true,"community":true,"price":{"secure":0.25,"community":0.19},"maxCount":{"secure":4,"community":2},"availability":"LOW","dataCenters":[` + dataCenters + `]}`
}

const globalJSON = `{"dataCenters":[{"id":"EU-RO-1","globalNetwork":true},{"id":"EU-SE-1","globalNetwork":true}]}`

func cpuSpec(cores, memoryGiB int64) provider.Spec {
	return provider.Spec{Name: "hl-job-a", Image: "alpine/curl:8.14.1", Architecture: "amd64", Command: []string{"sh", "-c", "true"},
		CPUMillis: cores * 1000, MemoryBytes: memoryGiB * gib, DiskBytes: 8 * gib}
}

func gpuSpec(name string) provider.Spec {
	spec := cpuSpec(8, 24)
	spec.Name, spec.AcceleratorType, spec.AcceleratorCount = name, a4500, 1
	return spec
}

func TestRentableCountsWhatHasStockAndSaysWhatLeavesTheRestOut(t *testing.T) {
	api := newFakeAPI(t)
	cfg := testSettings()
	p := newTestProvider(t, api, cfg)
	ctx := context.Background()
	api.on(http.MethodGet, "/v2/catalog/cpus", reply(200, cpusJSON))
	api.on(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500", reply(200, gpuJSON(`{"id":"EU-RO-1","availability":"LOW"}`)))

	if supply, err := p.Rentable(ctx, cpuSpec(4, 8), 3); err != nil || supply.Count != 3 {
		t.Fatalf("a CPU pod with stock: %+v, %v", supply, err)
	}
	if supply, _ := p.Rentable(ctx, cpuSpec(1<<30, 8), 3); supply.Count != 0 || !strings.Contains(supply.Why, "holds the shape's cores") {
		t.Errorf("a size no flavor holds: %+v", supply)
	}
	if supply, _ := p.Rentable(ctx, cpuSpec(32, 8), 3); supply.Count != 0 || !strings.Contains(supply.Why, "max_price_per_hour") {
		t.Errorf("a size above the ceiling: %+v", supply)
	}
	if supply, err := p.Rentable(ctx, gpuSpec("g"), 2); err != nil || supply.Count != 2 {
		t.Fatalf("a GPU with stock: %+v, %v", supply, err)
	}
	q := api.sent(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500")[0].Query
	for _, part := range []string{"include=AVAILABILITY", "product=POD", "count=1", "cloud=SECURE"} {
		if !strings.Contains(q, part) {
			t.Errorf("GPU catalog query %q lacks %s", q, part)
		}
	}
	many := gpuSpec("g")
	many.AcceleratorCount = 8
	if supply, _ := p.Rentable(ctx, many, 2); supply.Count != 0 || !strings.Contains(supply.Why, "no machine with that many") {
		t.Errorf("more GPUs than a machine has: %+v", supply)
	}

	cfg.DataCenters = []string{"US-TX-3"}
	p = newTestProvider(t, api, cfg)
	if supply, _ := p.Rentable(ctx, gpuSpec("g"), 2); supply.Count != 0 || !strings.Contains(supply.Why, "no stock") {
		t.Errorf("stock only outside the data centers allowed: %+v", supply)
	}
}

func TestPlaceIsTheBestStockedDataCenterOnTheGlobalNetworkThatHasEverySpec(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	ctx := context.Background()
	api.on(http.MethodGet, "/v2/catalog/datacenters", reply(200, globalJSON))
	api.on(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500", reply(200, gpuJSON(`{"id":"US-KS-9","availability":"HIGH"},{"id":"EU-SE-1","availability":"LOW"},{"id":"EU-RO-1","availability":"MEDIUM"}`)))

	place, err := p.Place(ctx, []provider.Spec{gpuSpec("a"), gpuSpec("b")})
	if err != nil || place != "EU-RO-1" {
		t.Fatalf("Place = %q, %v, want the best stocked data center on the global network", place, err)
	}
	if q := api.sent(http.MethodGet, "/v2/catalog/datacenters")[0].Query; q != "globalNetwork=true" {
		t.Errorf("data center query %q", q)
	}
	if asked := len(api.sent(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500")); asked != 1 {
		t.Errorf("two pods of one size were looked up %d times, want once", asked)
	}
	if supply, err := p.Networked(ctx, gpuSpec("a"), 2); err != nil || supply.Count != 2 {
		t.Fatalf("Networked = %+v, %v", supply, err)
	}
	if supply, err := p.Networked(ctx, cpuSpec(2, 4), 2); err != nil || supply.Count != 0 || !strings.Contains(supply.Why, "GPU pods only") {
		t.Errorf("Networked = %+v, %v, want none for a CPU pod", supply, err)
	}
	if _, err := p.Place(ctx, []provider.Spec{gpuSpec("a"), cpuSpec(2, 4)}); !errors.Is(err, provider.ErrNoOffer) {
		t.Errorf("a place with a CPU pod: %v, want ErrNoOffer", err)
	}

	api.on(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500", reply(200, gpuJSON(`{"id":"US-KS-9","availability":"HIGH"}`)))
	if _, err := p.Place(ctx, []provider.Spec{gpuSpec("a")}); !errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "global network") {
		t.Errorf("stock off the global network alone: %v, want ErrNoOffer", err)
	}
	if supply, _ := p.Networked(ctx, gpuSpec("a"), 2); supply.Count != 0 || supply.Why == "" {
		t.Errorf("Networked = %+v, want none", supply)
	}
}
