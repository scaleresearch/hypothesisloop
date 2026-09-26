package vast

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const (
	bytesPerMB = 1 << 20
	bytesPerGB = 1 << 30

	// offerSearchLimit is how many of the cheapest offers are asked for, enough to find
	// offerCandidates different machines among them.
	offerSearchLimit = 25
	// offerCandidates is how many machines one Create chooses among.
	offerCandidates = 5
)

type offer struct {
	ID           int64   `json:"id"`
	MachineID    int64   `json:"machine_id"`
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
		"cpu_cores_effective": map[string]any{"gte": provider.CeilDiv(spec.CPUMillis, 1000)},
		"cpu_ram":             map[string]any{"gte": provider.CeilDiv(spec.MemoryBytes, bytesPerMB)},
		"disk_space":          map[string]any{"gte": diskGB(spec)},
		// The quoted price covers the disk actually allocated.
		"allocated_storage": diskGB(spec),
		"dph_total":         map[string]any{"lte": p.cfg.MaxPricePerHour},
		"reliability2":      map[string]any{"gte": p.cfg.MinReliability},
		"order":             [][]string{{"dph_total", "asc"}},
		"limit":             offerSearchLimit,
	}
	if len(p.cfg.ExcludedMachines) > 0 {
		query["machine_id"] = map[string]any{"notin": p.cfg.ExcludedMachines}
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

// candidates returns the offers one Create tries, in the order it tries them: the cheapest offer
// of each of the offerCandidates cheapest machines that fit the spec, shuffled.
//
// Always taking the cheapest offer sent every retry of a job to the same machine, and a machine
// that cannot start a container stays the cheapest because nobody keeps it. Nothing the vendor
// shows before renting tells such a machine apart (the one met was verified, with a reliability
// of 0.998), and the adapter remembers nothing between calls, so the choice is spread over a few
// machines. A retry is then unlikely to meet the same machine again; it still can.
func (p *Provider) candidates(ctx context.Context, spec provider.Spec) ([]offer, error) {
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
		return nil, fmt.Errorf("vast: %w %q within the configured filters", provider.ErrNoOffer, spec.Name)
	}
	chosen := cheapestPerMachine(found.Offers, offerCandidates)
	p.shuffle(chosen)
	return chosen, nil
}

// cheapestPerMachine keeps the first offer of each machine, up to limit machines. offers come
// cheapest first.
func cheapestPerMachine(offers []offer, limit int) []offer {
	seen := map[int64]bool{}
	var kept []offer
	for _, candidate := range offers {
		if seen[candidate.MachineID] {
			continue
		}
		seen[candidate.MachineID] = true
		kept = append(kept, candidate)
		if len(kept) == limit {
			break
		}
	}
	return kept
}

func shuffleOffers(offers []offer) {
	rand.Shuffle(len(offers), func(i, j int) { offers[i], offers[j] = offers[j], offers[i] })
}

func diskGB(spec provider.Spec) int64 {
	return provider.CeilDiv(spec.DiskBytes, bytesPerGB)
}
