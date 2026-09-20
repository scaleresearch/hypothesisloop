package vast

import (
	"context"
	"fmt"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const (
	bytesPerMB = 1 << 20
	bytesPerGB = 1 << 30

	// offerCandidates is how many of the cheapest offers one Create may try. The search index
	// trails the market: it kept returning an offer for a while after that offer was rented.
	offerCandidates = 5
)

type offer struct {
	ID           int64   `json:"id"`
	PricePerHour float64 `json:"dph_total"`
}

// offerQuery builds the search for the cheapest on-demand offers that fit the spec. The vendor
// rents no machine without a GPU (a search for zero GPUs answers with storage offers that cannot
// be rented as instances), so a CPU-only spec takes the cheapest offer of any GPU model.
func (p *Provider) offerQuery(spec provider.Spec) (map[string]any, error) {
	query := map[string]any{
		"type":                "ondemand",
		"rentable":            map[string]any{"eq": true},
		"rented":              map[string]any{"eq": false},
		"cpu_cores_effective": map[string]any{"gte": ceilDiv(spec.CPUMillis, 1000)},
		"cpu_ram":             map[string]any{"gte": ceilDiv(spec.MemoryBytes, bytesPerMB)},
		"disk_space":          map[string]any{"gte": diskGB(spec)},
		// The quoted price covers the disk actually allocated.
		"allocated_storage": diskGB(spec),
		"dph_total":         map[string]any{"lte": p.cfg.MaxPricePerHour},
		"reliability2":      map[string]any{"gte": p.cfg.MinReliability},
		"order":             [][]string{{"dph_total", "asc"}},
		"limit":             offerCandidates,
	}
	if spec.AcceleratorCount == 0 {
		query["num_gpus"] = map[string]any{"gte": 1}
	} else {
		accelerator, err := p.cfg.accelerator(spec.AcceleratorType)
		if err != nil {
			return nil, err
		}
		query["num_gpus"] = map[string]any{"eq": spec.AcceleratorCount}
		query["gpu_name"] = map[string]any{"eq": accelerator.GPUName}
		if accelerator.MinGPURAMMB > 0 {
			query["gpu_ram"] = map[string]any{"gte": accelerator.MinGPURAMMB}
		}
	}
	if len(p.cfg.Regions) > 0 {
		query["geolocation"] = map[string]any{"in": p.cfg.Regions}
	}
	if p.cfg.VerifiedOnly {
		query["verified"] = map[string]any{"eq": true}
	}
	return query, nil
}

// cheapestOffers returns the offers that fit the spec, cheapest first.
func (p *Provider) cheapestOffers(ctx context.Context, spec provider.Spec) ([]offer, error) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return nil, err
	}
	var found struct {
		Offers []offer `json:"offers"`
	}
	if _, err := p.call(ctx, http.MethodPost, "/api/v0/bundles/", query, &found); err != nil {
		return nil, err
	}
	if len(found.Offers) == 0 {
		return nil, fmt.Errorf("vast: no offer matches %q within the configured filters", spec.Name)
	}
	return found.Offers, nil
}

func diskGB(spec provider.Spec) int64 {
	return ceilDiv(spec.DiskBytes, bytesPerGB)
}

func ceilDiv(value, unit int64) int64 {
	return (value + unit - 1) / unit
}
