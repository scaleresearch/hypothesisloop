package vast

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"

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

// held is the one place an offer is handed out from: the offers this process is renting right
// now and the ones its own instances hold. The vendor's search goes on listing an offer whose GPU
// is rented, ours included, for as long as the instance lives, so without it every job of a pass
// is sent to the same few offers and all but one are refused.
//
// A process that starts without it asks for an offer one of its instances holds, is told the
// offer is taken and passes over it. Until those instances are gone Rentable counts their offers
// as rentable, since nothing the adapter reads says which offer an instance was rented from.
type held struct {
	mu     sync.Mutex
	offers map[int64]holder
}

// holder is who has an offer: the instance rented from it, or none yet while the rent is under
// way. listed says a read of the account has shown that instance.
type holder struct {
	instance string
	listed   bool
}

// claim takes id for one rental. It is false when the offer is already claimed or held.
func (h *held) claim(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.offers[id]; taken {
		return false
	}
	h.offers[id] = holder{}
	return true
}

// settle ends the claim on id: the offer is held by instance, or free again when the rent
// failed and instance is empty.
func (h *held) settle(id int64, instance string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if instance == "" {
		delete(h.offers, id)
		return
	}
	h.offers[id] = holder{instance: instance}
}

// release frees the offer instance was rented from, once the instance is destroyed.
func (h *held) release(instance string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, holder := range h.offers {
		if holder.instance == instance {
			delete(h.offers, id)
		}
	}
}

// keep is told the account's instances after each read of them, and frees the offer of one that
// was listed before and is gone: an instance the vendor removed frees its offer without a
// Destroy. One not listed yet keeps its offer, since the list trails a rent.
func (h *held) keep(onAccount map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, holder := range h.offers {
		switch {
		case holder.instance == "":
		case onAccount[holder.instance]:
			holder.listed = true
			h.offers[id] = holder
		case holder.listed:
			delete(h.offers, id)
		}
	}
}

func (h *held) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.offers)
}

// free returns the offers nobody in this process has.
func (h *held) free(offers []offer) []offer {
	h.mu.Lock()
	defer h.mu.Unlock()
	var open []offer
	for _, candidate := range offers {
		if _, taken := h.offers[candidate.ID]; !taken {
			open = append(open, candidate)
		}
	}
	return open
}

// candidates returns the offers one Create tries, in the order it tries them: the cheapest offer
// of each of the offerCandidates cheapest machines that fit the spec and that this process does
// not already hold, shuffled.
//
// Always taking the cheapest offer sent every retry of a job to the same machine, and a machine
// that cannot start a container stays the cheapest because nobody keeps it. Nothing the vendor
// shows before renting tells such a machine apart (the one met was verified, with a reliability
// of 0.998), and the adapter remembers no machine's failures, so the choice is spread over a few
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
	open := p.held.free(found.Offers)
	if len(open) == 0 {
		return nil, fmt.Errorf("vast: %w %q: this agent's own instances hold all %d offers within the configured filters", provider.ErrNoOffer, spec.Name, len(found.Offers))
	}
	chosen := cheapestPerMachine(open, offerCandidates)
	p.shuffle(chosen)
	return chosen, nil
}

var _ provider.Market = (*Provider)(nil)

// Rentable counts the offers candidates chooses among: the same search, asked for as many offers
// as are wanted on top of the ones this process holds, which the search goes on listing.
func (p *Provider) Rentable(ctx context.Context, spec provider.Spec, atMost int) (int, error) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return 0, err
	}
	query["limit"] = atMost + p.held.count()
	var found struct {
		Offers []offer `json:"offers"`
	}
	if _, err := p.call(ctx, http.MethodPost, "/api/v0/bundles/", query, &found); err != nil {
		return 0, err
	}
	return min(atMost, len(p.held.free(found.Offers))), nil
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
